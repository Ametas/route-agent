package main

import (
	"context"
	"runtime"
	"time"

	"github.com/Ametas/route-agent/internal/agentinfo"
	"github.com/Ametas/route-agent/internal/awg"
	"github.com/Ametas/route-agent/internal/caddy"
	"github.com/Ametas/route-agent/internal/certs"
	"github.com/Ametas/route-agent/internal/firewall"
	"github.com/Ametas/route-agent/internal/kernel"
	"github.com/Ametas/route-agent/internal/lifecycle"
	"github.com/Ametas/route-agent/internal/mesh"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/shaper"
	"github.com/Ametas/route-agent/internal/singbox"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/update"
)

// buildHandlers wires every implemented RPC. A porting group adds ONE block here: build its
// service from deps (runner, fs, systemctl, auth, cfg, log) and assign its Handlers fields, e.g.
//
//	sb := singbox.New(singbox.Deps{Auth: d.auth, Runner: d.runner, FS: d.fs, Cfg: d.cfg, Log: d.log})
//	h.ApplyConfig = sb.ApplyConfig
//	h.ConfigureCaddy = sb.ConfigureCaddy
//
// Pass d.locks (the shared area lock registry, internal/locks) to every service that changes an
// area; its package doc lists which RPC takes which lock and the order for nesting.
//
// Fields left nil answer codes.Unimplemented (internal/server/stubs.go).
func buildHandlers(d deps) *server.Handlers {
	d = withLocks(d)
	h := &server.Handlers{}

	// Group A — process, security, lifecycle (spec A).
	info := &agentinfo.Service{Version: d.version.Get, Log: d.log}
	h.GetAgentInfo = info.GetAgentInfo

	upd := &update.Updater{
		Auth:       d.auth,
		Runner:     d.runner,
		Systemctl:  d.systemctl,
		Log:        d.log,
		BinaryPath: d.exe,
		Arch:       runtime.GOARCH,
		Unit:       "route-agent",
		Delay:      time.Second,
	}
	h.SelfUpdate = upd.SelfUpdate

	crt := &certs.Service{Auth: d.auth, FS: d.fs, Log: d.log}
	h.GetManagedCertificate = crt.GetManagedCertificate

	// Group B — config apply: sing-box, Caddy, Clash API observation (spec B).
	// sb is also what later groups reuse: sb.Version (E, C), sb.ValidateConfig (F),
	// sb.DetectCoreNotListening (E, PullNodeWarnings); caddy.FixPermissions,
	// caddy.ResolveBinary and caddy.LoadDNSProviderEnv are free functions.
	sb := &singbox.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Locks: d.locks}
	h.ApplyConfig = sb.ApplyConfig
	h.GetSingBoxUserTraffic = sb.GetSingBoxUserTraffic
	h.GetSingBoxConnections = sb.GetSingBoxConnections
	cdy := &caddy.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Locks: d.locks}
	h.ConfigureCaddy = cdy.ConfigureCaddy
	// Group C — binaries, rule sets, kernel source (spec C; cmd/route-agent/wire_binaries.go).
	wireBinaries(h, newBinaries(d))
	// Group D — AWG, mesh tunnel, firewall, traffic shaping (spec D). The mesh telemetry readers
	// (mesh.Status) and the AWG health loop (startPreBindLoops) are what group E formats.
	fw := &firewall.Service{Auth: d.auth, Runner: d.runner, Log: d.log, Locks: d.locks}
	h.ManageFirewall = fw.ManageFirewall
	awgSvc := &awg.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Firewall: fw, Locks: d.locks}
	h.ConfigureAwg = awgSvc.ConfigureAwg
	h.GetAwgObservation = awgSvc.GetAwgObservation
	meshSvc := &mesh.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Firewall: fw, Locks: d.locks}
	h.ConfigureMeshTunnel = meshSvc.ConfigureMeshTunnel
	thr := &shaper.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Log: d.log}
	h.ApplyTrafficThrottle = thr.ApplyTrafficThrottle
	// Group E — TODO: StreamTelemetry, GetTelemetry, PullNodeWarnings, RunNetworkDiagnostic, GetWarpKeyHealth.
	// Group F — rear (sing-box/mihomo), WARP guard, Olcrtc (spec F; cmd/route-agent/wire_groupf.go).
	wireGroupF(h, d)
	return h
}

// preBindSteps are startServer steps 1–4 (spec A §1.3), run in order before the credentials
// check. Each must never fail the boot.
func preBindSteps(d deps) []lifecycle.Step {
	return []lifecycle.Step{
		{Name: "sanitizeAwgToolsSymlink", Run: newBinaries(d).SanitizeAwgToolsSymlink}, // 1. spec A §10.1, C §3
		{Name: "ensureAwgSystemdUnit", Run: func(ctx context.Context) {
			systemd.EnsureUnit(ctx, d.fs, d.systemctl, d.log, "AWG",
				systemd.AwgUnitPath(d.cfg.AwgUnitFilePath), systemd.AwgUnit(d.cfg.AwgToolsBinaryPath))
		}},
		{Name: "ensureOlcrtcAgentSrvSystemdUnit", Run: func(ctx context.Context) {
			systemd.EnsureUnit(ctx, d.fs, d.systemctl, d.log, "olcrtc-agent-srv",
				d.cfg.OlcrtcAgentSrvUnitFilePath, systemd.OlcrtcAgentSrvUnit(d.cfg.OlcrtcAgentSrvBinaryPath, d.cfg.OlcrtcAgentSrvConfigDir))
		}},
		{Name: "fixXraySocketPermissions", Run: func(ctx context.Context) {
			caddy.FixXraySocketPermissions(ctx, d.runner, d.fs, d.log, caddy.XraySocketPaths)
		}},
	}
}

// startPreBindLoops is startServer step 5: loops TS started before the credentials check.
func startPreBindLoops(ctx context.Context, bg *lifecycle.Group, d deps) {
	_ = ctx
	_ = bg
	_ = d
	// AWG health and self-heal, every 30 s, ticks never overlap (spec A §10.3, D §4, E §2.5). Group E
	// reads its Status(); hoist the instance into deps when telemetry lands.
	awgHealth := &awg.Health{Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Locks: d.locks}
	bg.Start(ctx, lifecycle.Loop{Name: "awg-health", Interval: awg.HealthInterval, FirstDelay: time.Second, Tick: awgHealth.Tick})
}

// startPostBind is startServer step 9: after a successful bind.
func startPostBind(ctx context.Context, bg *lifecycle.Group, d deps) {
	_ = ctx
	_ = bg
	_ = d
	bg.Go(ctx, "kernel-tuning", (&kernel.Tuning{Runner: d.runner, FS: d.fs, Log: d.log, ProfilePath: d.cfg.SysctlProfilePath}).Run) // spec A §10.5, D §10
	// Rear WARP guard (spec A §10.4, F §3; cmd/route-agent/wire_groupf.go).
	startRearWarpGuard(ctx, bg, d)
}
