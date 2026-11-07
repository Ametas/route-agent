// Package caddy is the Go port of the Caddy side of group B (spec B §4, §5): the ConfigureCaddy
// RPC (caddy.go) and the helpers of src/utils/caddy.ts and fixCaddyPermissions
// (src/utils/singbox.ts:32-45) that other groups also call — binary resolution, the DNS-provider
// env file, the ownership fix-up under /var/lib/caddy (also run by every sing-box validation) and the xray
// socket permissions (also a boot step, spec A §10.2). The caddy.service drop-in for the custom
// binary (ensureCaddyCustomBinaryOverride) belongs to UploadCaddyBinary and lives in
// internal/binaries (group C); that upload reuses ResolveBinary's default path, the env-file
// reader and ValidateCmd from here rather than keeping copies.
//
// Permission model for the certificate store (architecture §6 item 1, deviations.md): TS ran
// `chown -R caddy:caddy /var/lib/caddy` and then `chmod -R 755 /var/lib/caddy`, which made every
// private key world-readable and defeated the "already current" skip (keys are planned at 0600,
// and 0755 never matched). The Go agent keeps the recursive chown — Caddy (user caddy) must own
// what the agent creates there, directories included, or certmagic cannot write its own ACME
// certificates next to the relayed ones — and drops the chmod: keys stay 0600 owned by caddy,
// which Caddy reads as owner and root processes (sing-box, the agent, the remnanode container's
// root) read through CAP_DAC_OVERRIDE.
package caddy

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

const (
	// LibDir is Caddy's state directory; the certificate store lives under it.
	LibDir = "/var/lib/caddy"
	// CloudflareEnvPath is the EnvironmentFile of the Caddy override (Xeon fronts only).
	CloudflareEnvPath = "/etc/caddy/cloudflare.env"
	// DefaultCustomBinaryPath is where UploadCaddyBinary puts the custom build.
	DefaultCustomBinaryPath = "/usr/local/bin/caddy-custom"
	// DefaultCaddyfilePath applies when CADDYFILE_PATH is set but empty (TS `||` fallback).
	DefaultCaddyfilePath = "/etc/caddy/Caddyfile"
	// DefaultCamouflagePath applies when the request carries none.
	DefaultCamouflagePath = "/var/www/camouflage"
)

// Timeouts of the external commands (TS had none, spec B R5).
const (
	chownTimeout    = 60 * time.Second
	chmodTimeout    = 10 * time.Second
	validateTimeout = 60 * time.Second
	reloadTimeout   = 90 * time.Second
	journalTimeout  = 10 * time.Second
)

// XraySocketPaths are the unix sockets the Remnawave xray creates and Caddy proxies to. TS knew
// the first two (caddy.ts:153-156); the whitelist/CDN socket is proxied by the orchestrator's
// Caddyfile too (orch:src/core/generators/caddy.ts) and was never chmodded (spec B R14).
var XraySocketPaths = []string{
	"/dev/shm/vless-xhttp.sock",
	"/dev/shm/vless-grpc.sock",
	"/dev/shm/vless-xhttp-cdn.sock",
}

// DefaultCamouflageRoots are the directories a camouflage path must resolve into
// (caddy.ts:118-123, without the TS-only `<cwd>/tests/temp`, spec B R13).
var DefaultCamouflageRoots = []string{"/var/www", "/tmp/camouflage", "/opt/route-agent/decoy"}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return jsjson.Trim(s) }

// FixPermissions is fixCaddyPermissions without its `chmod -R 755` (see the package comment):
// when dir exists, `chown -R caddy:caddy <dir>`; a failure (no caddy user, not root) is only
// logged, as the TS `|| true` swallowed it. It never fails the caller.
func FixPermissions(ctx context.Context, r runner.Runner, f fsx.FS, log *slog.Logger, dir string) {
	if _, err := os.Stat(f.Path(dir)); err != nil {
		return
	}
	if _, err := r.Run(ctx, runner.Cmd{Name: "chown", Args: []string{"-R", "caddy:caddy", dir}, Timeout: chownTimeout}); err != nil {
		log.Debug("chown of the Caddy state directory failed (ignored, as `|| true` in TS)", "err", err.Error(), "dir", dir)
	}
}

