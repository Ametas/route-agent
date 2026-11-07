package binaries

import (
	"slices"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/caddy"
	"github.com/Ametas/route-agent/internal/runner"
)

// Checklist 24: target, drop-in content, changed → daemon-reload + restart, unchanged → restart.
func TestCaddyUpload(t *testing.T) {
	h := newHarness(t, nil)
	r := h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c1"), "2.10.0", "caddy-custom"))
	expect(t, r, true, "Custom Caddy binary version 2.10.0 successfully updated and caddy.service restarted on it")
	if h.read("/usr/local/bin/caddy-custom") != goodELF("c1") {
		t.Fatal("target")
	}
	want := "[Service]\nExecStart=\nExecStart=/usr/local/bin/caddy-custom run --config /etc/caddy/Caddyfile\n" +
		"ExecReload=\nExecReload=/usr/local/bin/caddy-custom reload --config /etc/caddy/Caddyfile --force\n" +
		"EnvironmentFile=-/etc/caddy/cloudflare.env\n"
	if got := h.read(CaddyOverridePath); got != want || strings.Contains(got, "--environ") {
		t.Fatalf("drop-in:\n%s", got)
	}
	if i, j := h.index("systemctl daemon-reload"), h.index("systemctl restart caddy"); i < 0 || j < i || h.calls("restart caddy") != 1 {
		t.Fatalf("%q", h.displays())
	}
	if h.index(" version") > h.index("systemctl daemon-reload") {
		t.Fatal("smoke test must run before the swap")
	}

	before := len(h.displays())
	expect(t, h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c2"), "2.10.1", "")), true,
		"Custom Caddy binary version 2.10.1 successfully updated and caddy.service restarted on it")
	second := h.displays()[before:]
	if slices.Contains(second, "systemctl daemon-reload") || !slices.Contains(second, "systemctl restart caddy") {
		t.Fatalf("unchanged drop-in: %q", second)
	}

	t.Run("CADDY_BINARY_PATH", func(t *testing.T) {
		h := newHarness(t, map[string]string{"CADDY_BINARY_PATH": "/opt/caddy/bin/caddy", "CADDYFILE_PATH": "/srv/Caddyfile"})
		expect(t, h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c"), "1", "")), true,
			"Custom Caddy binary version 1 successfully updated and caddy.service restarted on it")
		if !h.exists("/opt/caddy/bin/caddy") || !strings.Contains(h.read(CaddyOverridePath), "ExecStart=/opt/caddy/bin/caddy run --config /srv/Caddyfile\n") {
			t.Fatal(h.read(CaddyOverridePath))
		}
	})
}

// Restart failure: with nothing to roll back to it is a failure (as TS); with .prev, rollback.
func TestCaddyRestartFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.on(display("systemctl restart caddy"), failing(1, "Job for caddy.service failed"))
	r := h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c"), "1", ""))
	expect(t, r, false, "Failed to upload binary: Command failed: systemctl restart caddy\nJob for caddy.service failed")

	h = newHarness(t, nil)
	h.write("/usr/local/bin/caddy-custom", "OLD", 0o755)
	n := 0
	h.on(display("systemctl restart caddy"), func(c runner.Cmd) (runner.Result, error) {
		n++
		if n == 1 {
			return runner.FailWith(c, 1, "", "Job for caddy.service failed")
		}
		return runner.Result{}, nil
	})
	r = h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c"), "1", ""))
	if r.Success || !strings.Contains(r.Message, "previous version") || h.read("/usr/local/bin/caddy-custom") != "OLD" || n != 2 {
		t.Fatalf("%+v n=%d", r, n)
	}
}

// New in Go: the candidate must run and accept the live Caddyfile (cloudflare.env merged).
func TestCaddyValidatesBeforeSwap(t *testing.T) {
	h := newHarness(t, nil)
	h.write("/etc/caddy/Caddyfile", "example.com {\n}\n", 0o644)
	// Read by caddy.LoadDNSProviderEnv (JS trim: a BOM and CRLF are stripped, as in TS) — the same
	// reader ConfigureCaddy validates with.
	h.write(caddy.CloudflareEnvPath, "\uFEFFCF_API_TOKEN=tok123\r\n# token\n\n", 0o600)
	h.write("/usr/local/bin/caddy-custom", "OLD", 0o755)
	var env []string
	h.on(func(c runner.Cmd) bool { return len(c.Args) > 0 && c.Args[0] == "validate" }, func(c runner.Cmd) (runner.Result, error) {
		env = c.Env
		return runner.FailWith(c, 1, "", "Error: adapting config: unrecognized directive: foo")
	})
	r := h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c"), "2.10.0", ""))
	expect(t, r, false, "Бинарь Caddy 2.10.0 отвергнут: он не принимает уже применённый на узле конфиг /etc/caddy/Caddyfile. "+
		"Прежний бинарь оставлен на месте, сервис не тронут.\nError: adapting config: unrecognized directive: foo")
	if !slices.Contains(env, "CF_API_TOKEN=tok123") {
		t.Fatalf("cloudflare.env not merged: %v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "EGRESS_CONTROL_SECRET=") {
			t.Fatal("node secret leaked to caddy validate")
		}
	}
	if h.read("/usr/local/bin/caddy-custom") != "OLD" || h.calls("restart") != 0 || h.exists(CaddyOverridePath) {
		t.Fatal("rejected binary touched the node")
	}
	// version smoke failure
	h = newHarness(t, nil)
	h.on(args("version"), failing(2, "bad"))
	r = h.upload(h.svc.UploadCaddyBinary, mdCtx(secret), ch("", goodELF("c"), "1", ""))
	if r.Success || !strings.HasPrefix(r.Message, "Failed to upload binary: Command failed: ") {
		t.Fatalf("%+v", r)
	}
}
