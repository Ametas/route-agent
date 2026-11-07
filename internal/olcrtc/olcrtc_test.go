package olcrtc

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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "0123456789abcdef0123456789abcdef"

type harness struct {
	t    *testing.T
	fs   fsx.FS
	fake *runner.Fake
	svc  *Service
	mu   sync.Mutex
	fail map[string]runner.Result // display → failing result
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": secret})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, fs: fsx.FS{Root: t.TempDir()}, fail: map[string]runner.Result{}}
	h.fake = &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r, ok := h.fail[c.Display()]; ok {
			return runner.FailWith(c, 1, r.Stdout, r.Stderr)
		}
		return runner.Result{}, nil
	}}
	h.svc = &Service{
		Auth: auth.New(secret), Runner: h.fake, FS: h.fs, Systemctl: systemd.Systemctl{Runner: h.fake}, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

func mdCtx(s string) context.Context {
	if s == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

func (h *harness) ran(d string) bool {
	for _, x := range h.fake.Displays() {
		if x == d {
			return true
		}
	}
	return false
}

func payload(u string) *agentpb.OlcrtcInstanceConfigPayload {
	return &agentpb.OlcrtcInstanceConfigPayload{UserShortUuid: u, Carrier: "jitsi", Transport: "datachannel", RoomId: "https://meet.example/room", CryptoKeyHex: "ab12", Dns: ""}
}

// 30. Metadata only.
func TestAuthMetadataOnly(t *testing.T) {
	h := newHarness(t)
	p := payload("u1")
	p.OrchestratorSecret = secret
	r, _ := h.svc.SyncOlcrtcInstance(context.Background(), p)
	if r.Success || r.Message != "Invalid orchestrator secret token." {
		t.Fatalf("sync %+v", r)
	}
	d, _ := h.svc.DeleteOlcrtcInstance(context.Background(), &agentpb.OlcrtcInstanceDeletePayload{OrchestratorSecret: secret, UserShortUuid: "u1"})
	if d.Success || d.Message != "Invalid orchestrator secret token." || len(h.fake.Calls()) != 0 {
		t.Fatalf("delete %+v", d)
	}
}

// 31. Name sanitizing and allowlist.
func TestInstanceName(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{"../../etc/passwd", "", "  ", "a b", strings.Repeat("a", 129)} {
		r, _ := h.svc.SyncOlcrtcInstance(mdCtx(secret), payload(bad))
		d, _ := h.svc.DeleteOlcrtcInstance(mdCtx(secret), &agentpb.OlcrtcInstanceDeletePayload{UserShortUuid: bad})
		if r.Success || r.Message != "Invalid or missing user_short_uuid." || d.Success || d.Message != "Invalid or missing user_short_uuid." {
			t.Fatalf("%q accepted", bad)
		}
	}
	r, _ := h.svc.SyncOlcrtcInstance(mdCtx(secret), payload(" ab\r\nc_-1 "))
	if !r.Success || r.Message != "olcrtc instance abc_-1 configured (restarted)." {
		t.Fatalf("sanitized name: %+v", r)
	}
}

// 32. YAML byte-exact.
func TestBuildYAML(t *testing.T) {
	p := payload("u")
	want := "mode: srv\nauth:\n  provider: jitsi\nroom:\n  id: \"https://meet.example/room\"\ncrypto:\n  key: \"ab12\"\nnet:\n  transport: datachannel\n  dns: \"8.8.8.8:53\"\n"
	if got := BuildYAML(p); got != want {
		t.Fatalf("got:\n%s", got)
	}
	p.Transport, p.Vp8Fps, p.Vp8BatchSize, p.Dns = "vp8channel", 30, 4294967295, " 1.1.1.1:53\n"
	if got := BuildYAML(p); !strings.HasSuffix(got, "  transport: vp8channel\n  dns: \"1.1.1.1:53\"\nvp8:\n  fps: 30\n  batch_size: -1\n") {
		t.Fatalf("vp8:\n%s", got)
	}
	p.Vp8Fps, p.Vp8BatchSize = 0, 0
	if strings.Contains(BuildYAML(p), "vp8:") {
		t.Fatal("zero vp8 values are omitted")
	}
	p.Transport, p.Vp8Fps, p.SeiFps, p.SeiFragmentSize, p.SeiAckTimeoutMs = "seichannel", 10, 5, 1200, 300
	if got := BuildYAML(p); !strings.HasSuffix(got, "  transport: seichannel\n  dns: \"1.1.1.1:53\"\nsei:\n  fps: 5\n  fragment_size: 1200\n  ack_timeout_ms: 300\n") {
		t.Fatalf("sei:\n%s", got)
	}
	// Escaping (spec F R10): quoted values escape `"`/`\`, unsafe plain values get quoted.
	p = payload("u")
	p.RoomId, p.Carrier, p.Transport = `a"b\c`, "evil: x", ""
	got := BuildYAML(p)
	if !strings.Contains(got, `  id: "a\"b\\c"`+"\n") || !strings.Contains(got, `  provider: "evil: x"`+"\n") || !strings.Contains(got, "  transport: \n") {
		t.Fatalf("escaping:\n%s", got)
	}
}

// 33. Sync: always enable, restart iff changed, otherwise start; 0600.
func TestSync(t *testing.T) {
	h := newHarness(t)
	r, _ := h.svc.SyncOlcrtcInstance(mdCtx(secret), payload("u1"))
	if !r.Success || r.Message != "olcrtc instance u1 configured (restarted)." {
		t.Fatalf("first %+v", r)
	}
	cfgPath := h.fs.Path("/etc/olcrtc-agent-srv/u1.yaml")
	b, _ := os.ReadFile(cfgPath)
	st, _ := os.Stat(cfgPath)
	if string(b) != BuildYAML(payload("u1")) || st.Mode().Perm() != 0o600 {
		t.Fatalf("yaml %q mode %v", b, st.Mode().Perm())
	}
	if !h.ran("systemctl enable olcrtc-agent-srv@u1") || !h.ran("systemctl restart olcrtc-agent-srv@u1") || !h.ran("systemctl daemon-reload") {
		t.Fatalf("calls %v", h.fake.Displays())
	}

	// Unchanged: start, no restart; a start failure is ignored.
	h2 := newHarness(t)
	h2.fs = h.fs
	h2.svc.FS = h.fs
	h2.fail["systemctl start olcrtc-agent-srv@u1"] = runner.Result{Stderr: "x"}
	r, _ = h2.svc.SyncOlcrtcInstance(mdCtx(secret), payload("u1"))
	if !r.Success || r.Message != "olcrtc instance u1 configured (unchanged)." || h2.ran("systemctl restart olcrtc-agent-srv@u1") ||
		!h2.ran("systemctl enable olcrtc-agent-srv@u1") || h2.ran("systemctl daemon-reload") {
		t.Fatalf("unchanged %+v %v", r, h2.fake.Displays())
	}

	// A TS-written 0644 file is fixed to 0600 without counting as a change.
	_ = os.Chmod(cfgPath, 0o644)
	r, _ = h2.svc.SyncOlcrtcInstance(mdCtx(secret), payload("u1"))
	st, _ = os.Stat(cfgPath)
	if !strings.HasSuffix(r.Message, "(unchanged).") || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode fix %+v %v", r, st.Mode().Perm())
	}

	// Restart failure: stderr || stdout || message, trimmed.
	h3 := newHarness(t)
	h3.fail["systemctl restart olcrtc-agent-srv@u2"] = runner.Result{Stdout: "  out text \n"}
	h3.fail["systemctl enable olcrtc-agent-srv@u2"] = runner.Result{Stderr: "no"}
	r, _ = h3.svc.SyncOlcrtcInstance(mdCtx(secret), payload("u2"))
	if r.Success || r.Message != "Failed to start olcrtc instance (olcrtc-agent-srv@u2): out text" {
		t.Fatalf("restart failure %+v", r)
	}
}

// 34. Template unit byte-exact; rewritten and daemon-reloaded only on difference.
func TestEnsureUnit(t *testing.T) {
	h := newHarness(t)
	want := "[Unit]\nDescription=olcrtc-agent-srv instance %i (managed by route-agent)\nAfter=network-online.target\nWants=network-online.target\n\n" +
		"[Service]\nType=simple\nUser=root\nExecStart=/usr/local/bin/olcrtc-agent-srv /etc/olcrtc-agent-srv/%i.yaml\nRestart=always\nRestartSec=5\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	if !h.svc.EnsureUnit(context.Background()) {
		t.Fatal("first ensure must write")
	}
	b, _ := os.ReadFile(h.fs.Path("/etc/systemd/system/olcrtc-agent-srv@.service"))
	if string(b) != want {
		t.Fatalf("unit:\n%s", b)
	}
	n := len(h.fake.Calls())
	if h.svc.EnsureUnit(context.Background()) || len(h.fake.Calls()) != n {
		t.Fatal("unchanged unit must not be rewritten or daemon-reloaded")
	}
}

