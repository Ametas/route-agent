package telemetry

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// fakeJournal is a JournalTail backed by a pipe; it records starts and the stream's end.
type fakeJournal struct {
	mu      sync.Mutex
	starts  int
	w       *io.PipeWriter
	stopped chan struct{}
	failErr error
}

func newFakeJournal() *fakeJournal { return &fakeJournal{stopped: make(chan struct{})} }

func (j *fakeJournal) Start(ctx context.Context) (io.ReadCloser, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.starts++
	if j.failErr != nil {
		return nil, j.failErr
	}
	r, w := io.Pipe()
	j.w = w
	go func() {
		<-ctx.Done() // the stream's end: the real tail gets SIGTERM here
		_ = w.Close()
		close(j.stopped)
	}()
	return r, nil
}

func (j *fakeJournal) write(s string) {
	j.mu.Lock()
	w := j.w
	j.mu.Unlock()
	_, _ = w.Write([]byte(s))
}

func (j *fakeJournal) startCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.starts
}

type harness struct {
	client agentpb.EgressAgentServiceClient
	done   chan struct{} // closed when the server handler returned
}

// serve runs svc behind the real gRPC server (internal/server) on 127.0.0.1.
func serve(t *testing.T, svc *Service) *harness {
	t.Helper()
	done := make(chan struct{}, 16)
	h := &server.Handlers{
		StreamTelemetry: func(req *agentpb.TelemetryRequest, st grpc.ServerStreamingServer[agentpb.TelemetryResponse]) error {
			defer func() { done <- struct{}{} }()
			return svc.StreamTelemetry(req, st)
		},
		GetTelemetry: svc.GetTelemetry,
	}
	srv := server.New(h, server.Options{Keepalive: server.DefaultKeepalive()})
	lis, err := server.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &harness{client: agentpb.NewEgressAgentServiceClient(conn), done: done}
}

func withSecret(ctx context.Context, s string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, auth.MetadataKey, s)
}

type got struct {
	f  *agentpb.TelemetryResponse
	at time.Time
}

func recvN(t *testing.T, st grpc.ServerStreamingClient[agentpb.TelemetryResponse], n int) []got {
	t.Helper()
	var out []got
	for len(out) < n {
		f, err := st.Recv()
		if err != nil {
			t.Fatalf("recv %d: %v", len(out), err)
		}
		out = append(out, got{f: f, at: time.Now()})
	}
	return out
}

