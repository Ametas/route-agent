package rear

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path"
	"strconv"
	"time"

	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Response strings the orchestrator reads or shows (spec F §2, orch nodeOps/actions.ts:263-277).
const (
	msgStopped = "Rear instance stopped and removed."
	// msgAlreadyCurrent keeps the literal "sing-box" for both cores (spec F R7): tests and logs
	// match on it.
	msgAlreadyCurrent    = "Rear sing-box configuration already current — nothing to write or reload."
	skippedCheckFailed   = "check_failed"
	internalErrorPrefix  = "Internal Agent Error: "
	notInstalledTemplate = " is not installed on this node — nothing to run a rear instance from."
)

// Serialize is the only on-disk form of a rear config: JSON.stringify(cfg, null, 2), byte for
// byte (jsjson), so the first Go push on a node written by TS still hits "already current".
func Serialize(cfg any) string { return jsjson.Stringify(cfg, "  ") }

// ConfigureRearSingbox implements the RPC (spec F §2): auth M|B; the whole apply under
// locks.Rear, waited for with the client's context (an orchestrator that gave up does not get its
// push applied later) and then run detached, every command with its own timeout.
func (s *Service) ConfigureRearSingbox(ctx context.Context, req *agentpb.RearSingboxConfigPayload) (*agentpb.RearSingboxConfigResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized ConfigureRearSingbox request blocked")
		return &agentpb.RearSingboxConfigResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	held, unlock, err := s.locks().Lock(ctx, locks.Rear)
	if err != nil {
		if ctx.Err() != nil {
			s.Log.Warn("ConfigureRearSingbox abandoned while waiting for the rear lock", "err", err.Error())
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return &agentpb.RearSingboxConfigResponse{Success: false, Message: internalErrorPrefix + err.Error()}, nil
	}
	defer unlock()
	resp, err := s.configure(context.WithoutCancel(held), req)
	if err != nil {
		s.Log.Error("Failed to process ConfigureRearSingbox RPC pipeline", "err", err.Error())
		return &agentpb.RearSingboxConfigResponse{Success: false, Message: internalErrorPrefix + err.Error()}, nil
	}
	return resp, nil
}

func (s *Service) configure(ctx context.Context, req *agentpb.RearSingboxConfigPayload) (*agentpb.RearSingboxConfigResponse, error) {
	core := s.ResolveCore(req.GetCore())
	previous := s.otherCore(core)

	if !req.GetEnabled() {
		// Both cores: "rear off" is a state of the node, not of the core the orchestrator thought
		// current; the configs go too (they hold the WARP private keys).
		for _, c := range []Core{core, previous} {
			s.stop(ctx, c)
			_ = os.Remove(s.FS.Path(c.ConfigPath))
			_ = os.Remove(s.FS.Path(c.UnitPath))
		}
		s.Log.Info("Rear instance stopped and removed (both cores)")
		return &agentpb.RearSingboxConfigResponse{Success: true, Message: msgStopped}, nil
	}

	if !s.exists(core.BinaryPath) {
		s.Log.Warn("Rear requested but its core is not installed", "binary", core.BinaryPath, "core", string(core.ID))
		return &agentpb.RearSingboxConfigResponse{
			Success: false, Message: core.Label + notInstalledTemplate, SkippedReason: string(core.ID) + "_not_installed",
		}, nil
	}

	cfg, err := jsjson.Parse(req.GetConfigJson())
	if err != nil {
		return nil, err
	}

	// Core switch. The previous core must go before the new one comes up (both bind loopback
	// 29000/29001), but only AFTER the new config passed its check: TS removed the previous core
	// first, so a check_failed left the node without any rear (spec F R1, architecture §6 item 8).
	validated := false
	if s.exists(previous.UnitPath) {
		if resp := s.check(ctx, core, cfg); resp != nil {
			return resp, nil
		}
		validated = true
		s.Log.Info("Switching rear core — removing the previous one", "from", string(previous.ID), "to", string(core.ID))
		s.stop(ctx, previous)
		_ = os.Remove(s.FS.Path(previous.UnitPath))
		_ = os.Remove(s.FS.Path(previous.ConfigPath))
		_ = s.Systemctl.DaemonReload(ctx)
	}

	// A rewritten unit means a changed ExecStart, which SIGHUP cannot pick up: restart instead.
	unitChanged := systemd.EnsureUnit(ctx, s.FS, s.Systemctl, s.Log, "rear "+core.Label, core.UnitPath, core.UnitContent())
	serialized := Serialize(cfg)
	if !unitChanged && s.configMatches(core, serialized) && s.running(ctx, core) {
		s.Log.Info("Rear config unchanged and instance is up — skipping write and reload", "core", string(core.ID))
		return &agentpb.RearSingboxConfigResponse{Success: true, Message: msgAlreadyCurrent, Running: true}, nil
	}

	if !validated {
		if resp := s.check(ctx, core, cfg); resp != nil {
			return resp, nil
		}
	}

	if err := s.FS.WriteFile(core.ConfigPath, []byte(serialized), fsx.WriteOptions{Mode: configMode}); err != nil {
		return nil, err
	}
	s.removeLegacyConfig()

	unit := core.UnitName()
	if err := s.Systemctl.Enable(ctx, "--now", unit); err != nil {
		return nil, err
	}
	if unitChanged {
		err = s.Systemctl.Restart(ctx, unit)
	} else {
		// Reload, never restart: ExecReload checks the config and signals only on success, so a
		// bad config leaves the rear running (rearReload.test.ts).
		err = s.Systemctl.Reload(ctx, unit)
	}
	if err != nil {
		return nil, err
	}

	running := s.running(ctx, core)
	s.Log.Info("Rear configuration applied", "running", running, "core", string(core.ID))
	return &agentpb.RearSingboxConfigResponse{
		Success: true, Message: "Rear " + core.Label + " configuration validated, applied and (re)started.", Running: running,
	}, nil
}

// check validates cfg with the core's own binary; nil when valid, else the check_failed answer.
func (s *Service) check(ctx context.Context, core Core, cfg any) *agentpb.RearSingboxConfigResponse {
	ok, errText := s.Validate(ctx, core, cfg)
	if ok {
		return nil
	}
	s.Log.Error("Rear config rejected by its own core", "err", errText, "core", string(core.ID))
	return &agentpb.RearSingboxConfigResponse{
		Success:       false,
		Message:       "Rejected by Node Agent: invalid rear " + core.Label + " syntax. Error: " + errText,
		Running:       s.running(ctx, core),
		SkippedReason: skippedCheckFailed,
	}
}

// Validate checks cfg with the binary that will run it: sing-box via ValidateSingbox (error text
// not truncated), mihomo by validateMihomoConfig — `<mihomo> -t -d <workdir> -f <temp>` on a temp
// file in the real work dir (rule-provider paths resolve, and a rejected config is never what a
// restarting process reads), error text cut to 1000 characters.
func (s *Service) Validate(ctx context.Context, core Core, cfg any) (bool, string) {
	if core.ID != Mihomo {
		if s.ValidateSingbox == nil {
			return false, "sing-box validator is not configured"
		}
		return s.ValidateSingbox(ctx, cfg)
	}
	wd := core.WorkDir()
	check := path.Join(wd, ".rear.check_"+uniqueID()+".yaml")
	defer func() { _ = os.Remove(s.FS.Path(check)) }()
	if err := s.FS.WriteFile(check, []byte(Serialize(cfg)), fsx.WriteOptions{Mode: configMode}); err != nil {
		return false, jsSlice(err.Error(), 1000)
	}
	_, err := s.Runner.Run(ctx, runner.Cmd{Name: core.BinaryPath, Args: []string{"-t", "-d", wd, "-f", check}, Timeout: mihomoCheckTimeout})
	if err != nil {
		text := runner.StderrOrMessage(err)
		if text == "" {
			text = "unknown error"
		}
		return false, jsSlice(text, 1000)
	}
	return true, ""
}

func (s *Service) configMatches(core Core, serialized string) bool {
	cur, err := s.FS.ReadFile(core.ConfigPath)
	return err == nil && string(cur) == serialized
}

// stop is stopRear: `systemctl disable --now <unit>`, errors ignored.
func (s *Service) stop(ctx context.Context, c Core) {
	_ = s.Systemctl.Disable(ctx, "--now", c.UnitName())
}

// removeLegacyConfig removes /etc/sing-box/rear.json after a successful write, unless the
// sing-box rear path is configured to be that very file.
func (s *Service) removeLegacyConfig() {
	if s.SingboxCore().ConfigPath == LegacyConfigPath || !s.exists(LegacyConfigPath) {
		return
	}
	if err := os.Remove(s.FS.Path(LegacyConfigPath)); err != nil {
		s.Log.Warn("Failed to remove the legacy rear config", "err", err.Error(), "path", LegacyConfigPath)
		return
	}
	s.Log.Info("Removed the legacy rear config from the shared sing-box directory", "path", LegacyConfigPath)
}

func uniqueID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + hex.EncodeToString(b[:])
}

// jsSlice is String.prototype.slice(0, n): n UTF-16 code units, never splitting a character.
func jsSlice(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}
