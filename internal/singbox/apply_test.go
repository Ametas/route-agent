package singbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "test-secret-123"

type harness struct {
	t    *testing.T
	fs   fsx.FS
	fake *runner.Fake
	svc  *Service

	mu         sync.Mutex
	version    string
	versionErr bool
	active     bool
	mainPID    string
	checkErr   string
	reloadErr  string
	ufw        bool
	checks     []string // check files seen, with "+" when the file existed during the check
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// A short root keeps the users-api unix socket path under the 108-byte limit.
	root, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	h := &harness{t: t, fs: fsx.FS{Root: root}, version: "sing-box version 1.12.3\n", active: true, mainPID: "0"}
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
		LookPath: func(string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.ufw {
				return "/usr/sbin/ufw", nil
			}
			return "", errors.New("not found")
		},
	}
	return h
}

func (h *harness) handle(c runner.Cmd) (runner.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := c.Display()
	switch {
	case c.Name == DefaultBinaryPath && len(c.Args) == 1 && c.Args[0] == "version":
		if h.versionErr {
			return runner.FailWith(c, 1, "", "boom")
		}
		return runner.Result{Stdout: h.version}, nil
	case c.Name == DefaultBinaryPath && len(c.Args) == 3 && c.Args[0] == "check":
		mark := c.Args[2]
		if _, err := os.Stat(h.fs.Path(c.Args[2])); err == nil {
			mark = "+" + mark
		}
		h.checks = append(h.checks, mark)
		if h.checkErr != "" {
			return runner.FailWith(c, 1, "", h.checkErr)
		}
	case d == "systemctl is-active sing-box":
		if !h.active {
			return runner.FailWith(c, 3, "inactive\n", "")
		}
		return runner.Result{Stdout: "active\n"}, nil
	case d == "systemctl show sing-box -p MainPID --value":
		return runner.Result{Stdout: h.mainPID + "\n"}, nil
	case c.Shell || d == "systemctl start sing-box":
		if h.reloadErr != "" {
			return runner.FailWith(c, 1, "", h.reloadErr)
		}
	}
	return runner.Result{}, nil
}

func (h *harness) set(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

func (h *harness) writeFile(p, content string) {
	h.t.Helper()
	if err := os.MkdirAll(h.fs.Path(path.Dir(p)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.fs.Path(p), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeConfig(content string) { h.writeFile(DefaultConfigPath, content) }
func (h *harness) writeUnit()                 { h.writeFile(DefaultUnitPath, "unit") }

func (h *harness) read(p string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.fs.Path(p))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) apply(cfgJSON string, force bool) *agentpb.ConfigResponse {
	h.t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", secret))
	resp, err := h.svc.ApplyConfig(ctx, &agentpb.ConfigPayload{ConfigJson: cfgJSON, ForceReload: force})
	if err != nil {
		h.t.Fatalf("gRPC error: %v", err)
	}
	return resp
}

func (h *harness) fork() {
	h.writeUnit()
	h.set(func() { h.version = "sing-box version 1.14.0-hotusers\n" })
}

// serialized is what the agent writes for cfgJSON (with users-api when fork).
func serialized(t *testing.T, cfgJSON string, fork bool) string {
	t.Helper()
	v := js(t, cfgJSON)
	if fork {
		v, _ = WithUsersAPIService(v)
	}
	return Serialize(v)
}

// usersAPI starts a fake users-api on the agent's socket path.
type usersAPI struct {
	mu     sync.Mutex
	reqs   []string // "METHOD rawpath body"
	status int
}

func (h *harness) startUsersAPI() *usersAPI {
	h.t.Helper()
	sock := h.fs.Path(UsersAPISocketPath)
	if err := os.MkdirAll(path.Dir(sock), 0o755); err != nil {
		h.t.Fatal(err)
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		h.t.Fatal(err)
	}
	u := &usersAPI{status: http.StatusOK}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Content-Type")+" "+string(b))
		st := u.status
		u.mu.Unlock()
		w.WriteHeader(st)
		if st != http.StatusOK {
			_, _ = w.Write([]byte("  inbound not found \n"))
		}
	})}
	go func() { _ = srv.Serve(lis) }()
	h.t.Cleanup(func() { _ = srv.Close() })
	return u
}

func (u *usersAPI) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.reqs...)
}

func hasShell(f *runner.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		if c.Shell || c.Display() == "systemctl start sing-box" {
			out = append(out, c.Display())
		}
	}
	return out
}

const egressCfg = `{"log":{"level":"warn"},"inbounds":[{"type":"vless","tag":"stars-in","listen":"::","listen_port":443}],"outbounds":[{"type":"direct","tag":"direct"}]}`

