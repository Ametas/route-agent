package caddy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/certs"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "test-secret-123"

const caddyfileBody = "example.com {\n    respond \"ok\"\n}\n"

type harness struct {
	t    *testing.T
	fs   fsx.FS
	fake *runner.Fake
	svc  *Service

	mu        sync.Mutex
	active    string // systemctl is-active caddy stdout
	validate  func(c runner.Cmd) (runner.Result, error)
	reload    func(c runner.Cmd) (runner.Result, error)
	journal   string
	journalOK bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, fs: fsx.FS{Root: t.TempDir()}, active: "active\n", journalOK: true}
	h.fake = &runner.Fake{Handler: h.handle}
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": secret})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = &Service{
		Auth:      auth.New(secret),
		Runner:    h.fake,
		FS:        h.fs,
		Systemctl: systemd.Systemctl{Runner: h.fake},
		Cfg:       cfg,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

func (h *harness) handle(c runner.Cmd) (runner.Result, error) {
	// Hooks are copied under the lock and called outside it, so a blocking hook does not
	// serialize the callers by itself.
	h.mu.Lock()
	active, validate, reload, journal, journalOK := h.active, h.validate, h.reload, h.journal, h.journalOK
	h.mu.Unlock()
	d := c.Display()
	switch {
	case d == "systemctl is-active caddy":
		if active != "active\n" {
			return runner.FailWith(c, 3, active, "")
		}
		return runner.Result{Stdout: active}, nil
	case len(c.Args) > 0 && c.Args[0] == "validate":
		if validate != nil {
			return validate(c)
		}
	case c.Shell:
		if reload != nil {
			return reload(c)
		}
	case strings.HasPrefix(d, "journalctl"):
		if !journalOK {
			return runner.FailWith(c, 1, "", "no journal")
		}
		return runner.Result{Stdout: journal}, nil
	}
	return runner.Result{}, nil
}

func authed() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", secret))
}

func (h *harness) call(req *agentpb.CaddyConfigPayload) *agentpb.CaddyConfigResponse {
	h.t.Helper()
	resp, err := h.svc.ConfigureCaddy(authed(), req)
	if err != nil {
		h.t.Fatalf("ConfigureCaddy returned a gRPC error: %v", err)
	}
	return resp
}

func (h *harness) read(p string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.fs.Path(p))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) mode(p string) os.FileMode {
	h.t.Helper()
	st, err := os.Stat(h.fs.Path(p))
	if err != nil {
		h.t.Fatal(err)
	}
	return st.Mode().Perm()
}

func (h *harness) mtime(p string) time.Time {
	h.t.Helper()
	st, err := os.Stat(h.fs.Path(p))
	if err != nil {
		h.t.Fatal(err)
	}
	return st.ModTime()
}

// Checklist 1, 2: bad or empty secret → status OK, success:false, exact text; nothing runs.
func TestAuth(t *testing.T) {
	h := newHarness(t)
	for _, ctx := range []context.Context{
		context.Background(),
		metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", "bad_secret")),
	} {
		resp, err := h.svc.ConfigureCaddy(ctx, &agentpb.CaddyConfigPayload{CaddyfileContent: "example.com {}"})
		if err != nil || resp.Success || resp.Message != auth.InvalidSecretMessage {
			t.Fatalf("got %v %v", resp, err)
		}
	}
	if len(h.fake.Calls()) != 0 {
		t.Fatalf("commands ran without auth: %v", h.fake.Displays())
	}
}

