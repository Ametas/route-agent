package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/awg"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/mesh"
	"github.com/Ametas/route-agent/internal/runner"
)

// VersionTTL is the lifetime of the sing-box and AWG version caches (telemetry.ts:146-157,
// 307-329).
const VersionTTL = 60 * time.Second

// webrtc_status values (telemetry.ts:76-96); the orchestrator compares the first three.
const (
	WebRTCNominal         = "nominal"
	WebRTCNoActiveTunnels = "no_active_tunnels"
	WebRTCNotInstalled    = "not_installed"
	WebRTCUnavailable     = "unavailable"
)

// unknown is what a frame carries for a cached value no probe has produced yet (only possible
// when the very first probe outlives the frame budget). The orchestrator reads it as "no data"
// (versionStatus.ts treats 'unknown' as not installed but raises no alert; NodeWarningWorker ignores
// anything but not_installed / built_not_loaded / loaded) — unlike not_installed, which would
// trigger a mesh self-repair.
const unknown = "unknown"

const systemctlTimeout = 10 * time.Second

// AwgStatus is what the AWG health loop exposes to telemetry ((*awg.Health).Status).
type AwgStatus interface {
	Status() string
}

// Collector gathers one telemetry frame (src/utils/telemetry.ts, hostNetwork.ts). It holds the
// caches shared by every stream and GetTelemetry; build ONE per process.
//
// It never heals anything: the AWG self-restart lives in its own 30 s loop (awg.Health), the frame
// only formats that loop's state (spec E §2.5, R2; deviations.md).
type Collector struct {
	Runner runner.Runner
	FS     fsx.FS
	Cfg    *config.Config
	Log    *slog.Logger
	// AwgHealth is the shared *awg.Health of the health loop.
	AwgHealth AwgStatus
	// Mesh is the shared *mesh.Status (kernel-module cache, interface reader).
	Mesh *mesh.Status
	// SingboxVersion is (*singbox.Service).Version (unit gate + `sing-box version`, uncached).
	SingboxVersion func(ctx context.Context) string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	webrtc     source[string]
	peers      source[uint32]
	meshKernel source[mesh.KernelStatus]
	meshIface  source[mesh.InterfaceStatus]

	initOnce sync.Once
	singbox  cached[string]
	awgVers  cached[awgVersions]
}

func (c *Collector) init() {
	c.initOnce.Do(func() {
		c.singbox.ttl = VersionTTL
		c.awgVers.ttl = VersionTTL
	})
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// InvalidateSingboxVersion is invalidateSingboxVersionCache: after a sing-box binary swap the
// next frame re-reads the version (binaries.Telemetry.SingboxSwapped).
func (c *Collector) InvalidateSingboxVersion() {
	c.init()
	c.singbox.invalidate()
}

// InvalidateAwgVersions is invalidateAwgVersionCache: clears tools and Go-core versions together
// (binaries.Telemetry.AwgSwapped, after an awg / awg-quick / amneziawg-go upload).
func (c *Collector) InvalidateAwgVersions() {
	c.init()
	c.awgVers.invalidate()
}

// WebRTCStatus is getWebRtcStatus (telemetry.ts:76-96): not_installed without the
// olcrtc-agent-srv binary; otherwise the number of running olcrtc-agent-srv@* units decides
// nominal / no_active_tunnels.
//
// TS ran `systemctl list-units … | awk '{print $1}'` through a shell; the pipeline's status was
// awk's, so a failing systemctl still meant "0 running" (no_active_tunnels) and 'unavailable'
// needed sh or awk to fail. Here systemctl runs without a shell, its stdout (partial on failure)
// is counted the same way (lines with a first field), and 'unavailable' is answered only when the
// command was killed by its timeout — the one case TS never answered at all.
func (c *Collector) WebRTCStatus(ctx context.Context) string {
	bin := c.Cfg.OlcrtcAgentSrvBinaryPath
	if bin == "" {
		return WebRTCNotInstalled
	}
	if _, err := os.Stat(c.FS.Path(bin)); err != nil {
		return WebRTCNotInstalled
	}
	res, err := c.Runner.Run(ctx, runner.Cmd{
		Name:    "systemctl",
		Args:    []string{"list-units", "--type=service", "--state=running", "--no-legend", "olcrtc-agent-srv@*"},
		Timeout: systemctlTimeout,
	})
	var re *runner.Error
	if err != nil && errors.As(err, &re) && re.Killed {
		return WebRTCUnavailable
	}
	running := 0
	for _, line := range strings.Split(res.Stdout, "\n") {
		if len(strings.Fields(line)) > 0 {
			running++
		}
	}
	if running == 0 {
		return WebRTCNoActiveTunnels
	}
	return WebRTCNominal
}

func (c *Collector) singboxVersion(ctx context.Context) string {
	if c.SingboxVersion == nil {
		return NotInstalled
	}
	return c.SingboxVersion(ctx)
}

func (c *Collector) awgVersionsProbe(ctx context.Context) awgVersions {
	var v awgVersions
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); v.Tools = AwgToolsVersion(ctx, c.Runner, c.Cfg.AwgToolsBinaryPath) }()
	go func() { defer wg.Done(); v.Go = AwgGoVersion(ctx, c.Runner, c.Cfg.AwgGoBinaryPath) }()
	wg.Wait()
	return v
}

