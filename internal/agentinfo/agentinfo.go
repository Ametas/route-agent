// Package agentinfo implements GetAgentInfo (src/services/system.service.ts:62-89, spec A §7).
//
// Deliberately unauthenticated: it is the orchestrator's read-only pre-flight, used also for
// desynchronized nodes; the secret in the body and in metadata is ignored. It never returns a
// gRPC error: on an internal failure it answers status OK with empty hash and version (the
// orchestrator then reports "mismatch"). Code 12 from this method would mean "very old agent".
package agentinfo

import (
	"context"
	"log/slog"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/contract"
)

// Service answers GetAgentInfo.
type Service struct {
	// Version returns the agent version (cached by the caller, e.g. version.Resolver.Get).
	Version func(context.Context) string
	// Hash returns the proto contract hash; nil means contract.Hash.
	Hash func() string
	Log  *slog.Logger
}

// GetAgentInfo is the RPC handler.
func (s *Service) GetAgentInfo(ctx context.Context, _ *agentpb.AgentInfoRequest) (resp *agentpb.AgentInfoResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("Failed to process GetAgentInfo RPC", "err", r)
			resp, err = &agentpb.AgentInfoResponse{ProtoContractSource: contract.Source}, nil
		}
	}()
	hash := s.Hash
	if hash == nil {
		hash = contract.Hash
	}
	return &agentpb.AgentInfoResponse{
		ProtoContractHash:   hash(),
		AgentVersion:        s.Version(ctx),
		ProtoContractSource: contract.Source,
	}, nil
}