// Checklist 19, 25, 28 + app.test.ts "should apply caddyfileContent and unpack camouflage html".
func TestApplyWritesAndReloads(t *testing.T) {
	h := newHarness(t)
	_ = os.MkdirAll(h.fs.Path(LibDir), 0o755)
	resp := h.call(&agentpb.CaddyConfigPayload{
		CaddyfileContent: "example.com {\n  root * /var/www\n}",
		CamouflageHtml:   "<html><body>Camouflage Site</body></html>",
		CamouflagePath:   "/var/www/camo",
	})
	if !resp.Success || resp.Message != "Caddyfile успешно валидирован и применен." {
		t.Fatalf("got %+v", resp)
	}
	if got := h.read("/var/www/camo/index.html"); got != "<html><body>Camouflage Site</body></html>" {
		t.Fatalf("html = %q", got)
	}
	if got := h.read("/etc/caddy/Caddyfile"); got != "example.com {\n  root * /var/www\n}" {
		t.Fatalf("caddyfile = %q", got)
	}
	want := []string{
		"chown -R caddy:caddy /var/lib/caddy",
		"caddy validate --config /etc/caddy/Caddyfile",
		"systemctl reload caddy || systemctl restart caddy",
	}
	if got := h.fake.Displays(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n got %q\nwant %q", got, want)
	}
	for _, c := range h.fake.Calls() {
		if c.Name == "chmod" {
			t.Fatal("chmod -R 755 must not be reproduced (spec B R1)")
		}
		if c.Shell && c.Name != h.svc.Cfg.CaddyReloadCommand {
			t.Fatalf("unexpected shell command %q", c.Name)
		}
	}
}

// Checklist 18: default path, CR/LF stripped and trimmed, traversal refused with the TS text.
func TestCamouflagePath(t *testing.T) {
	h := newHarness(t)
	resp := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody, CamouflageHtml: "<p>x</p>"})
	if !resp.Success || h.read("/var/www/camouflage/index.html") != "<p>x</p>" {
		t.Fatalf("default path: %+v", resp)
	}
	resp = h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody, CamouflageHtml: "<p>y</p>", CamouflagePath: " /var/www/de\r\ncoy \n"})
	if !resp.Success || h.read("/var/www/decoy/index.html") != "<p>y</p>" {
		t.Fatalf("sanitized path: %+v", resp)
	}
	resp = h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody, CamouflagePath: "/var/www/../etc"})
	if resp.Success || resp.Message != "Path Traversal restriction: /var/www/../etc is not within allowed directories" {
		t.Fatalf("traversal: %+v", resp)
	}
}

// app.test.ts "validateSafeCamouflagePath should allow safe paths and reject Path Traversal".
func TestValidateSafeCamouflagePath(t *testing.T) {
	for _, ok := range []string{"/var/www/my-site", "/tmp/camouflage/site", "/opt/route-agent/decoy", "/var/www"} {
		if _, err := ValidateSafeCamouflagePath(ok, DefaultCamouflageRoots); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/var/www/../etc/passwd", "/root/secret", "/var/wwwx", "/var/www/..hidden"} {
		_, err := ValidateSafeCamouflagePath(bad, DefaultCamouflageRoots)
		if err == nil || !strings.Contains(err.Error(), "Path Traversal restriction") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := ValidateSafeCamouflagePath("", DefaultCamouflageRoots); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty: %v", err)
	}
}

// Checklist 19, 20, 21: HTML only when non-blank, CF env 0600, certs before the Caddyfile, keys
// 0600, incomplete bulk entries skipped.
func TestPlanOrderAndModes(t *testing.T) {
	h := newHarness(t)
	plan, caddyfile, err := h.svc.Plan(&agentpb.CaddyConfigPayload{
		CaddyfileContent:   caddyfileBody,
		CamouflageHtml:     "   \n",
		CloudflareApiToken: "cf-token",
		ExtraCertPem:       "C1", ExtraKeyPem: "K1", ExtraCertDomain: "*.front.example.com",
		ExtraCertificates: []*agentpb.CaddyExtraCertificate{
			{Domain: " ring.example.com ", CertPem: "C2", KeyPem: "K2"},
			{Domain: "half.example.com", CertPem: "C3"},
			{Domain: "  ", CertPem: "C4", KeyPem: "K4"},
		},
	})
	if err != nil || caddyfile != "/etc/caddy/Caddyfile" {
		t.Fatal(err, caddyfile)
	}
	c1, k1 := certs.Paths("*.front.example.com", "")
	c2, k2 := certs.Paths("ring.example.com", "")
	want := []fsx.PlannedWrite{
		{Path: "/etc/caddy/cloudflare.env", Content: []byte("CF_API_TOKEN=cf-token\n"), Mode: 0o600},
		{Path: c1, Content: []byte("C1")}, {Path: k1, Content: []byte("K1"), Mode: 0o600},
		{Path: c2, Content: []byte("C2")}, {Path: k2, Content: []byte("K2"), Mode: 0o600},
		{Path: "/etc/caddy/Caddyfile", Content: []byte(caddyfileBody)},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d writes, want %d: %+v", len(plan), len(want), plan)
	}
	for i := range want {
		if plan[i].Path != want[i].Path || string(plan[i].Content) != string(want[i].Content) || plan[i].Mode != want[i].Mode {
			t.Errorf("write %d = %s %q %o, want %s %q %o", i, plan[i].Path, plan[i].Content, plan[i].Mode, want[i].Path, want[i].Content, want[i].Mode)
		}
	}
	if !strings.HasSuffix(k1, "/wildcard_.front.example.com/wildcard_.front.example.com.key") {
		t.Fatalf("certmagic-safe path: %s", k1)
	}

	resp := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody, CloudflareApiToken: "cf-token",
		ExtraCertificates: []*agentpb.CaddyExtraCertificate{{Domain: "ring.example.com", CertPem: "C2", KeyPem: "K2"}}})
	if !resp.Success {
		t.Fatal(resp)
	}
	if h.mode("/etc/caddy/cloudflare.env") != 0o600 || h.mode(k2) != 0o600 {
		t.Fatalf("modes: env %o key %o", h.mode("/etc/caddy/cloudflare.env"), h.mode(k2))
	}
}

