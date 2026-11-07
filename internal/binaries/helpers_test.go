package binaries

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "0123456789abcdef0123456789abcdef"

type agentpbChunk = agentpb.BinaryChunkPayload

// --- fake client stream --------------------------------------------------------------------

type fakeStream struct {
	ctx     context.Context
	msgs    []*agentpb.BinaryChunkPayload
	i       int
	recvErr error  // returned instead of io.EOF after the messages
	onEOF   func() // runs when the half-close is delivered
	onSend  func() // runs inside SendAndClose
	resp    *agentpb.UploadBinaryResponse
	sends   int
}

func (f *fakeStream) Recv() (*agentpb.BinaryChunkPayload, error) {
	if f.i < len(f.msgs) {
		m := f.msgs[f.i]
		f.i++
		return m, nil
	}
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	if f.onEOF != nil {
		f.onEOF()
	}
	return nil, io.EOF
}

func (f *fakeStream) SendAndClose(r *agentpb.UploadBinaryResponse) error {
	f.sends++
	f.resp = r
	if f.onSend != nil {
		f.onSend()
	}
	return nil
}
func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) Context() context.Context     { return f.ctx }
func (f *fakeStream) SendMsg(any) error            { return nil }
func (f *fakeStream) RecvMsg(any) error            { return nil }

func mdCtx(s string) context.Context {
	if s == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// ch builds a chunk: payload secret, bytes, version, target_binary.
func ch(sec, data, version, target string) *agentpb.BinaryChunkPayload {
	var b []byte
	if data != "" {
		b = []byte(data)
	}
	return &agentpb.BinaryChunkPayload{OrchestratorSecret: sec, Chunk: b, Version: version, TargetBinary: target}
}

// --- ELF fixtures --------------------------------------------------------------------------

func elfBin(arch, tag string) string {
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF")
	hdr[4], hdr[5], hdr[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(hdr[18:], elfMachines[arch])
	return string(hdr) + tag
}

func goodELF(tag string) string { return elfBin(runtime.GOARCH, tag) }

// --- harness -------------------------------------------------------------------------------

type rule struct {
	match func(runner.Cmd) bool
	res   func(runner.Cmd) (runner.Result, error)
}

type harness struct {
	t    *testing.T
	fs   fsx.FS
	tmp  string
	cfg  *config.Config
	fake *runner.Fake
	svc  *Service

	mu        sync.Mutex
	rules     []rule
	singboxCI int
	awgCI     int
	meshCI    int
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	e := map[string]string{"EGRESS_CONTROL_SECRET": secret}
	for k, v := range env {
		e[k] = v
	}
	cfg, err := config.Parse(e)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, fs: fsx.FS{Root: t.TempDir()}, tmp: t.TempDir(), cfg: cfg}
	h.fake = &runner.Fake{Handler: h.handle}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.svc = New(Deps{
		Auth: auth.New(secret), Runner: h.fake, FS: h.fs, Systemctl: systemd.Systemctl{Runner: h.fake}, Cfg: cfg, Log: log,
		Telemetry: Telemetry{
			SingboxSwapped:    func() { h.mu.Lock(); h.singboxCI++; h.mu.Unlock() },
			AwgSwapped:        func() { h.mu.Lock(); h.awgCI++; h.mu.Unlock() },
			MeshKernelChanged: func() { h.mu.Lock(); h.meshCI++; h.mu.Unlock() },
		},
	})
	h.svc.TempDir = h.tmp
	h.svc.PostCheckSettle = 0
	return h
}

// on adds a scripted outcome; later rules win.
func (h *harness) on(match func(runner.Cmd) bool, res func(runner.Cmd) (runner.Result, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rules = append(h.rules, rule{match, res})
}

func (h *harness) handle(c runner.Cmd) (runner.Result, error) {
	h.mu.Lock()
	rules := slices.Clone(h.rules)
	h.mu.Unlock()
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].match(c) {
			return rules[i].res(c)
		}
	}
	return runner.Result{}, nil
}

func display(s string) func(runner.Cmd) bool {
	return func(c runner.Cmd) bool { return c.Display() == s }
}
func args(a ...string) func(runner.Cmd) bool {
	return func(c runner.Cmd) bool { return !c.Shell && slices.Equal(c.Args, a) }
}
func named(n string) func(runner.Cmd) bool { return func(c runner.Cmd) bool { return c.Name == n } }

func stdout(s string) func(runner.Cmd) (runner.Result, error) {
	return func(runner.Cmd) (runner.Result, error) { return runner.Result{Stdout: s}, nil }
}
func failing(code int, stderr string) func(runner.Cmd) (runner.Result, error) {
	return func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, code, "", stderr) }
}

// runReal runs the command for real (bash -n, tar).
func runReal(c runner.Cmd) (runner.Result, error) { return runner.Exec{}.Run(context.Background(), c) }

func (h *harness) path(p string) string { return h.fs.Path(p) }

func (h *harness) write(p, content string, mode os.FileMode) {
	h.t.Helper()
	rp := h.path(p)
	if err := os.MkdirAll(filepath.Dir(rp), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(rp, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read(p string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.path(p))
	if err != nil {
		h.t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func (h *harness) exists(p string) bool {
	_, err := os.Lstat(h.path(p))
	return err == nil
}

func (h *harness) upload(fn func(UploadStream) error, ctx context.Context, msgs ...*agentpb.BinaryChunkPayload) *agentpb.UploadBinaryResponse {
	h.t.Helper()
	st := &fakeStream{ctx: ctx, msgs: msgs}
	if err := fn(st); err != nil {
		h.t.Fatalf("handler returned a gRPC error: %v", err)
	}
	if st.sends != 1 {
		h.t.Fatalf("expected exactly one answer, got %d", st.sends)
	}
	return st.resp
}

func (h *harness) displays() []string { return h.fake.Displays() }

// index returns the position of the first call whose Display contains sub, or -1.
func (h *harness) index(sub string) int {
	for i, d := range h.displays() {
		if strings.Contains(d, sub) {
			return i
		}
	}
	return -1
}

func (h *harness) calls(sub string) int {
	n := 0
	for _, d := range h.displays() {
		if strings.Contains(d, sub) {
			n++
		}
	}
	return n
}

// leftovers lists upload temp files (".…upload-…", "ruleset_…", "amneziawg_kernel_…") and swap
// temps ("…_*.tmp") under dirs.
func leftovers(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			n := e.Name()
			if strings.Contains(n, ".upload-") || strings.HasPrefix(n, "ruleset_") || strings.HasPrefix(n, "amneziawg_kernel_") ||
				strings.HasSuffix(n, ".tmp") || strings.Contains(n, ".tmp-") {
				out = append(out, filepath.Join(d, n))
			}
		}
	}
	return out
}

func expect(t *testing.T, r *agentpb.UploadBinaryResponse, success bool, msg string) {
	t.Helper()
	if r.GetSuccess() != success || r.GetMessage() != msg {
		t.Fatalf("got {success:%v message:%q}\nwant {success:%v message:%q}", r.GetSuccess(), r.GetMessage(), success, msg)
	}
}