// FixXraySocketPermissions runs `chmod 666 <socket>` for each path that exists, so Caddy (user
// caddy) can connect to sockets the root-run xray created. Failures are logged per path and the
// rest are still attempted; it never fails the caller (caddy.ts:180-196).
func FixXraySocketPermissions(ctx context.Context, r runner.Runner, f fsx.FS, log *slog.Logger, paths []string) {
	for _, p := range paths {
		if _, err := os.Stat(f.Path(p)); err != nil {
			continue
		}
		if _, err := r.Run(ctx, runner.Cmd{Name: "chmod", Args: []string{"666", p}, Timeout: chmodTimeout}); err != nil {
			log.Warn("Failed to adjust permissions on Xray unix socket", "err", err.Error(), "socketPath", p)
		}
	}
}

// ResolveBinary is resolveCaddyBinary: CADDY_BINARY_PATH (or /usr/local/bin/caddy-custom) when
// that file exists at CALL time — a custom binary uploaded mid-lifetime takes effect without an
// agent restart — else the bare "caddy" from PATH.
func ResolveBinary(f fsx.FS, cfg *config.Config) string {
	custom := orDefault(cfg.CaddyBinaryPath, DefaultCustomBinaryPath)
	if _, err := os.Stat(f.Path(custom)); err == nil {
		return custom
	}
	return "caddy"
}

// LoadDNSProviderEnv is loadCaddyDnsProviderEnv: KEY=value lines of the env file (trimmed lines,
// blanks and #-comments skipped, split at the first "=", key and value not trimmed further). A
// missing file gives an empty map.
func LoadDNSProviderEnv(f fsx.FS, path string) map[string]string {
	env := map[string]string{}
	data, err := f.ReadFile(path)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := jsTrim(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	return env
}

// ValidateCmd is `<bin> validate --config <caddyfile>` with the DNS-provider env file merged into
// the child environment: EnvironmentFile= reaches only processes systemd starts, not an ad-hoc
// child, and a Caddyfile with `dns cloudflare {env.CF_API_TOKEN}` fails validation without it.
// ConfigureCaddy validates the new Caddyfile with the resolved binary; UploadCaddyBinary
// (internal/binaries) validates the live Caddyfile with the candidate binary before the swap.
func ValidateCmd(f fsx.FS, bin, caddyfile string) runner.Cmd {
	return runner.Cmd{
		Name:    bin,
		Args:    []string{"validate", "--config", caddyfile},
		Env:     mergedEnv(LoadDNSProviderEnv(f, CloudflareEnvPath)),
		Timeout: validateTimeout,
	}
}

// mergedEnv is {...process.env, ...extra} on top of the runner's child environment.
func mergedEnv(extra map[string]string) []string {
	base := runner.ChildEnv()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, over := extra[k]; over {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// ErrInvalidPath is validateSafeCamouflagePath's "Invalid path provided".
var ErrInvalidPath = errors.New("Invalid path provided")

// ValidateSafeCamouflagePath resolves target (relative to the cwd, no symlink resolution) and
// accepts it only inside one of roots (the root itself included). Otherwise the error text is
// "Path Traversal restriction: <target> is not within allowed directories".
func ValidateSafeCamouflagePath(target string, roots []string) (string, error) {
	if target == "" {
		return "", ErrInvalidPath
	}
	resolved, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		rel, err := filepath.Rel(filepath.Clean(root), resolved)
		if err != nil {
			continue
		}
		// Same test as TS: relative.startsWith('..') — a child named "..x" is rejected too.
		if !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return resolved, nil
		}
	}
	return "", errors.New("Path Traversal restriction: " + target + " is not within allowed directories")
}