// Checklist 22 + caddyConfigUnchanged.test.ts "identical config is neither rewritten nor reloaded".
func TestUnchangedSkip(t *testing.T) {
	h := newHarness(t)
	req := &agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody,
		ExtraCertificates: []*agentpb.CaddyExtraCertificate{{Domain: "ring.example.com", CertPem: "CERT\n", KeyPem: "KEY\n"}}}
	if r := h.call(req); !r.Success {
		t.Fatal(r)
	}
	at := h.mtime("/etc/caddy/Caddyfile")
	n := len(h.fake.Calls())
	time.Sleep(20 * time.Millisecond)
	r := h.call(req)
	if !r.Success || r.Message != "Caddy configuration already current — nothing to write or reload." {
		t.Fatalf("got %+v", r)
	}
	if !h.mtime("/etc/caddy/Caddyfile").Equal(at) {
		t.Fatal("Caddyfile rewritten without changes")
	}
	if got := h.fake.Displays()[n:]; len(got) != 1 || got[0] != "systemctl is-active caddy" {
		t.Fatalf("skip ran commands: %q", got)
	}
}

// Checklist 23: a changed cert, Caddy down, or a drifted key mode each break the skip. The key
// ends up 0600 — and stays 0600, since chmod -R 755 is gone (spec B R1; the TS test failed as
// root with 0755).
func TestSkipBreakers(t *testing.T) {
	certOf := func(pem string) *agentpb.CaddyConfigPayload {
		return &agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody,
			ExtraCertificates: []*agentpb.CaddyExtraCertificate{{Domain: "ring.example.com", CertPem: pem, KeyPem: "KEY-MATERIAL\n"}}}
	}
	t.Run("changed cert", func(t *testing.T) {
		h := newHarness(t)
		h.call(certOf("CERT-V1\n"))
		if r := h.call(certOf("CERT-V2\n")); strings.Contains(r.Message, "already current") {
			t.Fatal("updated certificate taken for unchanged")
		}
	})
	t.Run("caddy down", func(t *testing.T) {
		h := newHarness(t)
		h.call(certOf("CERT\n"))
		at := h.mtime("/etc/caddy/Caddyfile")
		h.active = "inactive\n"
		time.Sleep(20 * time.Millisecond)
		if r := h.call(certOf("CERT\n")); strings.Contains(r.Message, "already current") || !r.Success {
			t.Fatalf("dead Caddy taken for current: %+v", r)
		}
		if h.mtime("/etc/caddy/Caddyfile").Equal(at) {
			t.Fatal("not rewritten")
		}
	})
	t.Run("drifted key mode", func(t *testing.T) {
		h := newHarness(t)
		h.call(certOf("CERT\n"))
		_, key := certs.Paths("ring.example.com", "")
		if err := os.Chmod(h.fs.Path(key), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := h.call(certOf("CERT\n")); strings.Contains(r.Message, "already current") {
			t.Fatal("drifted permissions taken for current")
		}
		if m := h.mode(key); m != 0o600 {
			t.Fatalf("key mode %o, want 600", m)
		}
		if r := h.call(certOf("CERT\n")); r.Message != MsgAlreadyCurrent {
			t.Fatalf("with R1 fixed the skip must fire on a cert node: %+v", r)
		}
	})
}

