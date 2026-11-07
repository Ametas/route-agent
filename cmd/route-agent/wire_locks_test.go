package main

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/version"
)

// Cross-group serialization through the ONE lock registry buildHandlers hands out: a binary
// upload (group C) never runs inside an ApplyConfig / ConfigureCaddy (group B) and vice versa.
// A gate in the fake runner parks the first RPC inside its critical section; the second must not
// issue a single command until the gate opens, and all of the first's commands precede it.

type uploadStream struct {
	ctx  context.Context
	msgs []*agentpb.BinaryChunkPayload
	resp *agentpb.UploadBinaryResponse
}

func (s *uploadStream) Recv() (*agentpb.BinaryChunkPayload, error) {
	if len(s.msgs) == 0 {
		return nil, io.EOF
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}
func (s *uploadStream) SendAndClose(r *agentpb.UploadBinaryResponse) error { s.resp = r; return nil }
func (s *uploadStream) SetHeader(metadata.MD) error                        { return nil }
func (s *uploadStream) SendHeader(metadata.MD) error                       { return nil }
func (s *uploadStream) SetTrailer(metadata.MD)                             {}
func (s *uploadStream) Context() context.Context                           { return s.ctx }
func (s *uploadStream) SendMsg(any) error                                  { return nil }
func (s *uploadStream) RecvMsg(any) error                                  { return nil }

func authed() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "x"))
}

// fakeELF is a 64-bit little-endian ELF header for this GOARCH (the upload checks the machine).
func fakeELF(tag string) []byte {
	machines := map[string]uint16{"amd64": 62, "arm64": 183, "386": 3, "arm": 40, "riscv64": 243}
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF")
	hdr[4], hdr[5], hdr[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(hdr[18:], machines[runtime.GOARCH])
	return append(hdr, tag...)
}

type lockRig struct {
	fake    *runner.Fake
	h       *server.Handlers
	gate    func(runner.Cmd) bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newLockRig(t *testing.T, gate func(runner.Cmd) bool) *lockRig {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	r := &lockRig{gate: gate, entered: make(chan struct{}), release: make(chan struct{})}
	r.fake = &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if r.gate(c) {
			first := false
			r.once.Do(func() { first = true })
			if first {
				close(r.entered)
				<-r.release
			}
		}
		return runner.Result{}, nil
	}}
	r.h = buildHandlers(deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: r.fake, fs: fsx.FS{Root: t.TempDir()},
		systemctl: systemd.Systemctl{Runner: r.fake}, auth: auth.New("x"), version: &version.Resolver{Runner: r.fake}, exe: "/x",
	})
	return r
}

// run starts first, waits until it is parked at the gate, starts second, checks second issued no
// command (isSecond) while first was parked, opens the gate and returns the recorded calls.
func (r *lockRig) run(t *testing.T, first, second func() bool, isSecond func(runner.Cmd) bool) []runner.Cmd {
	t.Helper()
	results := make(chan bool, 2)
	go func() { results <- first() }()
	select {
	case <-r.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("first RPC never reached the gate: %q", r.fake.Displays())
	}
	go func() { results <- second() }()
	time.Sleep(150 * time.Millisecond)
	for _, c := range r.fake.Calls() {
		if isSecond(c) {
			close(r.release)
			t.Fatalf("second RPC ran %q while the first held the area", c.Display())
		}
	}
	close(r.release)
	for i := 0; i < 2; i++ {
		select {
		case ok := <-results:
			if !ok {
				t.Fatalf("an RPC failed: %q", r.fake.Displays())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("RPCs did not finish")
		}
	}
	return r.fake.Calls()
}

// assertBefore: the last call matching a precedes the first matching b.
func assertBefore(t *testing.T, calls []runner.Cmd, a, b func(runner.Cmd) bool) {
	t.Helper()
	lastA, firstB := -1, -1
	for i, c := range calls {
		if a(c) {
			lastA = i
		}
		if b(c) && firstB < 0 {
			firstB = i
		}
	}
	if lastA < 0 || firstB < 0 || lastA > firstB {
		var d []string
		for _, c := range calls {
			d = append(d, c.Display())
		}
		t.Fatalf("interleaved (last first=%d, first second=%d): %q", lastA, firstB, d)
	}
}

func isUpload(c runner.Cmd) bool { return strings.Contains(c.Name, ".upload-") }
func display(s string) func(runner.Cmd) bool {
	return func(c runner.Cmd) bool { return c.Display() == s }
}
func singboxCheck(c runner.Cmd) bool {
	return len(c.Args) == 3 && c.Args[0] == "check" && strings.Contains(c.Args[2], ".config.check_")
}
func caddyApplyValidate(c runner.Cmd) bool {
	return !isUpload(c) && len(c.Args) > 0 && c.Args[0] == "validate"
}
func candidateVersion(c runner.Cmd) bool {
	return isUpload(c) && len(c.Args) == 1 && c.Args[0] == "version"
}

func (r *lockRig) applyConfig() bool {
	resp, err := r.h.ApplyConfig(authed(), &agentpb.ConfigPayload{ConfigJson: "{}"})
	return err == nil && resp.GetSuccess()
}

func (r *lockRig) configureCaddy() bool {
	resp, err := r.h.ConfigureCaddy(authed(), &agentpb.CaddyConfigPayload{CaddyfileContent: "example.com {\n}\n"})
	return err == nil && resp.GetSuccess()
}

func (r *lockRig) upload(fn func(binaries.UploadStream) error) func() bool {
	return func() bool {
		st := &uploadStream{ctx: authed(), msgs: []*agentpb.BinaryChunkPayload{{Chunk: fakeELF("bin"), Version: "1"}}}
		return fn(st) == nil && st.resp.GetSuccess()
	}
}

func TestSingboxUploadWaitsForApplyConfig(t *testing.T) {
	r := newLockRig(t, singboxCheck)
	calls := r.run(t, r.applyConfig, r.upload(r.h.UploadSingboxBinary), isUpload)
	// ApplyConfig ends with `systemctl start sing-box` (the unit was not active).
	assertBefore(t, calls, display("systemctl start sing-box"), isUpload)
}

func TestApplyConfigWaitsForSingboxUpload(t *testing.T) {
	r := newLockRig(t, candidateVersion)
	calls := r.run(t, r.upload(r.h.UploadSingboxBinary), r.applyConfig, singboxCheck)
	// The upload ends with SINGBOX_RESTART_COMMAND.
	assertBefore(t, calls, display("systemctl restart sing-box"), singboxCheck)
}

func TestCaddyUploadWaitsForConfigureCaddy(t *testing.T) {
	r := newLockRig(t, caddyApplyValidate)
	calls := r.run(t, r.configureCaddy, r.upload(r.h.UploadCaddyBinary), isUpload)
	// ConfigureCaddy ends with CADDY_RELOAD_COMMAND.
	assertBefore(t, calls, display("systemctl reload caddy || systemctl restart caddy"), isUpload)
}

func TestConfigureCaddyWaitsForCaddyUpload(t *testing.T) {
	r := newLockRig(t, candidateVersion)
	calls := r.run(t, r.upload(r.h.UploadCaddyBinary), r.configureCaddy, caddyApplyValidate)
	assertBefore(t, calls, display("systemctl restart caddy"), caddyApplyValidate)
}
