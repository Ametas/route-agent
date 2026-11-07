package binaries

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const sbUnit = "/etc/systemd/system/sing-box.service"

func (h *harness) uploadSingbox(content, version string) *agentpb.UploadBinaryResponse {
	h.t.Helper()
	return h.upload(h.svc.UploadSingboxBinary, mdCtx(secret), ch("", content, version, "sing-box"))
}

// Checklist 14 (order), 16 (restart command), 18 (unit), 19 (version cache).
func TestSingboxUploadOrder(t *testing.T) {
	h := newHarness(t, nil)
	h.write("/etc/sing-box/config.json", `{"route":{"rules":[]}}`, 0o644)
	h.write("/etc/route-agent/rear.json", `{}`, 0o644)
	expect(t, h.uploadSingbox(goodELF("v2"), "1.14.0-hotusers"), true, "sing-box binary version 1.14.0-hotusers successfully updated and restarted")

	d := h.displays()
	target := h.path("/usr/local/bin/sing-box")
	want := []string{
		" version",
		"setcap cap_net_admin,cap_net_bind_service=+ep " + h.path("/usr/local/bin") + "/.sing-box.upload-",
		" check -c " + h.path("/etc/sing-box/config.json"),
		" check -c " + h.path("/etc/route-agent/rear.json"),
		"systemctl is-active sing-box",
		"setcap cap_net_admin,cap_net_bind_service=+ep " + target,
		"systemctl daemon-reload",
		"systemctl enable sing-box",
		"systemctl restart sing-box",
	}
	pos := 0
	for _, w := range want {
		found := false
		for ; pos < len(d); pos++ {
			if strings.Contains(d[pos], w) {
				found = true
				pos++
				break
			}
		}
		if !found {
			t.Fatalf("missing or out of order: %q\ncalls: %q", w, d)
		}
	}
	for _, c := range h.fake.Calls() {
		if c.Display() == "systemctl restart sing-box" && !c.Shell {
			t.Fatal("restart must be the configured shell command SINGBOX_RESTART_COMMAND")
		}
		if strings.Contains(c.Display(), "reload sing-box") || strings.Contains(c.Display(), "--now") {
			t.Fatalf("forbidden call %q", c.Display())
		}
	}
	if h.read("/usr/local/bin/sing-box") != goodELF("v2") {
		t.Fatal("target content")
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
	if h.singboxCI != 1 {
		t.Fatalf("version cache invalidated %d times", h.singboxCI)
	}
	// Unit byte-exact, no capability lines.
	unit := h.read(sbUnit)
	if unit != SingboxUnit("/usr/local/bin/sing-box", "/etc/sing-box/config.json") ||
		strings.Contains(unit, "CapabilityBoundingSet") || strings.Contains(unit, "AmbientCapabilities") {
		t.Fatalf("unit:\n%s", unit)
	}
	wantUnit := "[Unit]\nDescription=sing-box service\nAfter=network.target nss-lookup.target\n\n[Service]\n" +
		"ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json\nRestart=always\nRestartSec=5\n" +
		"ExecReload=/bin/sh -c \"/usr/local/bin/sing-box check -c /etc/sing-box/config.json && /bin/kill -HUP $MAINPID\"\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	if unit != wantUnit {
		t.Fatalf("unit differs:\n%q\n%q", unit, wantUnit)
	}

	// Second upload: unit unchanged → no daemon-reload, no enable; restart still runs (16).
	before := len(h.displays())
	expect(t, h.uploadSingbox(goodELF("v3"), "1.14.1"), true, "sing-box binary version 1.14.1 successfully updated and restarted")
	second := h.displays()[before:]
	for _, c := range second {
		if c == "systemctl daemon-reload" || c == "systemctl enable sing-box" {
			t.Fatalf("unchanged unit caused %q", c)
		}
	}
	if !strings.Contains(strings.Join(second, "\n"), "systemctl restart sing-box") {
		t.Fatal("restart missing on second upload")
	}
	if h.read("/usr/local/bin/sing-box.prev") != goodELF("v2") {
		t.Fatal(".prev must hold the replaced binary")
	}
}

