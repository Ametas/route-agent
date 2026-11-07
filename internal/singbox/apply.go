package singbox

import (
	"context"
	"strconv"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// ApplyConfig response texts (spec B §1.5). The orchestrator only reads success; tests and
// operators match the texts.
const (
	MsgAlreadyCurrent = "Sing-box configuration already current — nothing to write or reload."
	MsgApplied        = "Configuration successfully validated, applied, and sing-box reloaded via gRPC channel."
	msgRejectedPrefix = "Rejected by Node Agent: Invalid sing-box syntax. Error: "
	msgInternalPrefix = "Internal Agent Error: "
)

func hotSwapMessage(n int) string {
	return "Applied " + strconv.Itoa(n) + " inbound roster change(s) through the live users-api — no reload, sessions kept."
}

// ApplyConfig: auth by metadata only; status OK on every path (spec B §0.3, §1.3).
func (s *Service) ApplyConfig(ctx context.Context, req *agentpb.ConfigPayload) (*agentpb.ConfigResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized gRPC execution blocked")
		return &agentpb.ConfigResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	// The orchestrator's 60 s deadline must not cut a write+reload in half: TS kept working after
	// the client gave up, and every step here has its own timeout.
	msg, ok := s.apply(context.WithoutCancel(ctx), req.GetConfigJson(), req.GetForceReload())
	return &agentpb.ConfigResponse{Success: ok, Message: msg}, nil
}

func (s *Service) internalError(err error) (string, bool) {
	s.Log.Error("Failed to process ApplyConfig RPC pipeline", "err", err.Error())
	return msgInternalPrefix + err.Error(), false
}

func (s *Service) apply(ctx context.Context, raw string, forceReload bool) (string, bool) {
	parsed, err := jsjson.Parse(raw)
	if err != nil {
		return s.internalError(err)
	}

	// users-api is injected HERE (only the agent knows whether the fork is installed), and
	// before the on-disk comparison, which would otherwise never match.
	isFork := s.IsFork(ctx)
	cfg := parsed
	if isFork {
		if cfg, err = WithUsersAPIService(parsed); err != nil {
			return s.internalError(err)
		}
	}
	coreRunning := s.UnitActive(ctx)
	imageStale := s.ImageStale(ctx)
	serialized := Serialize(cfg)

	// A push that changes nothing does nothing: every sing-box reload drops ALL sessions. Three
	// conditions: same bytes, unit alive (or a dead core would never be started again), image
	// not replaced (or an old process that rejected the new config would stay "current"
	// forever). success:true matters: PurgeWorker waits for every node to confirm.
	if s.configMatches(serialized) && coreRunning && !imageStale {
		s.Log.Info("Sing-box config unchanged and core is up — skipping write, validate and reload")
		return MsgAlreadyCurrent, true
	}

	if ok, errText := s.validate(ctx, serialized); !ok {
		return msgRejectedPrefix + errText, false
	}

	s.syncEgressFirewall(ctx, cfg)

	// Hot roster swap: only the fork has the socket; force_reload (access revoked — a QUIC
	// session would otherwise outlive the revocation) and a dead or stale core take the reload.
	written, hadPrevious := false, false
	if isFork && !forceReload && coreRunning && !imageStale {
		msg, wrote, had, err := s.tryHotRosterSwap(ctx, cfg, serialized)
		if err != nil {
			return s.internalError(err)
		}
		if msg != "" {
			return msg, true
		}
		written, hadPrevious = wrote, had
	}

	// Write (unless the failed hot swap already wrote these exact bytes — writing again would
	// copy the NEW file over config.json.bak and lose the rollback target, spec B R4) and reload.
	if !written {
		if hadPrevious, err = s.writeConfig(serialized, true); err != nil {
			return s.internalError(err)
		}
	}
	if err := s.reload(ctx, hadPrevious); err != nil {
		return s.internalError(err)
	}
	return MsgApplied, true
}

// tryHotRosterSwap returns the success text, or "" to fall back to the reload path, plus whether
// it already wrote the config (and whether one existed before). Any doubt falls back: the
// reload rebuilds from the written file, so a partial socket application converges.
func (s *Service) tryHotRosterSwap(ctx context.Context, cfg any, serialized string) (msg string, wrote, hadPrevious bool, err error) {
	onDisk := s.readConfigOnDisk()
	if onDisk == nil {
		s.Log.Warn("Горячая замена невозможна: конфиг на диске не прочитался — уходим в перезагрузку")
		return "", false, false, nil
	}
	plan, ok, err := PlanRosterUpdate(cfg, onDisk)
	if err != nil {
		return "", false, false, err
	}
	if !ok {
		// Never silent again (2026-09-21): name the reason, never the values.
		s.Log.Warn("Горячая замена невозможна — уходим в полную перезагрузку, сессии будут разорваны",
			"blocker", ExplainHotSwapBlocker(cfg, onDisk))
		return "", false, false, nil
	}
	// The file goes first: if the process dies between the two steps, the disk must hold the
	// roster we consider current.
	had, werr := s.writeConfig(serialized, true)
	if werr != nil {
		s.Log.Warn("Hot roster swap failed — falling back to a full reload", "err", werr.Error())
		return "", false, false, nil
	}
	if aerr := s.UsersAPI.Apply(ctx, s.FS, s.Log, plan); aerr != nil {
		s.Log.Warn("Hot roster swap failed — falling back to a full reload", "err", aerr.Error())
		return "", true, had, nil
	}
	return hotSwapMessage(len(plan)), true, had, nil
}
