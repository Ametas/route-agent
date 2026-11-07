package binaries

import (
	"testing"

	"github.com/Ametas/route-agent/internal/systemd"
)

const olcrtcList = "systemctl list-units --type=service --state=running --no-legend olcrtc-agent-srv@*"

// Checklist 28: swap → ensure template unit → unchanged: restart each running instance;
// changed: restart none.
func TestOlcrtcUpload(t *testing.T) {
	h := newHarness(t, nil)
	h.on(display(olcrtcList), stdout("olcrtc-agent-srv@a.service loaded active running x\n● olcrtc-agent-srv@b.service loaded active running y\n\n"))
	r := h.upload(h.svc.UploadOlcrtcAgentSrvBinary, mdCtx(secret), ch("", goodELF("o"), "0.3.1", "olcrtc-agent-srv"))
	expect(t, r, true, "olcrtc-agent-srv binary version 0.3.1 successfully updated")
	unit := systemd.OlcrtcAgentSrvUnit("/usr/local/bin/olcrtc-agent-srv", "/etc/olcrtc-agent-srv")
	if h.read("/etc/systemd/system/olcrtc-agent-srv@.service") != unit {
		t.Fatal("unit content")
	}
	if h.calls("daemon-reload") != 1 || h.calls(olcrtcList) != 0 || h.calls("restart") != 0 {
		t.Fatalf("unit changed → no restarts: %q", h.displays())
	}

	before := len(h.displays())
	r = h.upload(h.svc.UploadOlcrtcAgentSrvBinary, mdCtx(secret), ch("", goodELF("o2"), "", ""))
	expect(t, r, true, "olcrtc-agent-srv binary version unknown successfully updated")
	got := h.displays()[before:]
	want := map[string]bool{"systemctl restart olcrtc-agent-srv@a.service": false, "systemctl restart olcrtc-agent-srv@b.service": false}
	for _, d := range got {
		if _, ok := want[d]; ok {
			want[d] = true
		}
		if d == "systemctl daemon-reload" {
			t.Fatal("unchanged unit reloaded")
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("%s missing: %q", k, got)
		}
	}
	if h.read("/usr/local/bin/olcrtc-agent-srv") != goodELF("o2") || h.read("/usr/local/bin/olcrtc-agent-srv.prev") != goodELF("o") {
		t.Fatal("swap / prev")
	}
}