// Checklist 1–3.
func TestApplyAuth(t *testing.T) {
	h := newHarness(t)
	for _, ctx := range []context.Context{
		context.Background(),
		metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", "bad_secret")),
	} {
		resp, err := h.svc.ApplyConfig(ctx, &agentpb.ConfigPayload{ConfigJson: egressCfg})
		if err != nil || resp.Success || resp.Message != "Invalid orchestrator secret token." {
			t.Fatal(resp, err)
		}
	}
	h.svc.Auth = auth.New("")
	if r := h.apply(egressCfg, false); r.Success {
		t.Fatal("an empty EGRESS_CONTROL_SECRET must reject everything")
	}
	if len(h.fake.Calls()) != 0 {
		t.Fatal(h.fake.Displays())
	}
}

// Checklist 4.
func TestApplyInvalidJSON(t *testing.T) {
	h := newHarness(t)
	if r := h.apply("", false); r.Success || r.Message != "Internal Agent Error: Unexpected end of JSON input" {
		t.Fatal(r)
	}
	if r := h.apply("{", false); r.Success || !strings.HasPrefix(r.Message, "Internal Agent Error: ") {
		t.Fatal(r)
	}
}

// Checklist 5, 7, 16, 17: stock writes no services; first apply on an inactive core starts it.
func TestApplyStockWritesAndStarts(t *testing.T) {
	h := newHarness(t)
	h.writeUnit()
	h.set(func() { h.active = false })
	r := h.apply(egressCfg, false)
	if !r.Success || r.Message != MsgApplied {
		t.Fatal(r)
	}
	got := h.read(DefaultConfigPath)
	if got != serialized(t, egressCfg, false) || strings.Contains(got, "services") {
		t.Fatalf("written:\n%s", got)
	}
	if sh := hasShell(h.fake); len(sh) != 1 || sh[0] != "systemctl start sing-box" {
		t.Fatalf("reload commands %q", sh)
	}
	st, _ := os.Stat(h.fs.Path(DefaultConfigPath))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o, want 600 (spec B R8)", st.Mode().Perm())
	}
}

// Checklist 5, 6: fork gets users-api appended once; an unchanged push runs nothing else.
func TestApplyForkAndUnchangedSkip(t *testing.T) {
	h := newHarness(t)
	h.fork()
	h.writeConfig(serialized(t, egressCfg, true))
	at := time.Now().Add(-time.Hour)
	_ = os.Chtimes(h.fs.Path(DefaultConfigPath), at, at)

	r := h.apply(egressCfg, false)
	if !r.Success || r.Message != "Sing-box configuration already current — nothing to write or reload." {
		t.Fatal(r)
	}
	want := "/usr/local/bin/sing-box version|systemctl is-active sing-box|systemctl show sing-box -p MainPID --value"
	if got := strings.Join(h.fake.Displays(), "|"); got != want {
		t.Fatalf("commands %q", got)
	}
	st, _ := os.Stat(h.fs.Path(DefaultConfigPath))
	if !st.ModTime().Equal(at) {
		t.Fatal("file touched on the skip path")
	}
	if c := strings.Count(h.read(DefaultConfigPath), `"users-api"`); c != 1 {
		t.Fatalf("users-api present %d times", c)
	}
	// The same config already containing the service is not duplicated either.
	withSvc := jsjson.Stringify(js(t, serialized(t, egressCfg, true)), "")
	if r := h.apply(withSvc, false); r.Message != MsgAlreadyCurrent {
		t.Fatal(r)
	}
}

// Checklist 8, 16: a stale image restarts the core and never takes the hot path.
func TestApplyStaleImageRestarts(t *testing.T) {
	h := newHarness(t)
	h.fork()
	u := h.startUsersAPI()
	h.writeConfig(serialized(t, baseCfg, true))
	h.set(func() { h.mainPID = "4242" })
	_ = os.MkdirAll(h.fs.Path("/proc/4242"), 0o755)
	if err := os.Symlink("/usr/local/bin/sing-box (deleted)", h.fs.Path("/proc/4242/exe")); err != nil {
		t.Fatal(err)
	}
	// Same config: the stale image alone breaks the skip.
	if r := h.apply(baseCfg, false); r.Message != MsgApplied {
		t.Fatal(r)
	}
	next := strings.Replace(baseCfg, `"users":[{"name":"a","password":"p-a"}]`, `"users":[]`, 1)
	if r := h.apply(next, false); r.Message != MsgApplied {
		t.Fatal(r)
	}
	if len(u.requests()) != 0 {
		t.Fatal("hot path taken on a stale image")
	}
	for _, c := range hasShell(h.fake) {
		if c != "systemctl restart sing-box" {
			t.Fatalf("command %q, want SINGBOX_RESTART_COMMAND", c)
		}
	}
}

