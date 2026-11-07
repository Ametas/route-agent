package caddy

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/locks"
)

// ConfigureCaddy waits for the Caddy area with the client's context: a deadline that passes while
// UploadCaddyBinary holds the area abandons the call before any write or command.
func TestConfigureCaddyAbandonedWhileAreaHeld(t *testing.T) {
	h := newHarness(t)
	h.svc.Locks = locks.New()
	_, unlock, _ := h.svc.Locks.Lock(context.Background(), locks.Caddy)
	defer unlock()
	ctx, cancel := context.WithTimeout(authed(), 50*time.Millisecond)
	defer cancel()
	resp, err := h.svc.ConfigureCaddy(ctx, &agentpb.CaddyConfigPayload{CaddyfileContent: "example.com {\n}\n"})
	if status.Code(err) != codes.DeadlineExceeded || resp != nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if n := len(h.fake.Calls()); n != 0 {
		t.Fatalf("abandoned call ran %q", h.fake.Displays())
	}
}
