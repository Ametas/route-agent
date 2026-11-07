package singbox

import (
	"context"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const msgNoClashConnections = "clash_api not configured on this node"

// GetSingBoxConnections: auth M|B; stateless snapshot of the live Clash API connections
// (singboxConnections.service.ts:21-46). closed is always false: there is no closed buffer.
func (s *Service) GetSingBoxConnections(ctx context.Context, req *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetSingBoxConnections request blocked")
		return &agentpb.SingBoxConnectionsResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	ep, err := clashapi.ReadEndpoint(s.FS, s.configPath())
	if err != nil {
		s.Log.Debug("Could not read live sing-box config to resolve the clash_api endpoint", "err", err.Error())
	}
	if ep == nil {
		return &agentpb.SingBoxConnectionsResponse{Success: true, Message: msgNoClashConnections}, nil
	}
	records, err := s.Clash.Fetch(ctx, *ep)
	if err != nil {
		s.Log.Warn("Failed to fetch sing-box connections", "err", err.Error())
		return &agentpb.SingBoxConnectionsResponse{Success: false, Message: err.Error()}, nil
	}
	return &agentpb.SingBoxConnectionsResponse{Success: true, Message: "OK", Connections: clashapi.ToProto(records)}, nil
}
