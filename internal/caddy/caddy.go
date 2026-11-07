package caddy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/certs"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Response texts. The orchestrator's fixtures match the two failure prefixes byte for byte and
// greps the validate output for "proxy_protocol" (spec B §4.3).
const (
	MsgAlreadyCurrent  = "Caddy configuration already current — nothing to write or reload."
	MsgApplied         = "Caddyfile успешно валидирован и применен."
	MsgValidatePrefix  = "Ошибка синтаксиса Caddyfile:\n"
	MsgReloadPrefix    = "Сбой службы Caddy при reload:\n"
	validateFallbackRu = "Ошибка синтаксиса Caddyfile"
)

// Service implements ConfigureCaddy.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
	// CertBase overrides certs.CaddyCertBase (tests); empty means the real store.
	CertBase string
	// CamouflageRoots overrides DefaultCamouflageRoots (tests); nil means the default.
	CamouflageRoots []string

	// mu serializes Caddy applies (spec B §4.6, R6): two calls no longer share Caddyfile.bak or
	// interleave writes and reloads; the second waits and then usually hits "already current".
	mu sync.Mutex
}

// ConfigureCaddy: auth M|B (CaddyConfigPayload has no body secret, so metadata only in
// practice); status OK on every path, failures as success:false (spec B §0.3).
func (s *Service) ConfigureCaddy(ctx context.Context, req *agentpb.CaddyConfigPayload) (*agentpb.CaddyConfigResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, "") {
		s.Log.Warn("Unauthorized ConfigureCaddy request blocked")
		return &agentpb.CaddyConfigResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The orchestrator's deadline must not kill a reload half way: TS kept working after the
	// client gave up, and every command here has its own timeout.
	msg, ok := s.apply(context.WithoutCancel(ctx), req)
	return &agentpb.CaddyConfigResponse{Success: ok, Message: msg}, nil
}

// Plan builds the list of files ConfigureCaddy writes, in the order it writes them (spec B §4.2
// step 3): camouflage HTML, Cloudflare env (0600), single relayed cert pair, every complete bulk
// pair (keys 0600), and the Caddyfile LAST — Caddy fails the whole config at provision time if a
// referenced cert file is missing. It returns the Caddyfile path too.
func (s *Service) Plan(req *agentpb.CaddyConfigPayload) ([]fsx.PlannedWrite, string, error) {
	camo := sanitizeConfigInput(orDefault(req.GetCamouflagePath(), DefaultCamouflagePath))
	camo = orDefault(camo, DefaultCamouflagePath)
	roots := s.CamouflageRoots
	if roots == nil {
		roots = DefaultCamouflageRoots
	}
	camoPath, err := ValidateSafeCamouflagePath(camo, roots)
	if err != nil {
		return nil, "", err
	}

	var plan []fsx.PlannedWrite
	if html := req.GetCamouflageHtml(); jsTrim(html) != "" {
		plan = append(plan, fsx.PlannedWrite{Path: path.Join(camoPath, "index.html"), Content: []byte(html)})
	}
	// Xeon fronts only; never inlined into the Caddyfile. Not sanitized, as in TS (spec B R12).
	if tok := req.GetCloudflareApiToken(); jsTrim(tok) != "" {
		plan = append(plan, fsx.PlannedWrite{Path: CloudflareEnvPath, Content: []byte("CF_API_TOKEN=" + tok + "\n"), Mode: 0o600})
	}
	// Egress only: the wildcard relayed from a front. The domain is used untrimmed, as in TS.
	if c, k, d := req.GetExtraCertPem(), req.GetExtraKeyPem(), req.GetExtraCertDomain(); jsTrim(c) != "" && jsTrim(k) != "" && jsTrim(d) != "" {
		certPath, keyPath := certs.Paths(d, s.CertBase)
		plan = append(plan,
			fsx.PlannedWrite{Path: certPath, Content: []byte(c)},
			fsx.PlannedWrite{Path: keyPath, Content: []byte(k), Mode: 0o600})
	}
	for _, e := range req.GetExtraCertificates() {
		domain := jsTrim(e.GetDomain())
		// Half a pair on disk is worse than none: incomplete entries are skipped silently.
		if domain == "" || jsTrim(e.GetCertPem()) == "" || jsTrim(e.GetKeyPem()) == "" {
			continue
		}
		certPath, keyPath := certs.Paths(domain, s.CertBase)
		plan = append(plan,
			fsx.PlannedWrite{Path: certPath, Content: []byte(e.GetCertPem())},
			fsx.PlannedWrite{Path: keyPath, Content: []byte(e.GetKeyPem()), Mode: 0o600})
	}
	caddyfile := orDefault(s.Cfg.CaddyfilePath, DefaultCaddyfilePath)
	// An empty Caddyfile is accepted and written, as in TS (spec B R11).
	plan = append(plan, fsx.PlannedWrite{Path: caddyfile, Content: []byte(req.GetCaddyfileContent())})
	return plan, caddyfile, nil
}

