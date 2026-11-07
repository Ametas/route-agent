package binaries

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// OlcrtcHooks is what the olcrtc-agent-srv upload needs from the Olcrtc domain (group F).
type OlcrtcHooks interface {
	// EnsureUnit provisions the olcrtc-agent-srv@.service template and reports whether it wrote.
	EnsureUnit(ctx context.Context) bool
	// RunningInstances lists the running olcrtc-agent-srv@* units.
	RunningInstances(ctx context.Context) ([]string, error)
}

// DefaultOlcrtc reproduces olcrtc.service.ts:97-145 and :441-462.
type DefaultOlcrtc struct {
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
}

// EnsureUnit writes the template unit on a byte difference and daemon-reloads.
func (d *DefaultOlcrtc) EnsureUnit(ctx context.Context) bool {
	return systemd.EnsureUnit(ctx, d.FS, d.Systemctl, d.Log, "olcrtc-agent-srv",
		d.Cfg.OlcrtcAgentSrvUnitFilePath, systemd.OlcrtcAgentSrvUnit(d.Cfg.OlcrtcAgentSrvBinaryPath, d.Cfg.OlcrtcAgentSrvConfigDir))
}

// RunningInstances replaces `systemctl list-units --type=service --state=running --no-legend
// 'olcrtc-agent-srv@*' | awk '{print $1}'`: the first field of each line; a leading status glyph
// (●, *) that systemctl may print is skipped instead of being taken as a unit name.
func (d *DefaultOlcrtc) RunningInstances(ctx context.Context) ([]string, error) {
	res, err := d.Systemctl.Run(ctx, "list-units", "--type=service", "--state=running", "--no-legend", "olcrtc-agent-srv@*")
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

// UploadOlcrtcAgentSrvBinary implements the RPC (spec C §8).
func (s *Service) UploadOlcrtcAgentSrvBinary(stream UploadStream) error {
	target := or(s.cfg.OlcrtcAgentSrvBinaryPath, "/usr/local/bin/olcrtc-agent-srv")
	return s.receive(stream, receiveSpec{
		rpc: "UploadOlcrtcAgentSrvBinary", dir: s.binaryTempDir(target), prefix: tempPrefix(target), suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: auth.InvalidSecretMessage, empty: msgNoData,
		failPrefix: prefixUploadFailed, lockKey: "bin:" + target,
	}, func(ctx context.Context, r received) (Result, error) { return s.applyOlcrtc(ctx, r, target) })
}

func (s *Service) applyOlcrtc(ctx context.Context, r received, target string) (Result, error) {
	if err := checkELF(r.path, s.Arch); err != nil {
		return Result{}, err
	}
	hadPrev, err := s.install(r.path, target)
	if err != nil {
		return Result{}, err
	}
	s.log.Info("Atomically updated olcrtc-agent-srv binary", "path", target, "version", r.version, "prev", hadPrev)

	// If the unit file just changed, running instances are NOT restarted (as in TS).
	if s.olcrtc.EnsureUnit(ctx) {
		return ok("olcrtc-agent-srv binary version " + r.version + " successfully updated"), nil
	}
	units, err := s.olcrtc.RunningInstances(ctx)
	if err != nil {
		s.log.Warn("Failed to enumerate running olcrtc-agent-srv instances", "err", err.Error())
	}
	if len(units) > 0 {
		out := s.activate(ctx, activation{
			target: target, label: "olcrtc-agent-srv binary " + r.version, what: strings.Join(units, ", "),
			hadPrev: hadPrev,
			restart: func(ctx context.Context) error {
				var errs []error
				for _, u := range units {
					if err := s.systemctl.Restart(ctx, u); err != nil {
						s.log.Warn("Failed to restart olcrtc-agent-srv instance", "unit", u, "err", err.Error())
						errs = append(errs, err)
					}
				}
				return errors.Join(errs...)
			},
		})
		if out.message != "" {
			return fail(prefixUploadFailed + out.message), nil
		}
	}
	return ok("olcrtc-agent-srv binary version " + r.version + " successfully updated"), nil
}
