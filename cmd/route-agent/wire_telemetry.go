package main

import (
	"github.com/Ametas/route-agent/internal/awg"
	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/diag"
	"github.com/Ametas/route-agent/internal/mesh"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/singbox"
	"github.com/Ametas/route-agent/internal/telemetry"
	"github.com/Ametas/route-agent/internal/warnings"
)

// Group E wiring (spec E): telemetry, node warnings, network diagnostic, WARP key health — and
// the state shared with groups C, D and F.

// withShared completes deps with the process-wide shared instances (run() calls it once; tests
// that build deps by hand get theirs from buildHandlers). Idempotent.
func withShared(d deps) deps {
	d = withLocks(d)
	if d.awgHealth == nil {
		// AWG health and self-heal (spec D §4, E §2.5): its loop (startPreBindLoops) writes the
		// state, telemetry formats it.
		d.awgHealth = &awg.Health{Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Locks: d.locks}
	}
	if d.meshStatus == nil {
		// The kernel-module status cache is invalidated by the kernel-source upload (group C).
		d.meshStatus = &mesh.Status{Runner: d.runner, Cfg: d.cfg, LinkExists: telemetry.SysfsLinkExists(d.fs)}
	}
	if d.telemetry == nil {
		// sing-box version: the unit gate and parser of group B, cached here for 60 s.
		sb := &singbox.Service{Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log}
		d.telemetry = &telemetry.Collector{
			Runner: d.runner, FS: d.fs, Cfg: d.cfg, Log: d.log,
			AwgHealth: d.awgHealth, Mesh: d.meshStatus, SingboxVersion: sb.Version,
		}
	}
	return d
}

// telemetryCallbacks are the cache invalidations fired by group C after a swap (spec C §1 step
// 9, §3 step 5, §9 step 4f).
func telemetryCallbacks(d deps) binaries.Telemetry {
	var t binaries.Telemetry
	if d.telemetry != nil {
		t.SingboxSwapped = d.telemetry.InvalidateSingboxVersion
		t.AwgSwapped = d.telemetry.InvalidateAwgVersions
	}
	if d.meshStatus != nil {
		t.MeshKernelChanged = d.meshStatus.InvalidateKernel
	}
	return t
}

// wireGroupE assigns the group E handlers.
func wireGroupE(h *server.Handlers, d deps) {
	tel := &telemetry.Service{Auth: d.auth, Collector: d.telemetry, Log: d.log}
	h.StreamTelemetry = tel.StreamTelemetry
	h.GetTelemetry = tel.GetTelemetry

	sb := &singbox.Service{Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log}
	r := newRear(d)
	w := &warnings.Service{
		Auth: d.auth, Runner: d.runner, Cfg: d.cfg, Log: d.log,
		CoreNotListening: sb.DetectCoreNotListening, EmptyRuleProviders: r.DetectEmptyRuleProviders,
	}
	h.PullNodeWarnings = w.PullNodeWarnings

	dg := &diag.Service{Auth: d.auth, Runner: d.runner, Log: d.log, WarpKeyHealthReport: r.WarpKeyHealthReport}
	h.RunNetworkDiagnostic = dg.RunNetworkDiagnostic
	h.GetWarpKeyHealth = dg.GetWarpKeyHealth
}