// Checklist 24, 26: backup only when the Caddyfile exists; a validate failure restores it and the
// reply carries Caddy's raw output (the orchestrator greps proxy_protocol).
func TestValidateFailure(t *testing.T) {
	h := newHarness(t)
	h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "old {}\n"})
	if _, err := os.Stat(h.fs.Path("/etc/caddy/Caddyfile.bak")); err == nil {
		t.Fatal("backup made although no Caddyfile existed")
	}
	h.validate = func(c runner.Cmd) (runner.Result, error) {
		return runner.FailWith(c, 1, "", "Error: adapting config using caddyfile: module not registered: caddy.listeners.proxy_protocol\n")
	}
	r := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "new {}\n"})
	want := "Ошибка синтаксиса Caddyfile:\nError: adapting config using caddyfile: module not registered: caddy.listeners.proxy_protocol"
	if r.Success || r.Message != want {
		t.Fatalf("got %q", r.Message)
	}
	if got := h.read("/etc/caddy/Caddyfile"); got != "old {}\n" {
		t.Fatalf("not rolled back: %q", got)
	}
	if got := h.read("/etc/caddy/Caddyfile.bak"); got != "old {}\n" {
		t.Fatalf("backup: %q", got)
	}
	// stdout is the fallback when stderr is empty; then the Node exec text with the quoted path.
	h.validate = func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, 1, "  out text \n", "") }
	if r := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "new2 {}\n"}); r.Message != MsgValidatePrefix+"out text" {
		t.Fatalf("stdout fallback: %q", r.Message)
	}
	h.validate = func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, 1, "", "") }
	if r := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "new3 {}\n"}); r.Message != MsgValidatePrefix+`Command failed: caddy validate --config "/etc/caddy/Caddyfile"` {
		t.Fatalf("message fallback: %q", r.Message)
	}
}

// Checklist 25: the binary is resolved at call time; cloudflare.env is merged into validate's env.
func TestValidateBinaryAndEnv(t *testing.T) {
	h := newHarness(t)
	var got runner.Cmd
	h.validate = func(c runner.Cmd) (runner.Result, error) { got = c; return runner.Result{}, nil }
	h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody, CloudflareApiToken: "tok-1"})
	if got.Name != "caddy" {
		t.Fatalf("binary before upload = %q", got.Name)
	}
	if !contains(got.Env, "CF_API_TOKEN=tok-1") {
		t.Fatal("cloudflare.env not merged into validate env")
	}
	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "EGRESS_CONTROL_SECRET=") {
			t.Fatal("node secret leaked into the child env")
		}
	}
	if err := os.MkdirAll(h.fs.Path("/usr/local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.fs.Path(DefaultCustomBinaryPath), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody + "#2\n"})
	if got.Name != DefaultCustomBinaryPath || strings.Join(got.Args, " ") != "validate --config /etc/caddy/Caddyfile" {
		t.Fatalf("after upload: %s", got.Display())
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Checklist 27: reload failure restores the backup and reports the journal tail, else the error.
func TestReloadFailure(t *testing.T) {
	h := newHarness(t)
	h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "old {}\n"})
	h.reload = func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, 1, "", "Job failed\n") }
	h.journal = "line1\nline2\n"
	r := h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "new {}\n"})
	if r.Success || r.Message != "Сбой службы Caddy при reload:\nline1\nline2" {
		t.Fatalf("got %q", r.Message)
	}
	if h.read("/etc/caddy/Caddyfile") != "old {}\n" {
		t.Fatal("not rolled back")
	}
	last := h.fake.Displays()[len(h.fake.Calls())-1]
	if last != "journalctl -u caddy -n 12 --no-pager" {
		t.Fatalf("no second reload after restore expected; last command %q", last)
	}
	h.journalOK = false
	r = h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: "new2 {}\n"})
	if r.Message != "Сбой службы Caddy при reload:\nCommand failed: systemctl reload caddy || systemctl restart caddy\nJob failed\n" {
		t.Fatalf("fallback: %q", r.Message)
	}
}

