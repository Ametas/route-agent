package main

import (
	"context"
	"runtime"
	"time"

	"github.com/Ametas/route-agent/internal/agentinfo"
	"github.com/Ametas/route-agent/internal/certs"
	"github.com/Ametas/route-agent/internal/lifecycle"
	"github.com/Ametas/route-agent/internal/server"
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
// Fields left nil answer codes.Unimplemented (internal/server/stubs.go).
func buildHandlers(d deps) *server.Handlers {
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

	// Group B — TODO: ApplyConfig, ConfigureCaddy, GetSingBoxUserTraffic, GetSingBoxConnections.
	// Group C — TODO: Upload*Binary, UpgradeSingbox, UploadRearRuleSet, UploadMeshAwgKernelSource.
	// Group D — TODO: ConfigureAwg, GetAwgObservation, ConfigureMeshTunnel, ManageFirewall, ApplyTrafficThrottle.
	// Group E — TODO: StreamTelemetry, GetTelemetry, PullNodeWarnings, RunNetworkDiagnostic, GetWarpKeyHealth.
	// Group F — TODO: ConfigureRearSingbox, GetRearConnections, SyncOlcrtcInstance, DeleteOlcrtcInstance, StreamOlcrtcEvents.
	return h
}

// preBindSteps are startServer steps 1–4 (spec A §1.3), run in order before the credentials
// check. Each must never fail the boot.
func preBindSteps(d deps) []lifecycle.Step {
	return []lifecycle.Step{
		// 1. TODO(group C): sanitizeAwgToolsSymlink (spec A §10.1).
		{Name: "ensureAwgSystemdUnit", Run: func(ctx context.Context) {
			systemd.EnsureUnit(ctx, d.fs, d.systemctl, d.log, "AWG",
				systemd.AwgUnitPath(d.cfg.AwgUnitFilePath), systemd.AwgUnit(d.cfg.AwgToolsBinaryPath))
		}},
		{Name: "ensureOlcrtcAgentSrvSystemdUnit", Run: func(ctx context.Context) {
			systemd.EnsureUnit(ctx, d.fs, d.systemctl, d.log, "olcrtc-agent-srv",
				d.cfg.OlcrtcAgentSrvUnitFilePath, systemd.OlcrtcAgentSrvUnit(d.cfg.OlcrtcAgentSrvBinaryPath, d.cfg.OlcrtcAgentSrvConfigDir))
		}},
		// 4. TODO(group B): fixXraySocketPermissions (spec A §10.2).
	}
}

// startPreBindLoops is startServer step 5: loops TS started before the credentials check.
func startPreBindLoops(ctx context.Context, bg *lifecycle.Group, d deps) {
	_ = ctx
	_ = bg
	_ = d
	// TODO(group D/E): AWG health and self-heal, every 30 s (spec A §10.3, E §2.5):
	//   bg.Start(ctx, lifecycle.Loop{Name: "awg-health", Interval: 30 * time.Second, Tick: awgHealth.Tick})
}

// startPostBind is startServer step 9: after a successful bind.
func startPostBind(ctx context.Context, bg *lifecycle.Group, d deps) {
	_ = ctx
	_ = bg
	_ = d
	// TODO(group D): kernel tuning, one-shot (spec A §10.5) — bg.Go(ctx, "kernel-tuning", tuning.Apply)
	// TODO(group F): rear WARP guard every REAR_WARP_GUARD_INTERVAL_MS (spec A §10.4)
}