// 35. Delete.
func TestDelete(t *testing.T) {
	h := newHarness(t)
	h.fail["systemctl disable --now olcrtc-agent-srv@u1"] = runner.Result{Stderr: "not loaded"}
	d, _ := h.svc.DeleteOlcrtcInstance(mdCtx(secret), &agentpb.OlcrtcInstanceDeletePayload{UserShortUuid: "u1"})
	if !d.Success || d.Message != "olcrtc instance u1 removed." {
		t.Fatalf("nothing existed %+v", d)
	}
	p := h.fs.Path("/etc/olcrtc-agent-srv/u2.yaml")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o600)
	d, _ = h.svc.DeleteOlcrtcInstance(mdCtx(secret), &agentpb.OlcrtcInstanceDeletePayload{UserShortUuid: "u2"})
	if _, err := os.Stat(p); !d.Success || err == nil || !h.ran("systemctl disable --now olcrtc-agent-srv@u2") {
		t.Fatalf("delete %+v %v", d, h.fake.Displays())
	}
	for _, c := range h.fake.Calls() {
		if c.Shell {
			t.Fatal("no shell")
		}
	}
}

// 37 (hook side). Running instances parsing.
func TestRunningInstances(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		if c.Display() != "systemctl list-units --type=service --state=running --no-legend olcrtc-agent-srv@*" {
			t.Errorf("cmd %q", c.Display())
		}
		return runner.Result{Stdout: "olcrtc-agent-srv@a.service loaded active running x\n● olcrtc-agent-srv@b.service loaded active running y\n\n"}, nil
	}
	units, err := h.svc.RunningInstances(context.Background())
	if err != nil || strings.Join(units, ",") != "olcrtc-agent-srv@a.service,olcrtc-agent-srv@b.service" {
		t.Fatalf("units %v %v", units, err)
	}
}

