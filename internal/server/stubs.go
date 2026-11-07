package server

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// STUBS — every RPC whose Handlers field is nil lands here and answers codes.Unimplemented
// (12). A porting group removes its RPCs from this path simply by setting their Handlers fields
// in cmd/route-agent/wire.go; this file does not need editing. The TS agent mapped all 31 RPCs,
// and since group E landed so does the Go agent: nothing reaches this stub in the wired agent
// (cmd/route-agent TestAllRPCsWired, TestNoRPCAnswersUnimplemented). It stays as the safety net
// for a Handlers value built without wiring (tests). GetAgentInfo must never be left here: the
// orchestrator reads code 12 from it as "very old agent" (spec A §3.1).
func unimplementedRPC(method string) error {
	group := rpcOwner[method]
	return status.Errorf(codes.Unimplemented, "method %s not implemented yet in the Go agent (porting group %s)", method, group)
}
