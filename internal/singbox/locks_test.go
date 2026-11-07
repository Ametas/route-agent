package singbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/locks"
)

// ApplyConfig waits for the sing-box area with the client's context: a deadline that passes
// while another RPC (UploadSingboxBinary) holds the area abandons the push before any command.
func TestApplyConfigAbandonedWhileAreaHeld(t *testing.T) {
	h := newHarness(t)
	h.svc.Locks = locks.New()
	_, unlock, _ := h.svc.Locks.Lock(context.Background(), locks.SingBox)
	defer unlock()
	ctx, cancel := context.WithTimeout(authedCtx(), 50*time.Millisecond)
	defer cancel()
	resp, err := h.svc.ApplyConfig(ctx, &agentpb.ConfigPayload{ConfigJson: "{}"})
	if status.Code(err) != codes.DeadlineExceeded || resp != nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if _, serr := os.Stat(h.fs.Path(DefaultConfigPath)); len(h.fake.Calls()) != 0 || serr == nil {
		t.Fatalf("abandoned apply touched the node: %q", h.fake.Displays())
	}
}

// The egress ufw sync nests locks.Firewall inside locks.SingBox (the documented order): it waits
// while ManageFirewall (group D) holds the firewall, and the rest of the apply follows.
func TestApplyFirewallSyncTakesFirewallArea(t *testing.T) {
	h := newHarness(t)
	h.set(func() { h.ufw = true })
	h.svc.Locks = locks.New()
	_, unlock, _ := h.svc.Locks.Lock(context.Background(), locks.Firewall)
	done := make(chan *agentpb.ConfigResponse, 1)
	go func() {
		done <- h.apply(`{"inbounds":[{"type":"hysteria2","listen":"0.0.0.0","listen_port":2053}]}`, false)
	}()
	time.Sleep(100 * time.Millisecond)
	for _, d := range h.fake.Displays() {
		if strings.Contains(d, "ufw") {
			t.Fatalf("ufw ran while the firewall area was held: %q", d)
		}
	}
	unlock()
	select {
	case r := <-done:
		if !r.GetSuccess() {
			t.Fatal(r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply never finished")
	}
	if !strings.Contains(strings.Join(h.fake.Displays(), "\n"), "sudo ufw allow 2053/udp") {
		t.Fatalf("%q", h.fake.Displays())
	}
}
