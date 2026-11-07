// handlers.go: one Handlers field and one dispatch method per RPC of agentpb.EgressAgentServiceServer.

package server

import (
	"context"

	"google.golang.org/grpc"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// Handlers holds one function per RPC. A nil field answers codes.Unimplemented via the stub in
// stubs.go. Groups B–F fill their fields from their own packages (see cmd/route-agent/wire.go);
// nothing else in this package changes when a group lands.
type Handlers struct {
	// Group A (docs/go-rewrite/spec/a-*.md)
	SelfUpdate            func(context.Context, *agentpb.SelfUpdatePayload) (*agentpb.SelfUpdateResponse, error)
	GetAgentInfo          func(context.Context, *agentpb.AgentInfoRequest) (*agentpb.AgentInfoResponse, error)
	GetManagedCertificate func(context.Context, *agentpb.CertificateRequest) (*agentpb.CertificateResponse, error)
	// Group B (docs/go-rewrite/spec/b-*.md)
	ApplyConfig           func(context.Context, *agentpb.ConfigPayload) (*agentpb.ConfigResponse, error)
	ConfigureCaddy        func(context.Context, *agentpb.CaddyConfigPayload) (*agentpb.CaddyConfigResponse, error)
	GetSingBoxUserTraffic func(context.Context, *agentpb.SingBoxUserTrafficRequest) (*agentpb.SingBoxUserTrafficResponse, error)
	GetSingBoxConnections func(context.Context, *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error)
	// Group C (docs/go-rewrite/spec/c-*.md)
	UpgradeSingbox             func(context.Context, *agentpb.UpgradePayload) (*agentpb.UpgradeResponse, error)
	UploadSingboxBinary        func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadAwgToolsBinary       func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadAwgGoBinary          func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadCaddyBinary          func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadMihomoBinary         func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadRearRuleSet          func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadOlcrtcAgentSrvBinary func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	UploadMeshAwgKernelSource  func(grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error
	// Group D (docs/go-rewrite/spec/d-*.md)
	GetAwgObservation    func(context.Context, *agentpb.AwgObservationRequest) (*agentpb.AwgObservationResponse, error)
	ConfigureAwg         func(context.Context, *agentpb.AwgConfigPayload) (*agentpb.AwgConfigResponse, error)
	ConfigureMeshTunnel  func(context.Context, *agentpb.MeshTunnelConfigPayload) (*agentpb.MeshTunnelConfigResponse, error)
	ManageFirewall       func(context.Context, *agentpb.FirewallPayload) (*agentpb.FirewallResponse, error)
	ApplyTrafficThrottle func(context.Context, *agentpb.TrafficThrottleRequest) (*agentpb.TrafficThrottleResponse, error)
	// Group E (docs/go-rewrite/spec/e-*.md)
	StreamTelemetry      func(*agentpb.TelemetryRequest, grpc.ServerStreamingServer[agentpb.TelemetryResponse]) error
	GetTelemetry         func(context.Context, *agentpb.TelemetryRequest) (*agentpb.TelemetryResponse, error)
	RunNetworkDiagnostic func(context.Context, *agentpb.NetworkDiagnosticRequest) (*agentpb.NetworkDiagnosticResponse, error)
	GetWarpKeyHealth     func(context.Context, *agentpb.WarpKeyHealthRequest) (*agentpb.WarpKeyHealthResponse, error)
	PullNodeWarnings     func(context.Context, *agentpb.NodeWarningsRequest) (*agentpb.NodeWarningsResponse, error)
	// Group F (docs/go-rewrite/spec/f-*.md)
	SyncOlcrtcInstance   func(context.Context, *agentpb.OlcrtcInstanceConfigPayload) (*agentpb.OlcrtcInstanceConfigResponse, error)
	DeleteOlcrtcInstance func(context.Context, *agentpb.OlcrtcInstanceDeletePayload) (*agentpb.OlcrtcInstanceConfigResponse, error)
	StreamOlcrtcEvents   func(*agentpb.OlcrtcEventsRequest, grpc.ServerStreamingServer[agentpb.OlcrtcEvent]) error
	GetRearConnections   func(context.Context, *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error)
	ConfigureRearSingbox func(context.Context, *agentpb.RearSingboxConfigPayload) (*agentpb.RearSingboxConfigResponse, error)
}

// service adapts Handlers to the generated interface. Unimplemented is embedded one level down
// so the Handlers-based methods below take precedence and mustEmbed… is still satisfied.
type service struct {
	unimplemented
	h *Handlers
}

type unimplemented struct {
	agentpb.UnimplementedEgressAgentServiceServer
}

func (s *service) ApplyConfig(ctx context.Context, req *agentpb.ConfigPayload) (*agentpb.ConfigResponse, error) {
	if s.h.ApplyConfig == nil {
		return nil, unimplementedRPC("ApplyConfig")
	}
	return s.h.ApplyConfig(ctx, req)
}

func (s *service) StreamTelemetry(req *agentpb.TelemetryRequest, stream grpc.ServerStreamingServer[agentpb.TelemetryResponse]) error {
	if s.h.StreamTelemetry == nil {
		return unimplementedRPC("StreamTelemetry")
	}
	return s.h.StreamTelemetry(req, stream)
}

func (s *service) GetTelemetry(ctx context.Context, req *agentpb.TelemetryRequest) (*agentpb.TelemetryResponse, error) {
	if s.h.GetTelemetry == nil {
		return nil, unimplementedRPC("GetTelemetry")
	}
	return s.h.GetTelemetry(ctx, req)
}

func (s *service) UpgradeSingbox(ctx context.Context, req *agentpb.UpgradePayload) (*agentpb.UpgradeResponse, error) {
	if s.h.UpgradeSingbox == nil {
		return nil, unimplementedRPC("UpgradeSingbox")
	}
	return s.h.UpgradeSingbox(ctx, req)
}

func (s *service) SyncOlcrtcInstance(ctx context.Context, req *agentpb.OlcrtcInstanceConfigPayload) (*agentpb.OlcrtcInstanceConfigResponse, error) {
	if s.h.SyncOlcrtcInstance == nil {
		return nil, unimplementedRPC("SyncOlcrtcInstance")
	}
	return s.h.SyncOlcrtcInstance(ctx, req)
}

func (s *service) DeleteOlcrtcInstance(ctx context.Context, req *agentpb.OlcrtcInstanceDeletePayload) (*agentpb.OlcrtcInstanceConfigResponse, error) {
	if s.h.DeleteOlcrtcInstance == nil {
		return nil, unimplementedRPC("DeleteOlcrtcInstance")
	}
	return s.h.DeleteOlcrtcInstance(ctx, req)
}

func (s *service) StreamOlcrtcEvents(req *agentpb.OlcrtcEventsRequest, stream grpc.ServerStreamingServer[agentpb.OlcrtcEvent]) error {
	if s.h.StreamOlcrtcEvents == nil {
		return unimplementedRPC("StreamOlcrtcEvents")
	}
	return s.h.StreamOlcrtcEvents(req, stream)
}

func (s *service) SelfUpdate(ctx context.Context, req *agentpb.SelfUpdatePayload) (*agentpb.SelfUpdateResponse, error) {
	if s.h.SelfUpdate == nil {
		return nil, unimplementedRPC("SelfUpdate")
	}
	return s.h.SelfUpdate(ctx, req)
}

func (s *service) UploadSingboxBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadSingboxBinary == nil {
		return unimplementedRPC("UploadSingboxBinary")
	}
	return s.h.UploadSingboxBinary(stream)
}

func (s *service) UploadAwgToolsBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadAwgToolsBinary == nil {
		return unimplementedRPC("UploadAwgToolsBinary")
	}
	return s.h.UploadAwgToolsBinary(stream)
}

