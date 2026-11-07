// Package singbox is the Go port of the sing-box side of group B (spec B §1–3, §6–8, §5.2):
//
//   - ApplyConfig (apply.go): unchanged-skip, `sing-box check`, egress ufw sync, hot roster swap
//     through the fork's users-api, write + reload with rollback;
//   - the core primitives other groups reuse (core.go): unit state, stale-image detection, reload
//     command choice, ValidateConfig (the rear core, group F), Version (telemetry, group E;
//     binaries, group C), the unit file (unit.go, used by UploadSingboxBinary, group C);
//   - GetSingBoxUserTraffic and GetSingBoxConnections over the local Clash API (stats.go,
//     connections.go, transport in internal/clashapi);
//   - DetectCoreNotListening (listener.go) for PullNodeWarnings (group E).
//
// Configs are handled as internal/jsjson values so that what is written and compared is
// byte-identical to the TS agent's JSON.stringify(x, null, 2) (spec B §2): the first push after
// the switchover still hits "already current" on an unchanged node.
package singbox

import (
	"log/slog"
	"os/exec"
	"time"

	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Defaults behind the TS `config.X || '<default>'` fallbacks (an env var set to "" is kept by
// the config schema, then replaced here).
const (
	DefaultConfigPath = "/etc/sing-box/config.json"
	DefaultBinaryPath = "/usr/local/bin/sing-box"
	DefaultUnitPath   = "/etc/systemd/system/sing-box.service"
	// ActivePortsCachePath is the ufw port cache of the egress firewall sync.
	ActivePortsCachePath = "/opt/route-agent/active_ports.json"
)

// Timeouts of the external commands (TS had none, spec B R5).
const (
	versionTimeout = 10 * time.Second
	checkTimeout   = 60 * time.Second
	reloadTimeout  = 90 * time.Second
	ufwTimeout     = 30 * time.Second
	ssTimeout      = 10 * time.Second
)

// Service holds the dependencies and the per-area state of group B's sing-box side.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger

	// UsersAPI talks to the fork's users-api socket; the zero value uses the real socket.
	UsersAPI UsersAPI
	// Clash fetches /connections; the zero value uses a plain HTTP client.
	Clash clashapi.Client
	// LookPath finds ufw (`command -v ufw` in TS); nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Now is the clock of the listener warning; nil means time.Now.
	Now func() time.Time

	// Locks is the agent-wide area lock registry (internal/locks), shared with UploadSingboxBinary
	// (group C) and ManageFirewall (group D). ApplyConfig holds locks.SingBox for its whole
	// read-compare-write-reload (spec B §1.6, R6) and nests locks.Firewall around the ufw sync.
	// nil means a registry private to this Service (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
	// traffic is the per-connection baseline of GetSingBoxUserTraffic.
	traffic trafficState
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (s *Service) configPath() string { return orDefault(s.Cfg.SingboxConfigPath, DefaultConfigPath) }
func (s *Service) binaryPath() string { return orDefault(s.Cfg.SingboxBinaryPath, DefaultBinaryPath) }
func (s *Service) unitPath() string   { return orDefault(s.Cfg.SingboxUnitFilePath, DefaultUnitPath) }

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

func (s *Service) lookPath(name string) (string, error) {
	if s.LookPath != nil {
		return s.LookPath(name)
	}
	return exec.LookPath(name)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