// Checklist 1: bad secret → UNAUTHENTICATED with the TS details, no frame, no journalctl.
func TestStreamUnauthenticated(t *testing.T) {
	r := newRig(t)
	j := newFakeJournal()
	h := serve(t, &Service{Auth: auth.New(secret), Collector: r.col, Log: discard, Journal: j})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := h.client.StreamTelemetry(withSecret(ctx, "wrong"), &agentpb.TelemetryRequest{OrchestratorSecret: "also-wrong"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	s, _ := status.FromError(err)
	if s.Code() != codes.Unauthenticated || s.Message() != "UNAUTHENTICATED: Invalid orchestrator secret token." {
		t.Fatalf("%v", err)
	}
	if j.startCount() != 0 || len(r.fake.Calls()) != 0 {
		t.Fatal("work done for an unauthenticated stream")
	}
}

// Checklist 2–6, 18 and the architecture rules: payload-only secret; first frame at once; frames
// every Interval after the previous one with a source that hangs longer than the cadence (no
// overlapping ticks, one probe at a time); system_logs = journal output since the previous frame;
// strictly increasing ms timestamps; the client's cancel ends the stream and stops journalctl.
func TestStreamCadenceAndLifecycle(t *testing.T) {
	r := newRig(t)
	r.ans.ok("awg show awg0 dump", "PRIV\tPUB\t1\toff\n")
	r.ans.slow("awg show awg0 dump", 700*time.Millisecond) // longer than interval + budget
	j := newFakeJournal()
	const interval, budget = 300 * time.Millisecond, 100 * time.Millisecond
	svc := &Service{Auth: auth.New(secret), Collector: r.col, Log: discard, Journal: j, Interval: interval, StreamBudget: budget}
	h := serve(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t0 := time.Now()
	st, err := h.client.StreamTelemetry(ctx, &agentpb.TelemetryRequest{OrchestratorSecret: secret}) // no metadata
	if err != nil {
		t.Fatal(err)
	}
	frames := recvN(t, st, 2)
	if first := frames[0].at.Sub(t0); first > budget+400*time.Millisecond {
		t.Fatalf("first frame after %v", first)
	}
	if j.startCount() != 1 {
		t.Fatal("journalctl not started once")
	}
	j.write("line one\n")
	time.Sleep(50 * time.Millisecond)
	frames = append(frames, recvN(t, st, 4)...)
	for i := 1; i < len(frames); i++ {
		gap := frames[i].at.Sub(frames[i-1].at)
		if gap < interval-20*time.Millisecond || gap > interval+budget+300*time.Millisecond {
			t.Fatalf("gap %d = %v", i, gap)
		}
		if frames[i].f.Timestamp <= frames[i-1].f.Timestamp {
			t.Fatal("timestamp not strictly increasing")
		}
	}
	if ts := frames[0].f.Timestamp; ts < t0.UnixMilli()-1000 || ts > time.Now().UnixMilli() {
		t.Fatalf("timestamp %d is not epoch ms", ts)
	}
	if peak := r.ans.maxConcurrent("awg show awg0 dump"); peak != 1 {
		t.Fatalf("%d concurrent awg probes", peak)
	}
	logs := 0
	for _, f := range frames {
		if f.f.SystemLogs == "line one\n" {
			logs++
		} else if f.f.SystemLogs != "" {
			t.Fatalf("system_logs %q", f.f.SystemLogs)
		}
	}
	if logs != 1 {
		t.Fatal("journal output must appear in exactly one frame")
	}

	cancel()
	select {
	case <-j.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("journalctl not stopped on cancel")
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return on cancel")
	}
}

// With the production timing: the first frame arrives within the budget even if a probe hangs
// (orchestrator watchdog: 5 s), the second Interval (2 s) after the first. A failed journalctl
// spawn does not break the stream; N streams are independent.
func TestStreamDefaultTiming(t *testing.T) {
	r := newRig(t)
	r.ans.slow("awg show awg0 dump", 4*time.Second)
	r.ans.slow("modinfo amneziawg", 4*time.Second)
	j := newFakeJournal()
	j.failErr = errors.New("spawn journalctl ENOENT")
	h := serve(t, &Service{Auth: auth.New(secret), Collector: r.col, Log: discard, Journal: j})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	a, err := h.client.StreamTelemetry(withSecret(ctx, secret), &agentpb.TelemetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.client.StreamTelemetry(withSecret(ctx, secret), &agentpb.TelemetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fbc := make(chan got, 1)
	go func() {
		f, err := b.Recv()
		if err == nil {
			fbc <- got{f: f, at: time.Now()}
		}
		close(fbc)
	}()
	fa := recvN(t, a, 2)
	fb := []got{<-fbc}
	if fb[0].f == nil {
		t.Fatal("second stream got no frame")
	}
	if d := fa[0].at.Sub(t0); d > StreamBudget+time.Second {
		t.Fatalf("first frame after %v", d)
	}
	if d := fb[0].at.Sub(t0); d > StreamBudget+time.Second {
		t.Fatalf("second stream's first frame after %v", d)
	}
	if gap := fa[1].at.Sub(fa[0].at); gap < Interval-50*time.Millisecond || gap > Interval+StreamBudget+500*time.Millisecond {
		t.Fatalf("gap %v", gap)
	}
	if j.startCount() != 2 || fa[0].f.SystemLogs != "" {
		t.Fatal("each stream starts its own tail; a failed spawn leaves logs empty")
	}
	if r.ans.count("awg show awg0 dump") != 1 {
		t.Fatalf("hung probe re-spawned: %d", r.ans.count("awg show awg0 dump"))
	}
}

// Checklist 20, 21.
func TestGetTelemetry(t *testing.T) {
	r := newRig(t)
	writeFile(t, r.fs, "/proc/stat", "cpu  100 0 0 300 0 0 0 0 0 0\n")
	r.ans.slow("awg show awg0 dump", 3*time.Second)
	h := serve(t, &Service{Auth: auth.New(secret), Collector: r.col, Log: discard, Journal: newFakeJournal()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bad, err := h.client.GetTelemetry(withSecret(ctx, "nope"), &agentpb.TelemetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if bad.SystemLogs != "Error: Unauthorized secret token mismatch" || bad.WebrtcStatus != "unauthorized" ||
		bad.AwgStatus != "unauthorized" || bad.SingboxVersion != "unknown" || bad.AwgVersion != "unknown" ||
		bad.AwgToolsVersion != "unknown" || bad.AwgGoVersion != "unknown" || bad.MeshAwgKernelStatus != "unknown" ||
		bad.MeshAwgKernelVersion != "" || bad.MeshTunnelInterfaceStatus != "unknown" || bad.CpuUsage != 0 ||
		bad.ActiveConnections != 0 || bad.HostNetIface != "" || bad.Timestamp <= 0 || len(r.fake.Calls()) != 0 {
		t.Fatalf("%v", bad)
	}

	t0 := time.Now()
	ok, err := h.client.GetTelemetry(ctx, &agentpb.TelemetryRequest{OrchestratorSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("GetTelemetry took %v (orchestrator display deadline 1 s)", d)
	}
	if ok.SystemLogs != "OK" || ok.CpuUsage != 25 || ok.Timestamp <= 0 || ok.AwgStatus != "nominal" {
		t.Fatalf("%v", ok) // cpu: zero baseline = since boot
	}
}

func TestLogBuffer(t *testing.T) {
	var b logBuffer
	b.write([]byte("héllo "))
	b.write([]byte{0xD0}) // first byte of "Ж" split across reads
	b.write([]byte{0x96, '\n'})
	if s := b.take(); s != "héllo Ж\n" {
		t.Fatalf("%q", s)
	}
	if s := b.take(); s != "" {
		t.Fatal("not cleared")
	}
	// An incomplete tail waits for the next frame.
	b.write([]byte("a\xF0\x9F"))
	if s := b.take(); s != "a" {
		t.Fatalf("%q", s)
	}
	b.write([]byte("\x98\x80"))
	if s := b.take(); s != "😀" {
		t.Fatalf("%q", s)
	}
	// Capped to the last 8192 UTF-16 units, at a code-point boundary.
	b.write([]byte(strings.Repeat("x", 10000) + "end"))
	s := b.take()
	if len(utf16.Encode([]rune(s))) != LogLimit || !strings.HasSuffix(s, "end") {
		t.Fatal(len(s))
	}
	b.write([]byte(strings.Repeat("😀", 5000)))
	s = b.take()
	if n := len(utf16.Encode([]rune(s))); n != LogLimit || strings.ContainsRune(s, '�') {
		t.Fatal(n)
	}
	b.write([]byte(strings.Repeat("Ж", 20000)))
	if s := b.take(); len(utf16.Encode([]rune(s))) != LogLimit || strings.ContainsRune(s, '�') {
		t.Fatal("cyrillic cap")
	}
}