// Checklist 29 + app.test.ts fixCaddyPermissions / fixXraySocketPermissions.
func TestPermissionHelpers(t *testing.T) {
	h := newHarness(t)
	log := h.svc.Log
	FixPermissions(context.Background(), h.fake, h.fs, log, "/var/lib/caddy")
	if len(h.fake.Calls()) != 0 {
		t.Fatal("chown ran for a missing dir")
	}
	_ = os.MkdirAll(h.fs.Path("/var/lib/caddy"), 0o755)
	FixPermissions(context.Background(), h.fake, h.fs, log, "/var/lib/caddy")
	if got := h.fake.Displays(); len(got) != 1 || got[0] != "chown -R caddy:caddy /var/lib/caddy" {
		t.Fatalf("got %q", got)
	}

	f := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if strings.HasSuffix(c.Display(), "a.sock") {
			return runner.FailWith(c, 1, "", "simulated chmod failure")
		}
		return runner.Result{}, nil
	}}
	_ = os.MkdirAll(h.fs.Path("/dev/shm"), 0o755)
	for _, s := range []string{"/dev/shm/a.sock", "/dev/shm/b.sock"} {
		_ = os.WriteFile(h.fs.Path(s), nil, 0o600)
	}
	FixXraySocketPermissions(context.Background(), f, h.fs, log, []string{"/dev/shm/a.sock", "/dev/shm/missing.sock", "/dev/shm/b.sock"})
	if got := f.Displays(); strings.Join(got, "|") != "chmod 666 /dev/shm/a.sock|chmod 666 /dev/shm/b.sock" {
		t.Fatalf("got %q", got)
	}
	if !contains(XraySocketPaths, "/dev/shm/vless-xhttp-cdn.sock") {
		t.Fatal("CDN socket missing (spec B R14)")
	}
}

// app.test.ts loadCaddyDnsProviderEnv and resolveCaddyBinary.
func TestEnvFileAndBinary(t *testing.T) {
	h := newHarness(t)
	if got := LoadDNSProviderEnv(h.fs, CloudflareEnvPath); len(got) != 0 {
		t.Fatal(got)
	}
	_ = os.MkdirAll(h.fs.Path("/etc/caddy"), 0o755)
	_ = os.WriteFile(h.fs.Path(CloudflareEnvPath), []byte("# comment\nCF_API_TOKEN=real-token-value\n\nEMPTY_LINE_ABOVE=ignored\r\nA = b\nnoeq\n"), 0o600)
	got := LoadDNSProviderEnv(h.fs, CloudflareEnvPath)
	if len(got) != 3 || got["CF_API_TOKEN"] != "real-token-value" || got["EMPTY_LINE_ABOVE"] != "ignored" || got["A "] != " b" {
		t.Fatalf("got %q", got)
	}
	h.svc.Cfg.CaddyBinaryPath = filepath.Join("/opt", "caddy-custom-fake")
	if ResolveBinary(h.fs, h.svc.Cfg) != "caddy" {
		t.Fatal("must fall back before the binary exists")
	}
	_ = os.MkdirAll(h.fs.Path("/opt"), 0o755)
	_ = os.WriteFile(h.fs.Path("/opt/caddy-custom-fake"), []byte("x"), 0o755)
	if ResolveBinary(h.fs, h.svc.Cfg) != "/opt/caddy-custom-fake" {
		t.Fatal("must resolve the custom path once it exists")
	}
}

// R6: concurrent applies are serialized — the second starts only after the first finished.
func TestConcurrentAppliesSerialized(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var inFlight, maxInFlight int
	var mu sync.Mutex
	h.validate = func(c runner.Cmd) (runner.Result, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return runner.Result{}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.call(&agentpb.CaddyConfigPayload{CaddyfileContent: caddyfileBody + strings.Repeat("#", i+1)})
		}(i)
	}
	<-entered
	select {
	case <-entered:
		t.Fatal("second apply entered validate while the first was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if maxInFlight != 1 {
		t.Fatalf("max in flight = %d", maxInFlight)
	}
}