func TestSingboxRestartCommandConfigurable(t *testing.T) {
	h := newHarness(t, map[string]string{"SINGBOX_RESTART_COMMAND": "systemctl restart sing-box && echo ok", "RELOAD_COMMAND": "RELOAD-NEVER"})
	expect(t, h.uploadSingbox(goodELF("x"), "1"), true, "sing-box binary version 1 successfully updated and restarted")
	if h.calls("systemctl restart sing-box && echo ok") != 1 || h.calls("RELOAD-NEVER") != 0 {
		t.Fatalf("%q", h.displays())
	}
}

// Checklist 15: missing configs are skipped; a rejecting binary names the first failing config
// (front before rear); target untouched; no restart.
func TestSingboxLiveConfigCheck(t *testing.T) {
	t.Run("no configs", func(t *testing.T) {
		h := newHarness(t, nil)
		expect(t, h.uploadSingbox(goodELF("x"), "1"), true, "sing-box binary version 1 successfully updated and restarted")
		if h.calls(" check -c ") != 0 {
			t.Fatal("check ran without configs")
		}
	})
	for _, tc := range []struct{ name, rejects, path string }{
		{"front rejected", "config.json", "/etc/sing-box/config.json"},
		{"rear rejected", "rear.json", "/etc/route-agent/rear.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.write("/etc/sing-box/config.json", "{}", 0o644)
			h.write("/etc/route-agent/rear.json", "{}", 0o644)
			h.write("/usr/local/bin/sing-box", "OLD", 0o755)
			h.on(func(c runner.Cmd) bool {
				return len(c.Args) == 3 && c.Args[0] == "check" && strings.HasSuffix(c.Args[2], tc.rejects)
			},
				failing(1, "FATAL[0000] decode config: invalid uuid"))
			r := h.uploadSingbox(goodELF("new"), "1.14.0")
			expect(t, r, false, "Бинарь 1.14.0 отвергнут: он не принимает уже применённый на узле конфиг "+tc.path+
				". Прежний бинарь оставлен на месте, сервис не тронут.\nFATAL[0000] decode config: invalid uuid")
			if h.read("/usr/local/bin/sing-box") != "OLD" || h.calls("restart") != 0 || h.exists("/usr/local/bin/sing-box.prev") {
				t.Fatalf("target touched or restarted: %q", h.displays())
			}
			if l := leftovers(t, h.path("/usr/local/bin")); len(l) != 0 {
				t.Fatalf("leftovers %v", l)
			}
		})
	}
	t.Run("error text capped at 1000", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("/etc/sing-box/config.json", "{}", 0o644)
		h.on(func(c runner.Cmd) bool { return len(c.Args) > 0 && c.Args[0] == "check" }, failing(1, strings.Repeat("e", 3000)))
		r := h.uploadSingbox(goodELF("new"), "1")
		if !strings.HasSuffix(r.Message, "\n"+strings.Repeat("e", 1000)) {
			t.Fatalf("len %d", len(r.Message))
		}
	})
}

// Checklist 17: a restart failure with nothing to roll back to does not change the answer.
func TestSingboxRestartFailureWithoutPrev(t *testing.T) {
	h := newHarness(t, nil)
	h.on(func(c runner.Cmd) bool { return c.Shell }, failing(1, "Job failed"))
	expect(t, h.uploadSingbox(goodELF("x"), "1"), true, "sing-box binary version 1 successfully updated and restarted")
}

