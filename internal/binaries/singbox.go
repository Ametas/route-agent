package binaries

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

func (s *Service) singboxPath() string { return or(s.cfg.SingboxBinaryPath, defaultSingboxPath) }

// SingboxUnit is the sing-box.service content (src/utils/singbox.ts:94-107, spec C §1 step 10),
// byte-exact. It must NOT carry CapabilityBoundingSet/AmbientCapabilities: the bounding set
// stripped CAP_KILL from ExecReload and broke reload on a node whose process belonged to another
// user (mo-nl-node, 2026-09-04).
//
// TODO(group B landing): group B's singbox package also renders this unit; keep ONE copy (spec B
// §5.2 and C §1 describe the same function) and call it from here.
func SingboxUnit(bin, cfg string) string {
	return "[Unit]\n" +
		"Description=sing-box service\n" +
		"After=network.target nss-lookup.target\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + bin + " run -c " + cfg + "\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"ExecReload=/bin/sh -c \"" + bin + " check -c " + cfg + " && /bin/kill -HUP $MAINPID\"\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

// EnsureSingboxUnit is ensureSingboxSystemdUnit: write only on a byte difference, then
// daemon-reload; reports whether it wrote. Errors are logged, never returned.
func (s *Service) EnsureSingboxUnit(ctx context.Context) bool {
	return systemd.EnsureUnit(ctx, s.fs, s.systemctl, s.log, "sing-box",
		or(s.cfg.SingboxUnitFilePath, "/etc/systemd/system/sing-box.service"),
		SingboxUnit(s.singboxPath(), or(s.cfg.SingboxConfigPath, "/etc/sing-box/config.json")))
}

// UploadSingboxBinary implements the RPC (spec C §1).
func (s *Service) UploadSingboxBinary(stream UploadStream) error {
	target := s.singboxPath()
	return s.receive(stream, receiveSpec{
		rpc: "UploadSingboxBinary", dir: s.binaryTempDir(target), prefix: tempPrefix(target), suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: auth.InvalidSecretMessage, empty: msgNoData,
		failPrefix: prefixUploadFailed, lockKey: "bin:" + target,
	}, func(ctx context.Context, r received) (Result, error) { return s.applySingbox(ctx, r, target) })
}

func (s *Service) applySingbox(ctx context.Context, r received, target string) (Result, error) {
	if err := checkELF(r.path, s.Arch); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(r.path, 0o755); err != nil {
		return Result{}, err
	}
	if _, err := s.run(ctx, smokeTimeout, r.path, "version"); err != nil {
		return Result{}, err
	}
	s.setcap(ctx, r.path, "Failed to setcap on new sing-box binary")

	// Checked BEFORE the swap, on the candidate: a rejection leaves the working binary in place
	// and the service untouched (binary.service.ts:168-169).
	if cfgPath, errText, rejected := s.verifySingboxAcceptsLiveConfigs(ctx, r.path); rejected {
		s.log.Error("Rejected uploaded sing-box binary: it does not accept a config already applied on this node",
			"version", r.version, "configPath", cfgPath, "error", errText)
		return fail("Бинарь " + r.version + " отвергнут: он не принимает уже применённый на узле конфиг " +
			cfgPath + ". Прежний бинарь оставлен на месте, сервис не тронут.\n" + errText), nil
	}

	wasActive := s.unitActive(ctx, "sing-box")
	hadPrev, err := s.install(r.path, target)
	if err != nil {
		return Result{}, err
	}
	s.setcap(ctx, s.fs.Path(target), "Failed to setcap on target sing-box binary")
	s.log.Info("Atomically updated sing-box binary", "path", target, "version", r.version, "prev", hadPrev)
	call(s.telemetry.SingboxSwapped)

	// Restart unconditionally, even when the unit file changed: only restart execs the new file
	// (incident 2026-09-20; never RELOAD_COMMAND, never enable --now — singboxBinarySwap.test.ts).
	if s.EnsureSingboxUnit(ctx) {
		if err := s.systemctl.Enable(ctx, "sing-box"); err != nil {
			s.log.Warn("Failed to enable sing-box unit after provisioning", "err", err.Error())
		}
	}
	out := s.activate(ctx, activation{
		target: target, label: "sing-box binary " + r.version, what: "sing-box.service",
		wasActive: wasActive, hadPrev: hadPrev,
		restart: s.restartSingbox,
		state:   func(ctx context.Context) string { st, _ := s.systemctl.IsActive(ctx, "sing-box"); return st },
		afterRollback: func(ctx context.Context) {
			s.setcap(ctx, s.fs.Path(target), "Failed to setcap on restored sing-box binary")
			call(s.telemetry.SingboxSwapped)
		},
	})
	if out.rolledBack || out.message != "" {
		return fail(prefixUploadFailed + out.message), nil
	}
	// As in TS, a restart failure with nothing to roll back to is logged only (restartSingbox).
	return ok("sing-box binary version " + r.version + " successfully updated and restarted"), nil
}

// restartSingbox runs SINGBOX_RESTART_COMMAND (a configurable shell string, spec A §6).
func (s *Service) restartSingbox(ctx context.Context) error {
	cmd := or(s.cfg.SingboxRestartCommand, defaultSingboxRestartCommand)
	res, err := s.runner.Run(ctx, runner.Cmd{Name: cmd, Shell: true, Timeout: restartCommandTimeout})
	if res.Stdout != "" {
		s.log.Info("Restart after binary upgrade", "stdout", res.Stdout)
	}
	if res.Stderr != "" {
		s.log.Warn("Restart stderr", "stderr", res.Stderr)
	}
	if err != nil {
		s.log.Warn("Restart command failed after binary upgrade", "err", err.Error())
	}
	return err
}

func (s *Service) unitActive(ctx context.Context, unit string) bool {
	st, err := s.systemctl.IsActive(ctx, unit)
	return err == nil && st == "active"
}

// verifySingboxAcceptsLiveConfigs is verifyBinaryAcceptsLiveConfigs (binary.service.ts:40-54):
// the front config, then the rear config; a missing file is skipped; the first rejection wins.
// The mihomo rear.yaml is deliberately not checked here.
func (s *Service) verifySingboxAcceptsLiveConfigs(ctx context.Context, candidate string) (cfgPath, errText string, rejected bool) {
	for _, p := range []string{s.cfg.SingboxConfigPath, s.cfg.RearSingboxConfigPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(s.fs.Path(p)); err != nil {
			continue
		}
		if _, err := s.run(ctx, checkTimeout, candidate, "check", "-c", s.fs.Path(p)); err != nil {
			return p, truncate(or(runner.StderrOrMessage(err), "unknown error"), 1000), true
		}
	}
	return "", "", false
}

// Messages of UpgradeSingbox (spec C §2, §11).
const (
	msgUpgradeMissingURL  = "Missing download_url in request payload."
	msgUpgradeBadURL      = "Invalid download_url format."
	msgUpgradeBadProtocol = "Invalid URL protocol: must be http or https."
	// MsgUpgradeUnsupported is the honest failure that replaces the TS download path, which could
	// never succeed (curl got `--` before `-o`, spec C §2 and risk 1) and which the orchestrator
	// no longer calls. See docs/go-rewrite/deviations.md.
	MsgUpgradeUnsupported = "Failed to upgrade sing-box binary: UpgradeSingbox is not supported by this agent " +
		"(it never worked: the download step was broken and the URL is a tarball); deliver the binary with UploadSingboxBinary instead."
)

// UpgradeSingbox implements the unary RPC: same auth and input validation answers as TS, then an
// explicit failure instead of the always-failing download (owner decision, architecture §6 item 6).
func (s *Service) UpgradeSingbox(ctx context.Context, req *agentpb.UpgradePayload) (*agentpb.UpgradeResponse, error) {
	if !s.auth.Metadata(ctx) {
		s.log.Warn("Unauthorized UpgradeSingbox request blocked")
		return &agentpb.UpgradeResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	if msg := validateUpgradeURL(req.GetDownloadUrl()); msg != "" {
		return &agentpb.UpgradeResponse{Success: false, Message: msg}, nil
	}
	s.log.Warn("UpgradeSingbox called; it is not supported (use UploadSingboxBinary)", "version", or(req.GetVersion(), "latest"))
	return &agentpb.UpgradeResponse{Success: false, Message: MsgUpgradeUnsupported}, nil
}

// validateUpgradeURL mirrors the TS checks: empty → missing; `new URL()` would throw (no scheme,
// or an http(s) URL without a host) → bad format; scheme other than http/https → bad protocol.
func validateUpgradeURL(raw string) string {
	if raw == "" {
		return msgUpgradeMissingURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return msgUpgradeBadURL
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return msgUpgradeBadURL
		}
		return ""
	}
	return msgUpgradeBadProtocol
}
