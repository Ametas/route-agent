package agentinfo

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/contract"
)

func TestGetAgentInfo(t *testing.T) {
	s := &Service{Version: func(context.Context) string { return "v1.4.0-3-gabc1234" }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// No metadata, no body secret: still answers (unauthenticated by design).
	resp, err := s.GetAgentInfo(context.Background(), &agentpb.AgentInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoContractHash != contract.Expected || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(resp.ProtoContractHash) {
		t.Fatalf("hash %q", resp.ProtoContractHash)
	}
	if resp.ProtoContractSource != "canonical-json" || resp.AgentVersion != "v1.4.0-3-gabc1234" {
		t.Fatalf("%+v", resp)
	}
}

func TestGetAgentInfoInternalFailure(t *testing.T) {
	s := &Service{
		Version: func(context.Context) string { panic("boom") },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	resp, err := s.GetAgentInfo(context.Background(), &agentpb.AgentInfoRequest{})
	if err != nil || resp.ProtoContractHash != "" || resp.AgentVersion != "" || resp.ProtoContractSource != "canonical-json" {
		t.Fatalf("want OK with empty hash/version, got %v %+v", err, resp)
	}
}