// New in Go: the new binary does not come up → the previous one is restored and restarted.
func TestSingboxRollback(t *testing.T) {
	t.Run("restart error", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("/usr/local/bin/sing-box", "OLD", 0o755)
		fails := 0
		h.on(func(c runner.Cmd) bool { return c.Shell }, func(c runner.Cmd) (runner.Result, error) {
			fails++
			if fails == 1 {
				return runner.FailWith(c, 1, "", "Job for sing-box.service failed")
			}
			return runner.Result{}, nil
		})
		r := h.uploadSingbox(goodELF("new"), "1.14.0")
		if r.Success || !strings.Contains(r.Message, "previous version") || !strings.Contains(r.Message, "Job for sing-box.service failed") {
			t.Fatalf("%+v", r)
		}
		if h.read("/usr/local/bin/sing-box") != "OLD" || fails != 2 || h.singboxCI != 2 {
			t.Fatalf("content %q restarts %d invalidations %d", h.read("/usr/local/bin/sing-box"), fails, h.singboxCI)
		}
	})
	t.Run("not active after settle", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("/usr/local/bin/sing-box", "OLD", 0o755)
		n := 0
		h.on(display("systemctl is-active sing-box"), func(runner.Cmd) (runner.Result, error) {
			n++
			if n == 1 {
				return runner.Result{Stdout: "active\n"}, nil // before the swap
			}
			return runner.Result{Stdout: "activating\n", ExitCode: 3}, &runner.Error{Cmd: "systemctl is-active sing-box", Stdout: "activating\n", Code: 3}
		})
		r := h.uploadSingbox(goodELF("new"), "1.14.0")
		if r.Success || !strings.Contains(r.Message, `sing-box.service is "activating" after the restart`) {
			t.Fatalf("%+v", r)
		}
		if h.read("/usr/local/bin/sing-box") != "OLD" {
			t.Fatal("not rolled back")
		}
	})
	t.Run("was not active before: no blame", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("/usr/local/bin/sing-box", "OLD", 0o755)
		h.on(display("systemctl is-active sing-box"), stdout("failed\n"))
		expect(t, h.uploadSingbox(goodELF("new"), "1"), true, "sing-box binary version 1 successfully updated and restarted")
		if h.read("/usr/local/bin/sing-box") != goodELF("new") {
			t.Fatal("rolled back without evidence")
		}
	})
}

// Checklist 12: install creates the dir, overwrites, mode 0755, keeps .prev, no temp leftovers;
// a target in another directory is copied in atomically.
func TestInstall(t *testing.T) {
	h := newHarness(t, nil)
	src := filepath.Join(h.tmp, "cand")
	_ = os.WriteFile(src, []byte("NEW"), 0o600)
	had, err := h.svc.install(src, "/opt/new/dir/bin")
	if err != nil || had {
		t.Fatal(had, err)
	}
	if h.read("/opt/new/dir/bin") != "NEW" {
		t.Fatal("content")
	}
	_ = os.WriteFile(src, []byte("NEWER"), 0o600)
	if had, err = h.svc.install(src, "/opt/new/dir/bin"); err != nil || !had {
		t.Fatal(had, err)
	}
	st, _ := os.Stat(h.path("/opt/new/dir/bin"))
	if h.read("/opt/new/dir/bin") != "NEWER" || h.read("/opt/new/dir/bin.prev") != "NEW" || st.Mode().Perm() != 0o755 {
		t.Fatal("overwrite / prev / mode")
	}
	if l := leftovers(t, h.path("/opt/new/dir")); len(l) != 0 {
		t.Fatal(l)
	}
	if err := h.svc.rollback("/opt/new/dir/bin"); err != nil || h.read("/opt/new/dir/bin") != "NEW" {
		t.Fatal("rollback", err)
	}
	if err := h.svc.rollback("/opt/none"); err == nil {
		t.Fatal("rollback without prev must fail")
	}
}

// Checklist 33: UpgradeSingbox answers; the download path is an honest failure (deviation).
func TestUpgradeSingbox(t *testing.T) {
	h := newHarness(t, nil)
	ctxOK := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, secret))
	for _, tc := range []struct {
		ctx context.Context
		url string
		msg string
	}{
		{context.Background(), "https://x/y", auth.InvalidSecretMessage},
		{ctxOK, "", "Missing download_url in request payload."},
		{ctxOK, "not a url", "Invalid download_url format."},
		{ctxOK, "https://", "Invalid download_url format."},
		{ctxOK, "ftp://example.com/sb", "Invalid URL protocol: must be http or https."},
		{ctxOK, "file:///etc/passwd", "Invalid URL protocol: must be http or https."},
		{ctxOK, "https://github.com/SagerNet/sing-box/releases/download/v1.13.0/sing-box-1.13.0-linux-amd64.tar.gz", MsgUpgradeUnsupported},
	} {
		r, err := h.svc.UpgradeSingbox(tc.ctx, &agentpb.UpgradePayload{DownloadUrl: tc.url, Version: "1.13.0"})
		if err != nil || r.Success || r.Message != tc.msg {
			t.Errorf("%q: %v %+v", tc.url, err, r)
		}
	}
	if len(h.displays()) != 0 {
		t.Fatalf("UpgradeSingbox ran commands: %q", h.displays())
	}
}
