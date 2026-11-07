package binaries

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/runner"
)

const awgListUnits = "systemctl list-units route-awg@* awg-quick@* --state=active --plain --no-legend"

// Checklist 20, 21: target_binary selects the path; awg-quick needs a shebang and passes bash -n.
func TestAwgToolsTargets(t *testing.T) {
	h := newHarness(t, nil)
	h.on(named("bash"), runReal)

	r := h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret), ch("", goodELF("tools"), "1.0.2", "awg"))
	expect(t, r, true, "awg-tools (awg) binary version 1.0.2 successfully updated (service restart skipped: not yet configured)")
	if h.read("/usr/local/bin/awg") != goodELF("tools") || h.exists("/usr/local/bin/awg-quick") {
		t.Fatal("tools path")
	}
	if h.index("/.awg-tools.upload-") < 0 || !strings.HasSuffix(h.displays()[h.index("/.awg-tools.upload-")], " --version") {
		t.Fatalf("tools binary not smoke-tested: %q", h.displays())
	}
	for _, other := range []string{"wg", "", "garbage"} {
		r = h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret), ch("", goodELF(other), "2", other))
		if !r.Success || h.read("/usr/local/bin/awg") != goodELF(other) {
			t.Fatalf("target_binary %q: %+v", other, r)
		}
	}

	script := "#!/bin/bash\nset -e\necho up\n"
	r = h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret), ch("", script, "1.0.2", "awg-quick"))
	expect(t, r, true, "awg-quick script version 1.0.2 successfully updated (service restart skipped: not yet configured)")
	if h.read("/usr/local/bin/awg-quick") != script {
		t.Fatal("quick path")
	}

	r = h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret), ch("", "echo no shebang\n", "1", "awg-quick"))
	expect(t, r, false, "Invalid awg-quick script: missing shebang (#!) header.")

	r = h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret), ch("", "#!/bin/bash\nif then fi (\n", "1", "awg-quick"))
	if r.Success || !strings.HasPrefix(r.Message, "Invalid awg-quick script syntax: ") || !strings.Contains(r.Message, "syntax error") {
		t.Fatalf("%+v", r)
	}
	if h.read("/usr/local/bin/awg-quick") != script {
		t.Fatal("rejected script replaced the installed one")
	}
	if h.awgCI != 5 {
		t.Fatalf("AWG version cache invalidated %d times, want 5", h.awgCI)
	}
}

// Checklist 22: active route-awg@*/awg-quick@* units are restarted; fallback probe; suffixes.
func TestAwgRestartsActiveUnits(t *testing.T) {
	h := newHarness(t, nil)
	h.on(display(awgListUnits), stdout("route-awg@awg0.service loaded active exited AmneziaWG awg0\n"+
		"route-awg@awgmesh0.service loaded active exited AmneziaWG awgmesh0\n"+
		"awg-quick@wg1.service loaded active exited x\n  \n"))
	r := h.upload(h.svc.UploadAwgGoBinary, mdCtx(secret), ch("", goodELF("go"), "0.2.12", ""))
	expect(t, r, true, "amneziawg-go binary version 0.2.12 successfully updated and active AWG service(s) restarted")
	for _, u := range []string{"route-awg@awg0.service", "route-awg@awgmesh0.service", "awg-quick@wg1.service"} {
		if h.calls("systemctl restart "+u) != 1 {
			t.Fatalf("%s not restarted: %q", u, h.displays())
		}
	}
	if h.calls("is-active") != 0 {
		t.Fatal("fallback probe ran although units were listed")
	}

	t.Run("fallback probe of route-awg@<iface>", func(t *testing.T) {
		h := newHarness(t, map[string]string{"AWG_CONFIG_PATH": "/etc/amnezia/amneziawg/awg7.conf"})
		h.on(display("systemctl is-active route-awg@awg7"), stdout("active\n"))
		r := h.upload(h.svc.UploadAwgGoBinary, mdCtx(secret), ch("", goodELF("go"), "1", ""))
		expect(t, r, true, "amneziawg-go binary version 1 successfully updated and active AWG service(s) restarted")
		if h.calls("systemctl restart route-awg@awg7") != 1 {
			t.Fatalf("%q", h.displays())
		}
	})
	t.Run("every restart failed, no prev", func(t *testing.T) {
		h := newHarness(t, nil)
		h.on(display(awgListUnits), stdout("route-awg@awg0.service loaded active exited\n"))
		h.on(display("systemctl restart route-awg@awg0.service"), failing(1, "Job failed"))
		r := h.upload(h.svc.UploadAwgGoBinary, mdCtx(secret), ch("", goodELF("go"), "1", ""))
		expect(t, r, true, "amneziawg-go binary version 1 successfully updated (service restart skipped: not yet configured)")
	})
	t.Run("restart failed with prev: rollback", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("/usr/local/bin/amneziawg-go", "OLD", 0o755)
		h.on(display(awgListUnits), stdout("route-awg@awg0.service loaded active exited\n"))
		n := 0
		h.on(display("systemctl restart route-awg@awg0.service"), func(c runner.Cmd) (runner.Result, error) {
			n++
			if n == 1 {
				return runner.FailWith(c, 1, "", "awg-quick: exec format error")
			}
			return runner.Result{}, nil
		})
		r := h.upload(h.svc.UploadAwgGoBinary, mdCtx(secret), ch("", goodELF("go"), "1", ""))
		if r.Success || !strings.Contains(r.Message, "previous version") || h.read("/usr/local/bin/amneziawg-go") != "OLD" || n != 2 {
			t.Fatalf("%+v n=%d", r, n)
		}
	})
}

// Checklist 23: boot step removes a symlink at the tools path, leaves files and missing paths.
func TestSanitizeAwgToolsSymlink(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.SanitizeAwgToolsSymlink(context.Background()) // missing: no panic, nothing
	h.write("/usr/local/bin/amneziawg-go", "go", 0o755)
	if err := os.Symlink(h.path("/usr/local/bin/amneziawg-go"), h.path("/usr/local/bin/awg")); err != nil {
		t.Fatal(err)
	}
	h.svc.SanitizeAwgToolsSymlink(context.Background())
	if h.exists("/usr/local/bin/awg") || !h.exists("/usr/local/bin/amneziawg-go") {
		t.Fatal("symlink not removed or its target touched")
	}
	h.write("/usr/local/bin/awg", "real", 0o755)
	h.svc.SanitizeAwgToolsSymlink(context.Background())
	if h.read("/usr/local/bin/awg") != "real" {
		t.Fatal("regular file removed")
	}
}
