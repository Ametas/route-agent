// Package olcrtc is the Go port of the Olcrtc side of group F (spec F §6): one systemd template
// instance per (user, node) — olcrtc-agent-srv@<user_short_uuid>.service with its config at
// <OLCRTC_AGENT_SRV_CONFIG_DIR>/<user_short_uuid>.yaml — and the live event feed read from the
// journal of those instances.
//
//   - SyncOlcrtcInstance / DeleteOlcrtcInstance (instance.go), under locks.Olcrtc (shared with
//     UploadOlcrtcAgentSrvBinary, group C);
//   - StreamOlcrtcEvents (events.go): `journalctl -f` per open stream; unlike TS the stream ends
//     with an error when journalctl cannot start or exits, so the orchestrator reconnects;
//   - the hooks the olcrtc-agent-srv upload needs (binaries.OlcrtcHooks): EnsureUnit,
//     RunningInstances. The boot-time unit provisioning is group A's pre-bind step (same
//     systemd.EnsureUnit + systemd.OlcrtcAgentSrvUnit).
package olcrtc

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Service holds the dependencies of the Olcrtc handlers and hooks.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger

	// Journal starts the journal tail of StreamOlcrtcEvents; nil means the real journalctl.
	Journal JournalTail

	// Locks is the agent-wide area lock registry; nil means a private one (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
}

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

// EnsureUnit provisions the olcrtc-agent-srv@.service template on a byte difference and
// daemon-reloads; it reports whether it wrote (olcrtc.service.ts:97-145).
func (s *Service) EnsureUnit(ctx context.Context) bool {
	return systemd.EnsureUnit(ctx, s.FS, s.Systemctl, s.Log, "olcrtc-agent-srv",
		s.Cfg.OlcrtcAgentSrvUnitFilePath, systemd.OlcrtcAgentSrvUnit(s.Cfg.OlcrtcAgentSrvBinaryPath, s.Cfg.OlcrtcAgentSrvConfigDir))
}

// RunningInstances replaces `systemctl list-units --type=service --state=running --no-legend
// 'olcrtc-agent-srv@*' | awk '{print $1}'`: the first field of each line, skipping a leading
// status glyph (●, *) instead of taking it for a unit name.
func (s *Service) RunningInstances(ctx context.Context) ([]string, error) {
	res, err := s.Systemctl.Run(ctx, "list-units", "--type=service", "--state=running", "--no-legend", "olcrtc-agent-srv@*")
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		for _, f := range strings.Fields(line) {
			if f == "●" || f == "*" {
				continue
			}
			units = append(units, f)
			break
		}
	}
	return units, nil
}