func (s *Service) apply(ctx context.Context, req *agentpb.CaddyConfigPayload) (string, bool) {
	plan, caddyfile, err := s.Plan(req)
	if err != nil {
		s.Log.Error("Failed to configure Caddy", "err", err.Error())
		return err.Error(), false
	}

	// Nothing changed and Caddy is up: nothing to do. A correct config with a dead service must
	// still be reloaded, or nobody would ever bring it back.
	if s.FS.AllWritesAlreadyOnDisk(plan) && s.caddyActive(ctx) {
		s.Log.Info("Caddy config unchanged and service is up — skipping write, validate and reload")
		return MsgAlreadyCurrent, true
	}

	if err := s.FS.MkdirAll(path.Dir(caddyfile)); err != nil {
		return writeErrText(path.Dir(caddyfile), err), false
	}
	backup := caddyfile + ".bak"
	_, statErr := os.Stat(s.FS.Path(caddyfile))
	hadCaddyfile := statErr == nil
	if hadCaddyfile {
		_ = copyFile(s.FS, caddyfile, backup)
	}
	for _, w := range plan {
		if err := s.FS.WriteFile(w.Path, w.Content, fsx.WriteOptions{Mode: w.Mode}); err != nil {
			s.Log.Error("Failed to configure Caddy", "err", err.Error())
			return writeErrText(w.Path, err), false
		}
	}

	FixPermissions(ctx, s.Runner, s.FS, s.Log, LibDir)
	FixXraySocketPermissions(ctx, s.Runner, s.FS, s.Log, XraySocketPaths)

	restore := func() {
		if hadCaddyfile {
			_ = copyFile(s.FS, backup, caddyfile)
		}
	}

	// 1. Validate with the binary that will run it, with the DNS-provider env merged in:
	// EnvironmentFile= reaches only processes systemd starts, not this ad-hoc child.
	bin := ResolveBinary(s.FS, s.Cfg)
	_, err = s.Runner.Run(ctx, runner.Cmd{
		Name:    bin,
		Args:    []string{"validate", "--config", caddyfile},
		Env:     mergedEnv(LoadDNSProviderEnv(s.FS, CloudflareEnvPath)),
		Timeout: validateTimeout,
	})
	if err != nil {
		restore()
		valMsg := validateFailureText(bin, caddyfile, err)
		s.Log.Error("Caddyfile validation failed", "err", valMsg)
		// The raw validate output is load-bearing: the orchestrator greps it for proxy_protocol.
		return MsgValidatePrefix + valMsg, false
	}

	// 2. Reload (default `systemctl reload caddy || systemctl restart caddy`).
	if _, err := s.Runner.Run(ctx, runner.Cmd{Name: s.Cfg.CaddyReloadCommand, Shell: true, Timeout: reloadTimeout}); err != nil {
		restore()
		journal := ""
		if res, jerr := s.Runner.Run(ctx, runner.Cmd{Name: "journalctl", Args: []string{"-u", "caddy", "-n", "12", "--no-pager"}, Timeout: journalTimeout}); jerr == nil {
			journal = jsTrim(res.Stdout)
		}
		s.Log.Warn("Failed to reload Caddy service", "err", err.Error(), "journalLogs", journal)
		if journal == "" {
			journal = err.Error()
		}
		return MsgReloadPrefix + journal, false
	}
	return MsgApplied, true
}

// validateFailureText is `(err.stderr || err.stdout || err.message).trim()` with err.message in
// the Node exec form of the TS command string (`<bin> validate --config "<path>"`).
func validateFailureText(bin, caddyfile string, err error) string {
	var e *runner.Error
	if !errors.As(err, &e) {
		return jsTrim(err.Error())
	}
	switch {
	case e.Spawn != nil:
		return jsTrim(e.Error())
	case e.Stderr != "":
		return jsTrim(e.Stderr)
	case e.Stdout != "":
		return jsTrim(e.Stdout)
	}
	msg := jsTrim("Command failed: " + bin + ` validate --config "` + caddyfile + `"` + "\n" + e.Stderr)
	if msg == "" {
		return validateFallbackRu
	}
	return msg
}

func (s *Service) caddyActive(ctx context.Context) bool {
	state, err := s.Systemctl.IsActive(ctx, "caddy")
	return err == nil && state == "active"
}

// sanitizeConfigInput strips CR/LF and trims (config.service.ts:27-30).
func sanitizeConfigInput(v string) string {
	return jsTrim(strings.NewReplacer("\r", "", "\n", "").Replace(v))
}

// copyFile copies src to dst (atomically, keeping src's mode), like fs.copyFile.
func copyFile(f fsx.FS, src, dst string) error {
	data, err := f.ReadFile(src)
	if err != nil {
		return err
	}
	var mode fs.FileMode
	if st, err := os.Stat(f.Path(src)); err == nil {
		mode = st.Mode().Perm()
	}
	return f.WriteFile(dst, data, fsx.WriteOptions{Mode: mode})
}

// writeErrText renders a write failure the way Node's fs error message reads when it can
// (spec B §4.2 step 12 returned err.message verbatim).
func writeErrText(p string, err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fsx.NodeError("open", p, pe.Err)
	}
	return err.Error()
}