// Checklist 9: check runs on a temp file in the config dir that is always removed; a failure
// writes nothing.
func TestApplyCheck(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("{}")
	h.set(func() { h.checkErr = "FATAL[0000] decode config: unknown field\n" })
	r := h.apply(egressCfg, false)
	if r.Success || r.Message != "Rejected by Node Agent: Invalid sing-box syntax. Error: FATAL[0000] decode config: unknown field\n" {
		t.Fatal(r)
	}
	if h.read(DefaultConfigPath) != "{}" || len(hasShell(h.fake)) != 0 {
		t.Fatal("rejected config written or reloaded")
	}
	if len(h.checks) != 1 || !strings.HasPrefix(h.checks[0], "+/etc/sing-box/.config.check_") || !strings.HasSuffix(h.checks[0], ".json") {
		t.Fatalf("check file %q", h.checks)
	}
	entries, _ := os.ReadDir(h.fs.Path("/etc/sing-box"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config.check_") {
			t.Fatal("check file left behind")
		}
	}
	// An empty stderr reports the error text instead.
	h.set(func() { h.checkErr = "" })
	h.fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "check" {
			return runner.FailWith(c, 1, "", "")
		}
		return h.handle(c)
	}
	if r := h.apply(egressCfg, false); !strings.HasPrefix(r.Message, "Rejected by Node Agent: Invalid sing-box syntax. Error: Command failed: /usr/local/bin/sing-box check -c /etc/sing-box/.config.check_") {
		t.Fatal(r)
	}
}

// Checklist 10.
func TestApplyFirewallSync(t *testing.T) {
	h := newHarness(t)
	h.set(func() { h.ufw = true })
	cfgWith := `{"inbounds":[{"type":"hysteria2","listen":"0.0.0.0","listen_port":2053},{"type":"tuic","listen":"127.0.0.1","listen_port":20001}]}`
	h.apply(cfgWith, false)
	ufwCmds := func() []string {
		var out []string
		for _, d := range h.fake.Displays() {
			if strings.HasPrefix(d, "sudo ufw") {
				out = append(out, d)
			}
		}
		return out
	}
	if got := strings.Join(ufwCmds(), "|"); got != "sudo ufw allow 2053/udp|sudo ufw reload" {
		t.Fatal(got)
	}
	if got := h.read(ActivePortsCachePath); got != "[\n  2053\n]" {
		t.Fatalf("cache %q", got)
	}
	n := len(ufwCmds())
	h.apply(strings.Replace(cfgWith, "20001", "20002", 1), false)
	if len(ufwCmds()) != n {
		t.Fatal("unchanged port set must not touch ufw")
	}
	h.apply(egressCfg, false)
	if got := strings.Join(ufwCmds()[n:], "|"); got != "sudo ufw delete allow 2053/udp|sudo ufw reload" {
		t.Fatal(got)
	}
	if got := h.read(ActivePortsCachePath); got != "[]" {
		t.Fatalf("cache %q", got)
	}
	h.set(func() { h.ufw = false })
	m := len(ufwCmds())
	h.apply(cfgWith, false)
	if len(ufwCmds()) != m {
		t.Fatal("no ufw → no-op")
	}
}

const hotBase = `{"log":{"level":"warn"},"inbounds":[
 {"type":"tuic","tag":"tuic in/ü","listen":"::","listen_port":8443,"users":[{"name":"a","uuid":"u-a","password":"p-a"}]},
 {"type":"hysteria2","tag":"lightning-hy2","listen":"::","listen_port":8444,"users":[{"name":"a","password":"p-a"}]}],
 "outbounds":[{"type":"direct","tag":"direct"}]}`

var hotNext = strings.Replace(hotBase, `"users":[{"name":"a","uuid":"u-a","password":"p-a"}]`,
	`"users":[{"name":"a","uuid":"u-a","password":"p-a"},{"name":"b","uuid":"u-b","password":"p-b"}]`, 1)

// Checklist 11, 14: roster-only change goes through users-api, no reload.
func TestApplyHotSwap(t *testing.T) {
	h := newHarness(t)
	h.fork()
	u := h.startUsersAPI()
	old := serialized(t, hotBase, true)
	h.writeConfig(old)
	r := h.apply(hotNext, false)
	want := "Applied 1 inbound roster change(s) through the live users-api — no reload, sessions kept."
	if !r.Success || r.Message != want {
		t.Fatal(r)
	}
	reqs := u.requests()
	if len(reqs) != 1 || reqs[0] != `PUT /inbounds/tuic%20in%2F%C3%BC/users application/json [{"name":"a","uuid":"u-a","password":"p-a"},{"name":"b","uuid":"u-b","password":"p-b"}]` {
		t.Fatalf("requests %q", reqs)
	}
	if len(hasShell(h.fake)) != 0 {
		t.Fatal("hot path must not reload")
	}
	if h.read(DefaultConfigPath) != serialized(t, hotNext, true) || h.read(DefaultConfigPath+".bak") != old {
		t.Fatal("file and backup")
	}
}

