package mesh

import (
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// AWG 3.1 fields of the mesh strategy (2026-11-14): numbers and ranges are written, flags as
// on/off, garbage and newline injection never reach the config.
func TestAwg31Directives(t *testing.T) {
	got := Awg31Directives(&agentpb.MeshTunnelConfigPayload{
		ContentPaddingAddition: "24-34", RekeyAfterTime: "104-118", RekeyTimeout: "4-5", RejectAfterTime: "170-195",
		KeepaliveTimeout: "12", MaxHandshakeAttempts: "16-20", RandomTrailers: "on", DisableCookies: "off",
	})
	want := "ContentPaddingAddition = 24-34\nRekeyAfterTime = 104-118\nRekeyTimeout = 4-5\nRejectAfterTime = 170-195\n" +
		"KeepaliveTimeout = 12\nMaxHandshakeAttempts = 16-20\nRandomTrailers = on\nDisableCookies = off\n"
	if got != want {
		t.Fatalf("Awg31Directives =\n%s\nwant\n%s", got, want)
	}
	if got := Awg31Directives(&agentpb.MeshTunnelConfigPayload{}); got != "" {
		t.Fatalf("empty payload must write nothing, got %q", got)
	}
	if got := Awg31Directives(&agentpb.MeshTunnelConfigPayload{RekeyTimeout: "5\nPostUp = rm -rf /", KeepaliveTimeout: "abc", RandomTrailers: "maybe"}); got != "" {
		t.Fatalf("garbage must be dropped, got %q", got)
	}
	conf, _ := RenderConfig(&agentpb.MeshTunnelConfigPayload{PrivateKey: "k", RekeyTimeout: "4-5"})
	if !strings.Contains(conf, "RekeyTimeout = 4-5\n") {
		t.Fatalf("RenderConfig lost the AWG 3.1 field:\n%s", conf)
	}
}
