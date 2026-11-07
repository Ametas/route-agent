package binaries

import (
	"context"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/locks"
)

// An upload whose area is held by another group's RPC (here: the registry held directly) waits
// for it — even when the client leaves meanwhile (the wait is part of the detached apply, spec C
// §12 item 34) — and then applies.
func TestUploadWaitsForAreaHeldElsewhere(t *testing.T) {
	h := newHarness(t, nil)
	reg := locks.New()
	h.svc.locks = reg
	_, unlock, err := reg.Lock(context.Background(), locks.SingBox)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(mdCtx(secret))
	st := &fakeStream{ctx: ctx, msgs: []*agentpbChunk{ch("", goodELF("sb"), "1.13.0", "")}, onEOF: cancel}
	done := make(chan error, 1)
	go func() { done <- h.svc.UploadSingboxBinary(st) }()

	time.Sleep(100 * time.Millisecond)
	if n := h.calls(" version"); n != 0 {
		t.Fatalf("apply started while the sing-box area was held (%d calls: %q)", n, h.displays())
	}
	select {
	case err := <-done:
		t.Fatalf("upload answered while the area was held: %v", err)
	default:
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload never proceeded after the area was released")
	}
	expect(t, st.resp, true, "sing-box binary version 1.13.0 successfully updated and restarted")
}

// Each upload RPC takes its documented area (locks package doc).
func TestUploadAreas(t *testing.T) {
	h := newHarness(t, nil)
	for _, c := range []struct {
		rpc  string
		fn   func(UploadStream) error
		area locks.Name
	}{
		{"UploadSingboxBinary", h.svc.UploadSingboxBinary, locks.SingBox},
		{"UploadCaddyBinary", h.svc.UploadCaddyBinary, locks.Caddy},
		{"UploadAwgToolsBinary", h.svc.UploadAwgToolsBinary, locks.AWG},
		{"UploadAwgGoBinary", h.svc.UploadAwgGoBinary, locks.AWG},
		{"UploadMihomoBinary", h.svc.UploadMihomoBinary, locks.Rear},
		{"UploadRearRuleSet", h.svc.UploadRearRuleSet, locks.Rear},
		{"UploadOlcrtcAgentSrvBinary", h.svc.UploadOlcrtcAgentSrvBinary, locks.Olcrtc},
		{"UploadMeshAwgKernelSource", h.svc.UploadMeshAwgKernelSource, locks.Mesh},
	} {
		t.Run(c.rpc, func(t *testing.T) {
			reg := locks.New()
			h.svc.locks = reg
			_, unlock, _ := reg.Lock(context.Background(), c.area)
			st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", goodELF("x"), "1", "name")}}
			done := make(chan struct{})
			go func() { _ = c.fn(st); close(done) }()
			select {
			case <-done:
				t.Fatalf("%s answered while %s was held: %+v", c.rpc, c.area, st.resp)
			case <-time.After(50 * time.Millisecond):
			}
			unlock()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never proceeded", c.rpc)
			}
		})
	}
}