func (s *service) UploadAwgGoBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadAwgGoBinary == nil {
		return unimplementedRPC("UploadAwgGoBinary")
	}
	return s.h.UploadAwgGoBinary(stream)
}

func (s *service) UploadCaddyBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadCaddyBinary == nil {
		return unimplementedRPC("UploadCaddyBinary")
	}
	return s.h.UploadCaddyBinary(stream)
}

func (s *service) UploadMihomoBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadMihomoBinary == nil {
		return unimplementedRPC("UploadMihomoBinary")
	}
	return s.h.UploadMihomoBinary(stream)
}

func (s *service) UploadRearRuleSet(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadRearRuleSet == nil {
		return unimplementedRPC("UploadRearRuleSet")
	}
	return s.h.UploadRearRuleSet(stream)
}

func (s *service) GetAwgObservation(ctx context.Context, req *agentpb.AwgObservationRequest) (*agentpb.AwgObservationResponse, error) {
	if s.h.GetAwgObservation == nil {
		return nil, unimplementedRPC("GetAwgObservation")
	}
	return s.h.GetAwgObservation(ctx, req)
}

func (s *service) ConfigureAwg(ctx context.Context, req *agentpb.AwgConfigPayload) (*agentpb.AwgConfigResponse, error) {
	if s.h.ConfigureAwg == nil {
		return nil, unimplementedRPC("ConfigureAwg")
	}
	return s.h.ConfigureAwg(ctx, req)
}

func (s *service) ConfigureCaddy(ctx context.Context, req *agentpb.CaddyConfigPayload) (*agentpb.CaddyConfigResponse, error) {
	if s.h.ConfigureCaddy == nil {
		return nil, unimplementedRPC("ConfigureCaddy")
	}
	return s.h.ConfigureCaddy(ctx, req)
}

