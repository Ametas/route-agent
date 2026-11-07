package binaries

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Size caps (spec C risk 17: TS had none). The sing-box fork is ~81 MB today.
const (
	DefaultMaxBinaryBytes        int64 = 256 << 20
	DefaultMaxRuleSetBytes       int64 = 32 << 20
	DefaultMaxKernelSrcBytes     int64 = 64 << 20
	defaultPostCheckSettle             = 3 * time.Second
	smokeTimeout                       = 30 * time.Second
	checkTimeout                       = 60 * time.Second
	setcapTimeout                      = 15 * time.Second
	restartCommandTimeout              = 120 * time.Second
	singboxCapabilities                = "cap_net_admin,cap_net_bind_service=+ep"
	defaultAwgToolsPath                = "/usr/local/bin/awg"
	defaultAwgQuickPath                = "/usr/local/bin/awg-quick"
	defaultAwgGoPath                   = "/usr/local/bin/amneziawg-go"
	defaultSingboxPath                 = "/usr/local/bin/sing-box"
	defaultCaddyCustomPath             = "/usr/local/bin/caddy-custom"
	defaultCaddyfilePath               = "/etc/caddy/Caddyfile"
	defaultSingboxRestartCommand       = "systemctl restart sing-box"
)

// Result is what an apply step answers (UploadBinaryResponse / UpgradeResponse fields).
type Result struct {
	Success bool
	Message string
}

func ok(msg string) Result   { return Result{Success: true, Message: msg} }
func fail(msg string) Result { return Result{Success: false, Message: msg} }

// Telemetry holds the cache-invalidation callbacks of group E (see the package doc).
type Telemetry struct {
	SingboxSwapped    func()
	AwgSwapped        func()
	MeshKernelChanged func()
}

func call(f func()) {
	if f != nil {
		f()
	}
}

// Deps are what New needs.
type Deps struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger

	// Rear, Olcrtc, Mesh replace the defaults (nil → DefaultRear, DefaultOlcrtc, dkms.Installer).
	Rear      RearHooks
	Olcrtc    OlcrtcHooks
	Mesh      MeshKernelInstaller
	Telemetry Telemetry
}

// Service implements the group C handlers.
type Service struct {
	auth      *auth.Verifier
	runner    runner.Runner
	fs        fsx.FS
	systemctl systemd.Systemctl
	cfg       *config.Config
	log       *slog.Logger

	rear      RearHooks
	olcrtc    OlcrtcHooks
	mesh      MeshKernelInstaller
	telemetry Telemetry

	// Arch is the GOARCH an uploaded ELF must be built for ("" skips the machine check).
	Arch string
	// MaxBinaryBytes, MaxRuleSetBytes, MaxKernelSrcBytes cap one upload.
	MaxBinaryBytes    int64
	MaxRuleSetBytes   int64
	MaxKernelSrcBytes int64
	// PostCheckSettle is how long after a restart the unit must still be active before the swap
	// counts as good (a Type=simple unit with a crashing binary is "activating (auto-restart)").
	PostCheckSettle time.Duration
	// TempDir is where the kernel-source tarball and rule-set uploads are received (real path;
	// "" → os.TempDir()). Binaries are received next to their target instead.
	TempDir string

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// New builds the service with production defaults.
func New(d Deps) *Service {
	s := &Service{
		auth: d.Auth, runner: d.Runner, fs: d.FS, systemctl: d.Systemctl, cfg: d.Cfg, log: d.Log,
		rear: d.Rear, olcrtc: d.Olcrtc, mesh: d.Mesh, telemetry: d.Telemetry,
		Arch:              runtime.GOARCH,
		MaxBinaryBytes:    DefaultMaxBinaryBytes,
		MaxRuleSetBytes:   DefaultMaxRuleSetBytes,
		MaxKernelSrcBytes: DefaultMaxKernelSrcBytes,
		PostCheckSettle:   defaultPostCheckSettle,
		locks:             map[string]*sync.Mutex{},
	}
	if s.rear == nil {
		s.rear = &DefaultRear{Runner: d.Runner, FS: d.FS, Systemctl: d.Systemctl, Cfg: d.Cfg, Log: d.Log}
	}
	if s.olcrtc == nil {
		s.olcrtc = &DefaultOlcrtc{Runner: d.Runner, FS: d.FS, Systemctl: d.Systemctl, Cfg: d.Cfg, Log: d.Log}
	}
	if s.mesh == nil {
		s.mesh = newDefaultMeshInstaller(d)
	}
	return s
}

// lock serializes apply steps per key (one upload of one target at a time). A second upload of
// the same target waits for the first and then applies its own file (last one wins, in order).
func (s *Service) lock(key string) func() {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m, found := s.locks[key]
	if !found {
		m = &sync.Mutex{}
		s.locks[key] = m
	}
	s.locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Service) settle(ctx context.Context) {
	if s.PostCheckSettle <= 0 {
		return
	}
	t := time.NewTimer(s.PostCheckSettle)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// run executes a command with the given timeout.
func (s *Service) run(ctx context.Context, timeout time.Duration, name string, args ...string) (runner.Result, error) {
	return s.runner.Run(ctx, runner.Cmd{Name: name, Args: args, Timeout: timeout})
}

// setcap grants the sing-box capabilities; failure is only logged (spec C §1 steps 5, 8).
func (s *Service) setcap(ctx context.Context, realPath, logMsg string) {
	if _, err := s.run(ctx, setcapTimeout, "setcap", singboxCapabilities, realPath); err != nil {
		s.log.Warn(logMsg, "err", err.Error())
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// JS String.slice counts UTF-16 units; cut on a rune boundary so the text stays valid UTF-8.
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