// Checklist 12, 13 and R4.
func TestApplyHotSwapFallbacks(t *testing.T) {
	t.Run("users-api non-200 → reload", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		u := h.startUsersAPI()
		u.status = http.StatusNotFound
		h.writeConfig(serialized(t, hotBase, true))
		if r := h.apply(hotNext, false); r.Message != MsgApplied {
			t.Fatal(r)
		}
		if sh := hasShell(h.fake); len(sh) != 1 || sh[0] != "systemctl reload sing-box" {
			t.Fatal(sh)
		}
	})
	t.Run("socket missing → reload", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		h.writeConfig(serialized(t, hotBase, true))
		if r := h.apply(hotNext, false); r.Message != MsgApplied {
			t.Fatal(r)
		}
	})
	t.Run("non-roster diff → reload, no PUT", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		u := h.startUsersAPI()
		h.writeConfig(serialized(t, hotBase, true))
		if r := h.apply(strings.Replace(hotNext, "8444", "9444", 1), false); r.Message != MsgApplied || len(u.requests()) != 0 {
			t.Fatal(r, u.requests())
		}
	})
	t.Run("unreadable disk config → reload", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		u := h.startUsersAPI()
		h.writeConfig("{garbage")
		if r := h.apply(hotNext, false); r.Message != MsgApplied || len(u.requests()) != 0 {
			t.Fatal(r)
		}
	})
	t.Run("force_reload never takes the hot path", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		u := h.startUsersAPI()
		h.writeConfig(serialized(t, hotBase, true))
		if r := h.apply(hotNext, true); r.Message != MsgApplied || len(u.requests()) != 0 || len(hasShell(h.fake)) != 1 {
			t.Fatal(r)
		}
	})
	t.Run("failed PUT then failed reload restores the OLD config (R4)", func(t *testing.T) {
		h := newHarness(t)
		h.fork()
		u := h.startUsersAPI()
		u.status = http.StatusInternalServerError
		old := serialized(t, hotBase, true)
		h.writeConfig(old)
		h.set(func() { h.reloadErr = "Job for sing-box.service failed.\n" })
		r := h.apply(hotNext, false)
		if r.Success || r.Message != "Internal Agent Error: Command failed: systemctl reload sing-box\nJob for sing-box.service failed.\n" {
			t.Fatal(r)
		}
		if h.read(DefaultConfigPath) != old {
			t.Fatal("rollback target lost: the new config was restored")
		}
	})
}

// Checklist 14, 15: .bak taken before the write; a failed reload restores it, with no second reload.
func TestApplyReloadFailureRestores(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("{\"old\":true}")
	h.set(func() { h.reloadErr = "reload failed\n" })
	r := h.apply(egressCfg, false)
	if r.Success || r.Message != "Internal Agent Error: Command failed: systemctl reload sing-box\nreload failed\n" {
		t.Fatal(r)
	}
	if h.read(DefaultConfigPath) != "{\"old\":true}" || h.read(DefaultConfigPath+".bak") != "{\"old\":true}" {
		t.Fatal("not restored")
	}
	if len(hasShell(h.fake)) != 1 {
		t.Fatal("a second reload ran after the restore")
	}
	// No previous config: nothing to restore, the new file stays.
	h2 := newHarness(t)
	h2.set(func() { h2.reloadErr = "x" })
	h2.apply(egressCfg, false)
	if h2.read(DefaultConfigPath) != serialized(t, egressCfg, false) {
		t.Fatal("without a previous config the written file stays")
	}
}

// R6: applies are serialized.
func TestApplySerialized(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	h.fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "check" {
			entered <- struct{}{}
			<-release
		}
		return h.handle(c)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.apply(strings.Replace(egressCfg, "443", itoa(1000+i), 1), false)
		}(i)
	}
	<-entered
	select {
	case <-entered:
		t.Fatal("second apply entered while the first was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
}

// Switchover parity (spec B §2): a config file written by the TS agent (testdata generated with
// Node 22 from an orchestrator-shaped push) is "already current" for the Go agent on the very
// first push — stock and fork — so the switchover costs no reload and drops no session.
func TestSwitchoverMatchesTSWrittenFile(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	req := read("ts_request.json")
	for _, tc := range []struct {
		name, file string
		fork       bool
	}{{"stock", "ts_written_stock.json", false}, {"fork", "ts_written_fork.json", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeUnit()
			if tc.fork {
				h.fork()
			}
			h.writeConfig(read(tc.file))
			if r := h.apply(req, false); r.Message != MsgAlreadyCurrent {
				t.Fatalf("TS-written file not recognised: %+v", r)
			}
		})
	}
}
