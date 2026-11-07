// Package auth checks the orchestrator secret: the Go port of src/middleware/auth.ts and of the
// auth matrix in spec A §4.3.
//
// Every RPC handler calls exactly one helper, chosen by the RPC's Mode (ModeOf):
//
//	ModeMetadata         v.Metadata(ctx)                      ApplyConfig, SelfUpdate, …
//	ModeMetadataOrBody   v.MetadataOrBody(ctx, req.GetOrchestratorSecret())
//	                                                          (pass "" when the request message
//	                                                          has no orchestrator_secret field)
//	ModeMetadataOrChunk  g := v.Stream(ctx); per chunk: if !g.Admit(chunk.GetOrchestratorSecret()) → reject
//	ModeNone             nothing                              GetAgentInfo
//
// The failure SHAPE stays with the handler, because it differs per RPC (spec A §4.3): most
// unary RPCs answer status OK with {success:false, message: InvalidSecretMessage} plus empty
// repeated fields; server-streaming RPCs return Unauthenticated(); client-streaming uploads answer
// status OK with success:false (deterministically — spec A §4.3 "Go recommendation").
package auth

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// MetadataKey is the header the orchestrator always sends (orch GrpcConnectionManager.ts:329-333).
const MetadataKey = "x-orchestrator-secret"

// InvalidSecretMessage is the exact failure text; repository tests compare it.
const InvalidSecretMessage = "Invalid orchestrator secret token."

// Mode is how an RPC authenticates (spec A §4.3).
type Mode int

const (
	// ModeNone: no authentication (GetAgentInfo only).
	ModeNone Mode = iota
	// ModeMetadata (M): metadata only; a body secret is ignored.
	ModeMetadata
	// ModeMetadataOrBody (M|B): metadata, else the request's orchestrator_secret.
	ModeMetadataOrBody
	// ModeMetadataOrChunk (M|C): client stream; metadata, else the secret in the FIRST chunk.
	ModeMetadataOrChunk
)

func (m Mode) String() string {
	switch m {
	case ModeMetadata:
		return "M"
	case ModeMetadataOrBody:
		return "M|B"
	case ModeMetadataOrChunk:
		return "M|C"
	}
	return "none"
}

// modes is the matrix of spec A §4.3, keyed by RPC method name.
var modes = map[string]Mode{
	"ApplyConfig":                ModeMetadata,
	"ConfigureAwg":               ModeMetadata,
	"ConfigureCaddy":             ModeMetadataOrBody,
	"GetManagedCertificate":      ModeMetadataOrBody,
	"ManageFirewall":             ModeMetadata,
	"SelfUpdate":                 ModeMetadata,
	"UpgradeSingbox":             ModeMetadata,
	"SyncOlcrtcInstance":         ModeMetadata,
	"DeleteOlcrtcInstance":       ModeMetadata,
	"ConfigureMeshTunnel":        ModeMetadata,
	"ConfigureRearSingbox":       ModeMetadataOrBody,
	"ApplyTrafficThrottle":       ModeMetadataOrBody,
	"GetSingBoxUserTraffic":      ModeMetadataOrBody,
	"GetSingBoxConnections":      ModeMetadataOrBody,
	"GetRearConnections":         ModeMetadataOrBody,
	"GetWarpKeyHealth":           ModeMetadataOrBody,
	"PullNodeWarnings":           ModeMetadataOrBody,
	"GetAwgObservation":          ModeMetadataOrBody,
	"RunNetworkDiagnostic":       ModeMetadataOrBody,
	"GetTelemetry":               ModeMetadataOrBody,
	"StreamTelemetry":            ModeMetadataOrBody,
	"StreamOlcrtcEvents":         ModeMetadataOrBody,
	"UploadSingboxBinary":        ModeMetadataOrChunk,
	"UploadAwgToolsBinary":       ModeMetadataOrChunk,
	"UploadAwgGoBinary":          ModeMetadataOrChunk,
	"UploadCaddyBinary":          ModeMetadataOrChunk,
	"UploadOlcrtcAgentSrvBinary": ModeMetadataOrChunk,
	"UploadMeshAwgKernelSource":  ModeMetadataOrChunk,
	"UploadMihomoBinary":         ModeMetadataOrChunk,
	"UploadRearRuleSet":          ModeMetadataOrChunk,
	"GetAgentInfo":               ModeNone,
}

// ModeOf returns the auth mode of an RPC and whether the method is known.
func ModeOf(method string) (Mode, bool) {
	m, ok := modes[method]
	return m, ok
}

// Methods returns every method in the matrix (31).
func Methods() []string {
	out := make([]string, 0, len(modes))
	for k := range modes {
		out = append(out, k)
	}
	return out
}

// Verifier holds the node secret (EGRESS_CONTROL_SECRET).
type Verifier struct {
	secret []byte
}

// New returns a Verifier. An empty secret is accepted and then rejects every call (fail closed).
func New(secret string) *Verifier { return &Verifier{secret: []byte(secret)} }

// Verify is verifySecret: false for an empty token, an empty expected secret or a length
// mismatch; otherwise a constant-time comparison of the UTF-8 bytes.
func (v *Verifier) Verify(token string) bool {
	if token == "" || len(v.secret) == 0 || len(token) != len(v.secret) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), v.secret) == 1
}

// FromMetadata is extractSecretFromMetadata: the first x-orchestrator-secret value, or "".
func FromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(MetadataKey)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// Metadata is mode M.
func (v *Verifier) Metadata(ctx context.Context) bool { return v.Verify(FromMetadata(ctx)) }

// MetadataOrBody is mode M|B (authenticateCall): metadata first, then the body secret.
func (v *Verifier) MetadataOrBody(ctx context.Context, bodySecret string) bool {
	return v.Metadata(ctx) || v.Verify(bodySecret)
}

// Stream starts mode M|C for one client stream.
func (v *Verifier) Stream(ctx context.Context) *StreamGate {
	return &StreamGate{v: v, verified: v.Metadata(ctx)}
}

// StreamGate implements the per-stream rule of binaryReceiver.ts:61-102: verified by metadata at
// the start, or by the secret of the first chunk; once verified it stays verified (later chunk
// secrets are not re-checked); a first chunk without a valid secret rejects the stream, and a
// later chunk cannot rescue it.
type StreamGate struct {
	v        *Verifier
	verified bool
	rejected bool
}

// Admit is called for every received chunk with that chunk's orchestrator_secret. It returns
// false when the stream must be rejected (the caller answers success:false and stops reading).
func (g *StreamGate) Admit(chunkSecret string) bool {
	if g.rejected {
		return false
	}
	if !g.verified && g.v.Verify(chunkSecret) {
		g.verified = true
	}
	if !g.verified {
		g.rejected = true
	}
	return g.verified
}

// Verified reports the state, for the end-of-stream check when no chunk arrived at all.
func (g *StreamGate) Verified() bool { return g.verified && !g.rejected }

// Unauthenticated is the gRPC error for server-streaming RPCs (StreamTelemetry,
// StreamOlcrtcEvents): code 16 with details "UNAUTHENTICATED: Invalid orchestrator secret token."
// (telemetry.service.ts:29-34).
func Unauthenticated() error {
	return status.Error(codes.Unauthenticated, "UNAUTHENTICATED: "+InvalidSecretMessage)
}
