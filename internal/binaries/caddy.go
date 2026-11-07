package binaries

import (
	"context"
	"os"

	"github.com/Ametas/route-agent/internal/caddy"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
)

// CaddyOverridePath is the Caddy custom-binary drop-in (src/utils/caddy.ts:9-11). Its
// EnvironmentFile is caddy.CloudflareEnvPath; the binary default is caddy.DefaultCustomBinaryPath
// (shared with caddy.ResolveBinary, so ConfigureCaddy validates with what this upload installs).
const CaddyOverridePath = "/etc/systemd/system/caddy.service.d/override.conf"

func (s *Service) caddyPath() string { return or(s.cfg.CaddyBinaryPath, caddy.DefaultCustomBinaryPath) }

// CaddyOverride is the drop-in that switches the apt caddy.service to the custom binary.
//
// Deviation from TS (docs/go-rewrite/deviations.md): ExecStart has no `--environ`. That flag makes
// Caddy print its whole environment at start — including CF_API_TOKEN loaded from
// cloudflare.env by EnvironmentFile= — into the journal (spec C risk 3). Nothing reads that output.
// ExecReload must be reset and overridden too, or reload keeps invoking the apt binary, which
// cannot parse `dns cloudflare` (caddy.ts:85-90).
func CaddyOverride(bin, caddyfile string) string {
	return "[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=" + bin + " run --config " + caddyfile + "\n" +
		"ExecReload=\n" +
		"ExecReload=" + bin + " reload --config " + caddyfile + " --force\n" +
		"EnvironmentFile=-" + caddy.CloudflareEnvPath + "\n"
}

// UploadCaddyBinary implements the RPC (spec C §5).
func (s *Service) UploadCaddyBinary(stream UploadStream) error {
	target := s.caddyPath()
	return s.receive(stream, receiveSpec{
		rpc: "UploadCaddyBinary", dir: s.binaryTempDir(target), prefix: tempPrefix(target), suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: msgNoValidBinary, empty: msgNoValidBinary,
		failPrefix: prefixUploadFailed, area: locks.Caddy,
	}, func(ctx context.Context, r received) (Result, error) { return s.applyCaddy(ctx, r, target) })
}

func (s *Service) applyCaddy(ctx context.Context, r received, target string) (Result, error) {
	// New in Go (spec C risk 3): the candidate must run and accept the live Caddyfile before it
	// replaces anything. On egress nodes Caddy terminates VLESS; a bad build took subscribers down.
	if err := s.smokeELF(ctx, r.path, "version"); err != nil {
		return Result{}, err
	}
	caddyfile := or(s.cfg.CaddyfilePath, caddy.DefaultCaddyfilePath)
	if errText, rejected := s.verifyCaddyAcceptsLiveConfig(ctx, r.path, caddyfile); rejected {
		s.log.Error("Rejected uploaded Caddy binary: it does not accept the live Caddyfile", "version", r.version, "error", errText)
		return fail("Бинарь Caddy " + r.version + " отвергнут: он не принимает уже применённый на узле конфиг " +
			caddyfile + ". Прежний бинарь оставлен на месте, сервис не тронут.\n" + errText), nil
	}

	wasActive := s.systemctl.Active(ctx, "caddy")
	hadPrev, err := s.install(r.path, target)
	if err != nil {
		return Result{}, err
	}
	s.log.Info("Atomically updated custom Caddy binary", "path", target, "version", r.version, "prev", hadPrev)

	// The drop-in write + daemon-reload happen once; the restart (needed even when the drop-in is
	// unchanged: the running process holds the old binary in memory) is what activate retries.
	if err := s.ensureCaddyOverride(ctx, target, caddyfile); err != nil {
		return Result{}, err
	}
	out := s.activate(ctx, activation{
		target: target, label: "Caddy binary " + r.version, what: "caddy.service",
		wasActive: wasActive, hadPrev: hadPrev,
		restart: func(ctx context.Context) error { return s.systemctl.Restart(ctx, "caddy") },
		state:   func(ctx context.Context) string { st, _ := s.systemctl.IsActive(ctx, "caddy"); return st },
	})
	if out.message != "" {
		return fail(prefixUploadFailed + out.message), nil
	}
	if out.restartErr != nil {
		// No previous binary to roll back to: TS answered the restart error as a failure too.
		return Result{}, out.restartErr
	}
	return ok("Custom Caddy binary version " + r.version + " successfully updated and caddy.service restarted on it"), nil
}

// ensureCaddyOverride writes the drop-in when its bytes differ and runs daemon-reload; errors
// propagate (caddy.ts:78-116). The restart is left to the caller (activate).
func (s *Service) ensureCaddyOverride(ctx context.Context, bin, caddyfile string) error {
	want := []byte(CaddyOverride(bin, caddyfile))
	if s.fs.UpToDate(CaddyOverridePath, want, 0) {
		s.log.Debug("Caddy systemd override is already up to date; skipping write", "path", CaddyOverridePath)
		return nil
	}
	if err := s.fs.WriteFile(CaddyOverridePath, want, fsx.WriteOptions{Mode: 0o644}); err != nil {
		return err
	}
	s.log.Info("Provisioned/updated Caddy systemd override to use custom binary", "path", CaddyOverridePath, "binaryPath", bin)
	return s.systemctl.DaemonReload(ctx)
}

// verifyCaddyAcceptsLiveConfig runs caddy.ValidateCmd — `<candidate> validate --config
// <Caddyfile>` with cloudflare.env merged into the child environment by the same reader
// ConfigureCaddy uses (caddy.LoadDNSProviderEnv, spec B §25). A missing Caddyfile is skipped
// (first install). The output is not logged beyond the error.
func (s *Service) verifyCaddyAcceptsLiveConfig(ctx context.Context, candidate, caddyfile string) (string, bool) {
	real := s.fs.Path(caddyfile)
	if _, err := os.Stat(real); err != nil {
		return "", false
	}
	if _, err := s.runner.Run(ctx, caddy.ValidateCmd(s.fs, candidate, real)); err != nil {
		return truncate(or(runner.StderrOrMessage(err), "unknown error"), 1000), true
	}
	return "", false
}