// Frame collects every field of a TelemetryResponse except system_logs and timestamp, which
// belong to the caller (stream vs unary). All sources run concurrently; a command-backed source
// waits at most budget (see source). prev is the CPU baseline; the new one is returned.
func (c *Collector) Frame(ctx context.Context, prev CPUStats, budget time.Duration) (*agentpb.TelemetryResponse, CPUStats) {
	c.init()
	now := c.now()
	f := &agentpb.TelemetryResponse{}
	var next CPUStats
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}

	run(func() {
		stat, err := c.FS.ReadFile("/proc/stat")
		f.CpuUsage, next = CPUUsage(string(stat), err, prev)
	})
	run(func() {
		mi, err := c.FS.ReadFile("/proc/meminfo")
		f.MemUsage = MemUsage(string(mi), err)
	})
	run(func() { f.ActiveConnections = ConnectionCount(c.FS) })
	run(func() {
		if hn := ReadHostNetwork(c.FS); hn != nil {
			f.HostNetIface, f.HostNetRxBytes, f.HostNetTxBytes = hn.Iface, hn.RxBytes, hn.TxBytes
		}
	})
	run(func() { f.WebrtcStatus = c.webrtc.get(ctx, budget, WebRTCUnavailable, c.WebRTCStatus) })
	run(func() { f.SingboxVersion = c.singbox.get(ctx, now, budget, unknown, c.singboxVersion) })
	run(func() {
		f.AwgActivePeers = c.peers.get(ctx, budget, 0, func(pctx context.Context) uint32 {
			return awg.ActivePeers(pctx, c.Runner, c.now())
		})
	})
	run(func() {
		v := c.awgVers.get(ctx, now, budget, awgVersions{Tools: unknown, Go: unknown}, c.awgVersionsProbe)
		f.AwgToolsVersion, f.AwgVersion, f.AwgGoVersion = v.Tools, v.Tools, v.Go
	})
	run(func() {
		if c.AwgHealth == nil {
			f.AwgStatus = unknown
			return
		}
		f.AwgStatus = c.AwgHealth.Status()
	})
	run(func() {
		if c.Mesh == nil {
			f.MeshAwgKernelStatus = unknown
			return
		}
		k := c.meshKernel.get(ctx, budget, mesh.KernelStatus{Status: unknown}, c.Mesh.Kernel)
		f.MeshAwgKernelStatus, f.MeshAwgKernelVersion = k.Status, k.Version
	})
	run(func() {
		if c.Mesh == nil {
			f.MeshTunnelInterfaceStatus, f.MeshTunnelLivePeerKeys = unknown, []string{}
			return
		}
		st := c.meshIface.get(ctx, budget, mesh.InterfaceStatus{Status: unknown, LivePeerKeys: []string{}}, c.Mesh.Interface)
		// active_peers is len(live_peer_keys) by construction (mesh.Status), which the orchestrator
		// relies on (meshRouting.ts:29-40); re-derived here so no fallback can break it.
		f.MeshTunnelInterfaceStatus = st.Status
		f.MeshTunnelLivePeerKeys = st.LivePeerKeys
		f.MeshTunnelActivePeers = uint32(len(st.LivePeerKeys))
	})
	wg.Wait()
	return f, next
}

// SysfsLinkExists answers "does network interface iface exist" from /sys/class/net, the
// file-system twin of `ip link show <iface>` exiting 0 (spec E §8.1). Telemetry asked it every 2 s
// per stream through a process; wire it into mesh.Status.LinkExists.
func SysfsLinkExists(fs fsx.FS) func(iface string) bool {
	return func(iface string) bool {
		if iface == "" || strings.ContainsAny(iface, "/") || iface == "." || iface == ".." {
			return false
		}
		_, err := os.Lstat(fs.Path("/sys/class/net/" + iface))
		return err == nil
	}
}
