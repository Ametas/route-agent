// Package rear is the Go port of the rear proxy of group F (spec F §1–5): the second egress
// instance on a node, run either on sing-box (the front's own binary) or on mihomo, which decides
// where the subscribers' "star" traffic leaves through and holds the node's WARP keys.
//
//   - ConfigureRearSingbox (configure.go): enable/disable, core switch, unchanged-skip, check by the
//     core's own binary, atomic write, reload (never restart unless the unit changed);
//   - the WARP guard loop (guard.go): flips the `warp` selector between the pool and direct by
//     the live key count, locally and while the orchestrator is away;
//   - read-only observations over the rear Clash API (clash.go): GetRearConnections, and for group
//     E the WARP key health (GetWarpKeyHealth) and the empty rule-provider warning
//     (PullNodeWarnings);
//   - the hooks the mihomo and rule-set uploads of group C need (hooks.go, binaries.RearHooks).
//
// Which core carries the rear is read from disk (whose config exists, mihomo first), never from
// the orchestrator: the guard and the health report run locally every 30 s.
package rear

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Timeouts (TS had none on any command, spec F R9; fetches had 5 s / 3 s).
const (
	// mihomoCheckTimeout bounds `mihomo -t` (it parses every rule set of the config).
	mihomoCheckTimeout = 60 * time.Second
	// clashTimeout bounds one Clash API call (AbortSignal.timeout(5000)).
	clashTimeout = 5 * time.Second
	// ruleProvidersTimeout bounds GET /providers/rules (AbortSignal.timeout(3000)).
	ruleProvidersTimeout = 3 * time.Second
	// maxClashBody caps a Clash API response kept in memory.
	maxClashBody = 64 << 20
)

// configMode: rear configs carry the WARP private keys of the node (spec F R4: TS wrote them
// with the umask default, 0644). The owner of an existing file is kept by fsx.
const configMode = 0o600

// Service holds the dependencies of the rear handlers, loop and hooks. It keeps no state of its
// own, so several instances (handlers, upload hooks, group E) may coexist.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger

	// ValidateSingbox checks a sing-box config with the sing-box binary
	// (singbox.Service.ValidateConfig: `sing-box check -c` on a temp file next to the FRONT config,
	// after the Caddy ownership fix-up — exactly validateSingBoxConfig, which the TS rear reused).
	ValidateSingbox func(ctx context.Context, cfg any) (bool, string)
	// HTTP is the client for the rear Clash API; nil means http.DefaultClient (every call still has
	// its own timeout).
	HTTP *http.Client
	// Now is the clock (warning timestamps); nil means time.Now.
	Now func() time.Time

	// Locks is the agent-wide area lock registry (internal/locks): ConfigureRearSingbox holds
	// locks.Rear, shared with UploadMihomoBinary and UploadRearRuleSet (group C). nil means a
	// registry private to this Service (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
}

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

func (s *Service) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
