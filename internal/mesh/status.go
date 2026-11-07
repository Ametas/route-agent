package mesh

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/awg"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/runner"
)

// Kernel module status values (telemetry.ts:356-376). The proto also lists "error", which the
// agent never emits.
const (
	KernelNotInstalled   = "not_installed"
	KernelBuiltNotLoaded = "built_not_loaded"
	KernelLoaded         = "loaded"

	// InterfaceAbsent / InterfaceUp are the interface statuses (never "down", as in TS).
	InterfaceAbsent = "absent"
	InterfaceUp     = "up"

	// KernelStatusTTL is the cache lifetime of the kernel status (telemetry.ts:381-389).
	KernelStatusTTL = 60 * time.Second

	modinfoTimeout = 10 * time.Second
	lsmodTimeout   = 5 * time.Second
)

// KernelStatus is mesh_awg_kernel_status / mesh_awg_kernel_version.
type KernelStatus struct {
	Status  string
	Version string
}

// InterfaceStatus is mesh_tunnel_interface_status / _active_peers / _live_peer_keys. ActivePeers
// is always len(LivePeerKeys) (the orchestrator relies on it, architecture §1).
type InterfaceStatus struct {
	Status       string
	ActivePeers  uint32
	LivePeerKeys []string
}

var (
	reModVersion = regexp.MustCompile(`(?m)^version:\s*(\S+)`)
	// grep -w amneziawg: the word bounded by non-word characters (letters, digits, underscore).
	// The same check as dkms.Installer's (unexported there; group C owns that package).
	reModuleWord = regexp.MustCompile(`(^|[^A-Za-z0-9_])amneziawg($|[^A-Za-z0-9_])`)
)

// Status holds the telemetry readers of the mesh. Safe for concurrent use.
type Status struct {
	Runner runner.Runner
	Cfg    *config.Config
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	cached    *KernelStatus
	checkedAt time.Time
}

func (s *Status) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Kernel is getMeshAwgKernelStatusCached: `modinfo amneziawg` (fails → not_installed, version ""),
// version from its `version:` line, then lsmod contains the word amneziawg → loaded, else
// built_not_loaded (`lsmod | grep -w amneziawg` without a shell). Cached for 60 s.
func (s *Status) Kernel(ctx context.Context) KernelStatus {
	now := s.now()
	s.mu.Lock()
	if s.cached != nil && now.Sub(s.checkedAt) < KernelStatusTTL {
		ks := *s.cached
		s.mu.Unlock()
		return ks
	}
	s.mu.Unlock()

	ks := s.probeKernel(ctx)
	s.mu.Lock()
	s.cached = &ks
	s.checkedAt = now
	s.mu.Unlock()
	return ks
}

func (s *Status) probeKernel(ctx context.Context) KernelStatus {
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: "modinfo", Args: []string{"amneziawg"}, Timeout: modinfoTimeout})
	if err != nil {
		return KernelStatus{Status: KernelNotInstalled}
	}
	version := ""
	if m := reModVersion.FindStringSubmatch(res.Stdout); m != nil {
		version = m[1]
	}
	lsmod, err := s.Runner.Run(ctx, runner.Cmd{Name: "lsmod", Timeout: lsmodTimeout})
	if err == nil && reModuleWord.MatchString(lsmod.Stdout) {
		return KernelStatus{Status: KernelLoaded, Version: version}
	}
	return KernelStatus{Status: KernelBuiltNotLoaded, Version: version}
}

// InvalidateKernel drops the cached kernel status (invalidateMeshAwgKernelStatusCache), so the next
// telemetry tick re-reads it — wire it to binaries.Telemetry.MeshKernelChanged.
func (s *Status) InvalidateKernel() {
	s.mu.Lock()
	s.cached = nil
	s.checkedAt = time.Time{}
	s.mu.Unlock()
}

// Interface is getMeshTunnelInterfaceStatus: `ip link show <mesh iface>` fails → absent; else
// `awg show <iface> dump` → up with the live peers (handshake ≤ 180 s); a failed dump → up, 0, [].
func (s *Status) Interface(ctx context.Context) InterfaceStatus {
	iface := Iface(s.Cfg)
	if _, err := s.Runner.Run(ctx, runner.Cmd{Name: "ip", Args: []string{"link", "show", iface}, Timeout: awg.IPTimeout}); err != nil {
		return InterfaceStatus{Status: InterfaceAbsent, LivePeerKeys: []string{}}
	}
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: "awg", Args: []string{"show", iface, "dump"}, Timeout: awg.ShowTimeout})
	if err != nil {
		return InterfaceStatus{Status: InterfaceUp, LivePeerKeys: []string{}}
	}
	keys := awg.LivePeers(res.Stdout, s.now().Unix())
	return InterfaceStatus{Status: InterfaceUp, ActivePeers: uint32(len(keys)), LivePeerKeys: keys}
}