// --- StreamOlcrtcEvents ---------------------------------------------------------------------

type fakeTail struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	err    error
	ctx    context.Context
	waitFn func() error
	mu     sync.Mutex
}

func (f *fakeTail) Start(ctx context.Context) (io.ReadCloser, func() error, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	f.mu.Lock()
	f.ctx = ctx
	f.mu.Unlock()
	go func() { <-ctx.Done(); _ = f.w.CloseWithError(errors.New("killed")) }()
	wait := f.waitFn
	if wait == nil {
		wait = func() error { return nil }
	}
	return f.r, wait, nil
}

type fakeServerStream struct {
	ctx  context.Context
	mu   sync.Mutex
	sent []*agentpb.OlcrtcEvent
	got  chan struct{}
}

func (s *fakeServerStream) Send(e *agentpb.OlcrtcEvent) error {
	s.mu.Lock()
	s.sent = append(s.sent, e)
	s.mu.Unlock()
	s.got <- struct{}{}
	return nil
}
func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(any) error            { return nil }
func (s *fakeServerStream) RecvMsg(any) error            { return nil }

func TestStreamUnauthenticated(t *testing.T) {
	h := newHarness(t)
	err := h.svc.StreamOlcrtcEvents(&agentpb.OlcrtcEventsRequest{}, &fakeServerStream{ctx: mdCtx("bad"), got: make(chan struct{}, 8)})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unauthenticated || st.Message() != "UNAUTHENTICATED: Invalid orchestrator secret token." {
		t.Fatalf("err %v", err)
	}
	if JournalArgs[1] != "olcrtc-agent-srv@*" || strings.Join(JournalArgs, " ") != "-u olcrtc-agent-srv@* -n 0 -f --output json" {
		t.Fatal("journalctl args")
	}
}

