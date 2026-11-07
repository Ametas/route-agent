package main

import (
	"context"

	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/lifecycle"
	"github.com/Ametas/route-agent/internal/olcrtc"
	"github.com/Ametas/route-agent/internal/rear"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/singbox"
)

// Group F wiring lives in its own file so wire.go and wire_binaries.go only gain call lines (and
// no import that would collide with the other groups' edits of their import blocks).

// The real hook implementations group C's uploads call (internal/binaries).
var (
	_ binaries.RearHooks   = (*rear.Service)(nil)
	_ binaries.OlcrtcHooks = (*olcrtc.Service)(nil)
)

// newRear builds the rear service (spec F §1–5). It is stateless, so the handlers, the upload
// hooks, the WARP guard and group E (GetWarpKeyHealth → WarpKeyHealthReport, PullNodeWarnings →
// DetectEmptyRuleProviders) may each build their own from deps.
func newRear(d deps) *rear.Service {
	// The sing-box rear is checked exactly like the front config (validateSingBoxConfig);
	// ValidateConfig takes no lock and uses only these fields.
	sb := &singbox.Service{Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log}
	return &rear.Service{
		Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log,
		ValidateSingbox: sb.ValidateConfig, Locks: d.locks,
	}
}

// newOlcrtc builds the Olcrtc service (spec F §6).
func newOlcrtc(d deps) *olcrtc.Service {
	return &olcrtc.Service{Auth: d.auth, Runner: d.runner, FS: d.fs, Systemctl: d.systemctl, Cfg: d.cfg, Log: d.log, Locks: d.locks}
}

// wireGroupF assigns the group F handlers.
func wireGroupF(h *server.Handlers, d deps) {
	r := newRear(d)
	h.ConfigureRearSingbox = r.ConfigureRearSingbox
	h.GetRearConnections = r.GetRearConnections
	o := newOlcrtc(d)
	h.SyncOlcrtcInstance = o.SyncOlcrtcInstance
	h.DeleteOlcrtcInstance = o.DeleteOlcrtcInstance
	h.StreamOlcrtcEvents = o.StreamOlcrtcEvents
}

// startRearWarpGuard starts the WARP guard after bind (spec A §10.4, F §3): the first pass one
// interval later, passes never overlap.
func startRearWarpGuard(ctx context.Context, bg *lifecycle.Group, d deps) {
	r := newRear(d)
	loop := r.GuardLoop()
	bg.Start(ctx, loop)
	d.log.Info("Rear WARP guard started", "intervalMs", d.cfg.RearWarpGuardIntervalMs)
}
