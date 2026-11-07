package auth

import (
	"context"
	"sort"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
)

const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func withMD(v string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKey, v))
}

func TestVerify(t *testing.T) {
	v := New(secret)
	for _, bad := range []string{"", "wrong", secret + "x", secret[:63]} {
		if v.Verify(bad) {
			t.Errorf("Verify(%q) = true", bad)
		}
	}
	if !v.Verify(secret) {
		t.Fatal("correct secret rejected")
	}
	empty := New("")
	if empty.Verify("") || empty.Verify("x") {
		t.Fatal("an empty expected secret must never match (fail closed)")
	}
}

func TestMatrixCoversEveryRPC(t *testing.T) {
	sd := agentpb.File_agent_proto.Services().Get(0)
	if sd.Methods().Len() != 31 {
		t.Fatalf("proto has %d methods", sd.Methods().Len())
	}
	var missing []string
	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		mode, ok := ModeOf(string(m.Name()))
		if !ok {
			missing = append(missing, string(m.Name()))
			continue
		}
		// Client streams are M|C; GetAgentInfo is none; nothing else is.
		if m.IsStreamingClient() != (mode == ModeMetadataOrChunk) {
			t.Errorf("%s: streaming client=%v but mode %s", m.Name(), m.IsStreamingClient(), mode)
		}
		if (m.Name() == "GetAgentInfo") != (mode == ModeNone) {
			t.Errorf("%s: mode %s", m.Name(), mode)
		}
		// M|B only makes sense where the request has the body field — except ConfigureCaddy,
		// which TS checks with authenticateCall although its payload has no such field (B §0.2).
		hasField := m.Input().Fields().ByName("orchestrator_secret") != nil
		if mode == ModeMetadataOrBody && !hasField && m.Name() != "ConfigureCaddy" {
			t.Errorf("%s is M|B but %s has no orchestrator_secret", m.Name(), m.Input().Name())
		}
	}
	if len(missing) > 0 || len(Methods()) != 31 {
		sort.Strings(missing)
		t.Fatalf("matrix incomplete: missing %v, size %d", missing, len(Methods()))
	}
	// Spot checks straight from spec A §4.3.
	for name, want := range map[string]Mode{
		"ApplyConfig": ModeMetadata, "SelfUpdate": ModeMetadata, "ConfigureAwg": ModeMetadata,
		"GetManagedCertificate": ModeMetadataOrBody, "StreamTelemetry": ModeMetadataOrBody,
		"UploadRearRuleSet": ModeMetadataOrChunk, "GetAgentInfo": ModeNone,
	} {
		if got, _ := ModeOf(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestMetadataOnly(t *testing.T) {
	v := New(secret)
	if !v.Metadata(withMD(secret)) {
		t.Fatal("good metadata rejected")
	}
	if v.Metadata(context.Background()) || v.Metadata(withMD("bad")) {
		t.Fatal("missing/bad metadata accepted")
	}
	// First value wins.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{MetadataKey: {"bad", secret}})
	if v.Metadata(ctx) {
		t.Fatal("only the first metadata value counts")
	}
}

func TestMetadataOrBody(t *testing.T) {
	v := New(secret)
	if !v.MetadataOrBody(withMD(secret), "") || !v.MetadataOrBody(context.Background(), secret) || !v.MetadataOrBody(withMD("bad"), secret) {
		t.Fatal("either source must authenticate")
	}
	if v.MetadataOrBody(withMD("bad"), "bad") || v.MetadataOrBody(context.Background(), "") {
		t.Fatal("bad both ways accepted")
	}
}

func TestStreamGate(t *testing.T) {
	v := New(secret)
	// Metadata verifies; later chunk secrets are ignored.
	g := v.Stream(withMD(secret))
	if !g.Admit("") || !g.Admit("wrong") || !g.Verified() {
		t.Fatal("metadata-verified stream rejected")
	}
	// First chunk verifies, later chunks need nothing.
	g = v.Stream(context.Background())
	if !g.Admit(secret) || !g.Admit("") || !g.Verified() {
		t.Fatal("first-chunk secret rejected")
	}
	// First chunk without a secret rejects; a later good chunk cannot rescue it.
	g = v.Stream(context.Background())
	if g.Admit("") {
		t.Fatal("first chunk without secret admitted")
	}
	if g.Admit(secret) || g.Verified() {
		t.Fatal("a later chunk rescued a rejected stream")
	}
	// No chunks at all and bad metadata: not verified (end-of-stream check).
	if v.Stream(withMD("bad")).Verified() {
		t.Fatal("unverified stream reported verified")
	}
}

func TestUnauthenticated(t *testing.T) {
	st, _ := status.FromError(Unauthenticated())
	if st.Code() != codes.Unauthenticated || st.Message() != "UNAUTHENTICATED: Invalid orchestrator secret token." {
		t.Fatalf("%v %q", st.Code(), st.Message())
	}
}