// 36. Mapping, skipping, cancel stops the journal; a journal that ends closes the stream with
// UNAVAILABLE (deviation, spec F R3).
func TestStreamEvents(t *testing.T) {
	h := newHarness(t)
	r, w := io.Pipe()
	tail := &fakeTail{r: r, w: w}
	h.svc.Journal = tail
	ctx, cancel := context.WithCancel(mdCtx(secret))
	stream := &fakeServerStream{ctx: ctx, got: make(chan struct{}, 8)}
	done := make(chan error, 1)
	go func() { done <- h.svc.StreamOlcrtcEvents(&agentpb.OlcrtcEventsRequest{}, stream) }()

	lines := []string{
		"",
		"not json",
		`{"_SYSTEMD_UNIT":"sing-box.service","MESSAGE":"{\"type\":\"x\"}"}`,
		`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@u1.service","MESSAGE":"plain stderr line"}`,
		`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@u1.service","MESSAGE":[104,105]}`,
		`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@u1.service","MESSAGE":"null"}`,
		`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@u1.service","MESSAGE":"{\"type\":\"traffic\",\"ts\":\"2026-09-15T10:00:00.5Z\",\"session_id\":\"s\",\"device_id\":\"d\",\"addr\":\"1.2.3.4:5\",\"bytes_in\":500,\"bytes_out\":600,\"health\":{\"a\":1,\"b\":[true]}}"}`,
	}
	// The last line is split mid-way, inside a multi-byte character, to check reassembly.
	tail2 := `{"_SYSTEMD_UNIT":"olcrtc-agent-srv@u-2.service","MESSAGE":"{\"type\":\"session_close\",\"reason\":\"тайм\"}"}` + "\n"
	go func() {
		_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
		cut := strings.Index(tail2, "тайм") + 1
		_, _ = w.Write([]byte(tail2[:cut]))
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte(tail2[cut:]))
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-stream.got:
		case <-time.After(5 * time.Second):
			t.Fatal("event not delivered")
		}
	}
	stream.mu.Lock()
	a, b := stream.sent[0], stream.sent[1]
	stream.mu.Unlock()
	if a.UserShortUuid != "u1" || a.Type != "traffic" || a.Timestamp != 1789466400500 || a.SessionId != "s" || a.DeviceId != "d" ||
		a.Addr != "1.2.3.4:5" || a.BytesIn != 500 || a.BytesOut != 600 || a.HealthJson != `{"a":1,"b":[true]}` || a.Reason != "" {
		t.Fatalf("a %+v", a)
	}
	if b.UserShortUuid != "u-2" || b.Type != "session_close" || b.Reason != "тайм" || b.HealthJson != "" || b.Timestamp < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("b %+v", b)
	}

	cancel()
	select {
	case err := <-done:
		if st, _ := status.FromError(err); st.Code() != codes.Canceled {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end on cancel")
	}
	tail.mu.Lock()
	if tail.ctx.Err() == nil {
		t.Fatal("journal not stopped")
	}
	tail.mu.Unlock()
}

func TestStreamEndsWhenJournalEnds(t *testing.T) {
	h := newHarness(t)
	r, w := io.Pipe()
	h.svc.Journal = &fakeTail{r: r, w: w, waitFn: func() error { return errors.New("exit status 1") }}
	done := make(chan error, 1)
	go func() {
		done <- h.svc.StreamOlcrtcEvents(&agentpb.OlcrtcEventsRequest{OrchestratorSecret: secret}, &fakeServerStream{ctx: context.Background(), got: make(chan struct{}, 8)})
	}()
	_ = w.Close()
	select {
	case err := <-done:
		st, _ := status.FromError(err)
		if st.Code() != codes.Unavailable || st.Message() != "olcrtc event journal (journalctl) ended: exit status 1" {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream stayed open after journalctl exited")
	}

	h.svc.Journal = &fakeTail{err: errors.New("exec: \"journalctl\": executable file not found in $PATH")}
	err := h.svc.StreamOlcrtcEvents(&agentpb.OlcrtcEventsRequest{OrchestratorSecret: secret}, &fakeServerStream{ctx: context.Background(), got: make(chan struct{}, 8)})
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable || !strings.HasPrefix(st.Message(), "olcrtc event journal could not be started: ") {
		t.Fatalf("spawn failure: %v", err)
	}
}

func TestParseJournalLine(t *testing.T) {
	now := time.UnixMilli(42)
	ev, ok := ParseJournalLine(`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@x.service","MESSAGE":"{\"type\":7,\"ts\":\"junk\",\"bytes_in\":\"12\",\"bytes_out\":1.9}"}`, now)
	if !ok || ev.Type != "7" || ev.Timestamp != 42 || ev.BytesIn != 12 || ev.BytesOut != 1 {
		t.Fatalf("%+v", ev)
	}
	if ev, ok := ParseJournalLine(`{"_SYSTEMD_UNIT":"olcrtc-agent-srv@x.service","MESSAGE":"5"}`, now); !ok || ev.Type != "" {
		t.Fatal("a primitive event is emitted with empty fields, as in TS")
	}
	for _, l := range []string{`null`, `5`, `{"_SYSTEMD_UNIT":"olcrtc-agent-srv@.service","MESSAGE":"{}"}`, `{"_SYSTEMD_UNIT":"olcrtc-agent-srv@x.service","MESSAGE":""}`} {
		if _, ok := ParseJournalLine(l, now); ok {
			t.Fatalf("%q must be skipped", l)
		}
	}
}