func (s *service) UploadOlcrtcAgentSrvBinary(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadOlcrtcAgentSrvBinary == nil {
		return unimplementedRPC("UploadOlcrtcAgentSrvBinary")
	}
	return s.h.UploadOlcrtcAgentSrvBinary(stream)
}

func (s *service) UploadMeshAwgKernelSource(stream grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]) error {
	if s.h.UploadMeshAwgKernelSource == nil {
		return unimplementedRPC("UploadMeshAwgKernelSource")
	}
	return s.h.UploadMeshAwgKernelSource(stream)
}

func (s *service) ConfigureMeshTunnel(ctx context.Context, req *agentpb.MeshTunnelConfigPayload) (*agentpb.MeshTunnelConfigResponse, error) {
	if s.h.ConfigureMeshTunnel == nil {
		return nil, unimplementedRPC("ConfigureMeshTunnel")
	}
	return s.h.ConfigureMeshTunnel(ctx, req)
}

func (s *service) ManageFirewall(ctx context.Context, req *agentpb.FirewallPayload) (*agentpb.FirewallResponse, error) {
	if s.h.ManageFirewall == nil {
		return nil, unimplementedRPC("ManageFirewall")
	}
	return s.h.ManageFirewall(ctx, req)
}

func (s *service) GetAgentInfo(ctx context.Context, req *agentpb.AgentInfoRequest) (*agentpb.AgentInfoResponse, error) {
	if s.h.GetAgentInfo == nil {
		return nil, unimplementedRPC("GetAgentInfo")
	}
	return s.h.GetAgentInfo(ctx, req)
}

func (s *service) RunNetworkDiagnostic(ctx context.Context, req *agentpb.NetworkDiagnosticRequest) (*agentpb.NetworkDiagnosticResponse, error) {
	if s.h.RunNetworkDiagnostic == nil {
		return nil, unimplementedRPC("RunNetworkDiagnostic")
	}
	return s.h.RunNetworkDiagnostic(ctx, req)
}

func (s *service) GetManagedCertificate(ctx context.Context, req *agentpb.CertificateRequest) (*agentpb.CertificateResponse, error) {
	if s.h.GetManagedCertificate == nil {
		return nil, unimplementedRPC("GetManagedCertificate")
	}
	return s.h.GetManagedCertificate(ctx, req)
}

func (s *service) GetSingBoxUserTraffic(ctx context.Context, req *agentpb.SingBoxUserTrafficRequest) (*agentpb.SingBoxUserTrafficResponse, error) {
	if s.h.GetSingBoxUserTraffic == nil {
		return nil, unimplementedRPC("GetSingBoxUserTraffic")
	}
	return s.h.GetSingBoxUserTraffic(ctx, req)
}

func (s *service) GetSingBoxConnections(ctx context.Context, req *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error) {
	if s.h.GetSingBoxConnections == nil {
		return nil, unimplementedRPC("GetSingBoxConnections")
	}
	return s.h.GetSingBoxConnections(ctx, req)
}

func (s *service) GetRearConnections(ctx context.Context, req *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error) {
	if s.h.GetRearConnections == nil {
		return nil, unimplementedRPC("GetRearConnections")
	}
	return s.h.GetRearConnections(ctx, req)
}

func (s *service) ApplyTrafficThrottle(ctx context.Context, req *agentpb.TrafficThrottleRequest) (*agentpb.TrafficThrottleResponse, error) {
	if s.h.ApplyTrafficThrottle == nil {
		return nil, unimplementedRPC("ApplyTrafficThrottle")
	}
	return s.h.ApplyTrafficThrottle(ctx, req)
}

func (s *service) ConfigureRearSingbox(ctx context.Context, req *agentpb.RearSingboxConfigPayload) (*agentpb.RearSingboxConfigResponse, error) {
	if s.h.ConfigureRearSingbox == nil {
		return nil, unimplementedRPC("ConfigureRearSingbox")
	}
	return s.h.ConfigureRearSingbox(ctx, req)
}

func (s *service) GetWarpKeyHealth(ctx context.Context, req *agentpb.WarpKeyHealthRequest) (*agentpb.WarpKeyHealthResponse, error) {
	if s.h.GetWarpKeyHealth == nil {
		return nil, unimplementedRPC("GetWarpKeyHealth")
	}
	return s.h.GetWarpKeyHealth(ctx, req)
}

func (s *service) PullNodeWarnings(ctx context.Context, req *agentpb.NodeWarningsRequest) (*agentpb.NodeWarningsResponse, error) {
	if s.h.PullNodeWarnings == nil {
		return nil, unimplementedRPC("PullNodeWarnings")
	}
	return s.h.PullNodeWarnings(ctx, req)
}

var _ agentpb.EgressAgentServiceServer = (*service)(nil)
var _ = grpc.SupportPackageIsVersion9
