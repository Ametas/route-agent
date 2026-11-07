package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "update-secret"

// release serves the same assets under /stable/ and /nightly/ (one base per channel, as the
// fixture configures them) and records which paths were asked for.
type release struct {
	binary    []byte
	checksums string
	hits      atomic.Int32

	mu    sync.Mutex
	paths []string
}

func (r *release) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.mu.Unlock()
		name := req.URL.Path
		if rest, ok := strings.CutPrefix(name, "/"+ChannelStable); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(name, "/"+ChannelNightly); ok {
			name = rest
		}
		switch name {
		case "/" + ChecksumsAsset:
			_, _ = io.WriteString(w, r.checksums)
		case "/" + AssetName("amd64"):
			_, _ = w.Write(r.binary)
		default:
			http.NotFound(w, req)
		}
	})
}

// requested returns the paths asked for so far.
func (r *release) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type fixture struct {
	u      *Updater
	fake   *runner.Fake
	bin    string
	srv    *httptest.Server
	rel    *release
	result chan error
}

func newFixture(t *testing.T, rel *release, versionOut string) *fixture {
	t.Helper()
	srv := httptest.NewServer(rel.handler(t))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	bin := filepath.Join(dir, "route-agent")
	if err := os.WriteFile(bin, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if len(c.Args) == 1 && c.Args[0] == "--version" {
			return runner.Result{Stdout: versionOut + "\n"}, nil
		}
		return runner.Result{}, nil
	}}
	f := &fixture{fake: fake, bin: bin, srv: srv, rel: rel, result: make(chan error, 4)}
	f.u = &Updater{
		Auth:       auth.New(secret),
		Runner:     fake,
		Systemctl:  systemd.Systemctl{Runner: fake},
		HTTP:       srv.Client(),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BinaryPath: bin,
		ReleaseBaseURLs: map[string]string{
			ChannelStable:  srv.URL + "/" + ChannelStable,
			ChannelNightly: srv.URL + "/" + ChannelNightly + "/",
		},
		Arch:   "amd64",
		Unit:   "route-agent",
		Delay:  10 * time.Millisecond,
		OnDone: func(err error) { f.result <- err },
	}
	return f
}

func md(v string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, v))
}

// mdChannel is a call with the secret and the x-agent-channel header.
func mdChannel(channel string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(auth.MetadataKey, secret, ChannelHeader, channel))
}

func (f *fixture) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("update did not finish")
		return nil
	}
}

func TestSuccessSwapsAndKeepsPrev(t *testing.T) {
	newBin := []byte("NEW-BINARY-CONTENT")
	rel := &release{binary: newBin, checksums: fmt.Sprintf("%s  %s\n%s  %s\n", sumOf([]byte("other")), AssetName("arm64"), sumOf(newBin), AssetName("amd64"))}
	f := newFixture(t, rel, "v1.5.0")

	resp, err := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if err != nil || !resp.Success || resp.Message != MsgInitiated || !strings.Contains(resp.Message, "Self-update sequence initiated") {
		t.Fatalf("ack: %v %+v", err, resp)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got, _ := os.ReadFile(f.bin); string(got) != string(newBin) {
		t.Fatalf("binary not swapped: %q", got)
	}
	if got, _ := os.ReadFile(PrevPath(f.bin)); string(got) != "OLD-BINARY" {
		t.Fatalf("prev not kept: %q", got)
	}
	if st, _ := os.Stat(f.bin); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	calls := f.fake.Calls()
	if len(calls) != 2 || calls[0].Args[0] != "--version" || calls[1].Name != "systemctl" ||
		strings.Join(calls[1].Args, " ") != "restart route-agent" {
		t.Fatalf("calls %v", f.fake.Displays())
	}
	for _, c := range calls {
		if c.Timeout <= 0 {
			t.Fatalf("command without timeout: %+v", c)
		}
	}
	// route-agent, route-agent.prev and the channel file (no channel file before → stable).
	entries, _ := os.ReadDir(filepath.Dir(f.bin))
	if len(entries) != 3 {
		t.Fatalf("leftover files: %v", entries)
	}
	if got, _ := os.ReadFile(ChannelPath(f.bin)); string(got) != "stable\n" {
		t.Fatalf("channel file %q", got)
	}
	for _, p := range f.rel.requested() {
		if !strings.HasPrefix(p, "/stable/") {
			t.Fatalf("no header and no channel file must use stable, asked for %s", p)
		}
	}
}

func TestChecksumMismatchDoesNotSwap(t *testing.T) {
	rel := &release{binary: []byte("TAMPERED"), checksums: sumOf([]byte("GENUINE")) + "  " + AssetName("amd64") + "\n"}
	f := newFixture(t, rel, "v1.5.0")
	if resp, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{}); !resp.Success {
		t.Fatalf("ack %+v", resp)
	}
	err := f.wait(t)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if got, _ := os.ReadFile(f.bin); string(got) != "OLD-BINARY" {
		t.Fatal("binary must stay untouched")
	}
	if _, err := os.Stat(PrevPath(f.bin)); !os.IsNotExist(err) {
		t.Fatal("no .prev on a failed update")
	}
	if len(f.fake.Calls()) != 0 {
		t.Fatalf("nothing may run on mismatch: %v", f.fake.Displays())
	}
	entries, _ := os.ReadDir(filepath.Dir(f.bin))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestBadVersionDoesNotSwap(t *testing.T) {
	newBin := []byte("NEW")
	rel := &release{binary: newBin, checksums: sumOf(newBin) + " *" + AssetName("amd64") + "\n"}
	f := newFixture(t, rel, "0.0.0-development")
	_, _ = f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if err := f.wait(t); err == nil || !strings.Contains(err.Error(), "not vX.Y.Z") {
		t.Fatalf("got %v", err)
	}
	if got, _ := os.ReadFile(f.bin); string(got) != "OLD-BINARY" {
		t.Fatal("binary must stay untouched")
	}
	if d := f.fake.Displays(); len(d) != 1 {
		t.Fatalf("only --version may run: %v", d)
	}
}

func TestMissingAssetEntry(t *testing.T) {
	rel := &release{binary: []byte("X"), checksums: sumOf([]byte("X")) + "  " + AssetName("arm64") + "\n"}
	f := newFixture(t, rel, "v1.5.0")
	_, _ = f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if err := f.wait(t); err == nil || !strings.Contains(err.Error(), "no entry for route-agent-linux-amd64") {
		t.Fatalf("got %v", err)
	}
}

func TestAuthAndLock(t *testing.T) {
	rel := &release{binary: []byte("X"), checksums: ""}
	f := newFixture(t, rel, "v1.5.0")

	resp, err := f.u.SelfUpdate(md("wrong"), &agentpb.SelfUpdatePayload{})
	if err != nil || resp.Success || resp.Message != "Invalid orchestrator secret token." {
		t.Fatalf("auth: %v %+v", err, resp)
	}
	resp, _ = f.u.SelfUpdate(context.Background(), &agentpb.SelfUpdatePayload{})
	if resp.Success {
		t.Fatal("no metadata must be rejected")
	}
	time.Sleep(50 * time.Millisecond)
	if rel.hits.Load() != 0 || len(f.fake.Calls()) != 0 {
		t.Fatal("rejected call must not start anything")
	}

	f.u.Delay = 300 * time.Millisecond
	first, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	second, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if first.Message != MsgInitiated || second.Message != MsgAlreadyRunning || !second.Success {
		t.Fatalf("lock: %+v / %+v", first, second)
	}
	_ = f.wait(t) // fails (empty checksums) but must release the lock
	select {
	case err := <-f.result:
		t.Fatalf("a second run happened: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	third, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if third.Message != MsgInitiated {
		t.Fatalf("lock not released: %+v", third)
	}
	_ = f.wait(t)
}

func validRelease() *release {
	newBin := []byte("NEW-BINARY-CONTENT")
	return &release{binary: newBin, checksums: sumOf(newBin) + "  " + AssetName("amd64") + "\n"}
}

func assertChannelPaths(t *testing.T, rel *release, channel string) {
	t.Helper()
	paths := rel.requested()
	if len(paths) != 2 {
		t.Fatalf("want checksums and binary, got %v", paths)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/"+channel+"/") {
			t.Fatalf("want channel %s, asked for %s", channel, p)
		}
	}
}

func TestNightlyHeaderUsesNightlyAndPersists(t *testing.T) {
	f := newFixture(t, validRelease(), "v1.5.0-3-gabc1234")
	resp, err := f.u.SelfUpdate(mdChannel(ChannelNightly), &agentpb.SelfUpdatePayload{})
	want := "Self-update sequence initiated (channel=nightly). Agent will pull code and restart in 1 second."
	if err != nil || !resp.Success || resp.Message != want {
		t.Fatalf("ack: %v %+v", err, resp)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	assertChannelPaths(t, f.rel, ChannelNightly)
	got, _ := os.ReadFile(ChannelPath(f.bin))
	if string(got) != "nightly\n" {
		t.Fatalf("channel file %q", got)
	}
	if st, _ := os.Stat(ChannelPath(f.bin)); st.Mode().Perm() != 0o644 {
		t.Fatalf("channel file mode %o", st.Mode().Perm())
	}
	if filepath.Dir(ChannelPath(f.bin)) != filepath.Dir(f.bin) {
		t.Fatal("channel file must sit next to the binary")
	}
}

func TestStableHeaderOverridesPersistedNightly(t *testing.T) {
	f := newFixture(t, validRelease(), "v1.5.0")
	if err := os.WriteFile(ChannelPath(f.bin), []byte("nightly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, _ := f.u.SelfUpdate(mdChannel(ChannelStable), &agentpb.SelfUpdatePayload{})
	if !strings.Contains(resp.Message, "channel=stable") {
		t.Fatalf("ack %+v", resp)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	assertChannelPaths(t, f.rel, ChannelStable)
	if got, _ := os.ReadFile(ChannelPath(f.bin)); string(got) != "stable\n" {
		t.Fatalf("channel file %q", got)
	}
}

func TestNoHeaderUsesPersistedChannel(t *testing.T) {
	f := newFixture(t, validRelease(), "v1.5.0-3-gabc1234")
	if err := os.WriteFile(ChannelPath(f.bin), []byte("nightly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if resp.Message != MsgInitiated {
		t.Fatalf("without the header the reply stays as before: %+v", resp)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	assertChannelPaths(t, f.rel, ChannelNightly)
	if got, _ := os.ReadFile(ChannelPath(f.bin)); string(got) != "nightly\n" {
		t.Fatalf("channel file %q", got)
	}
}

func TestGarbageChannelFileMeansStable(t *testing.T) {
	f := newFixture(t, validRelease(), "v1.5.0")
	if err := os.WriteFile(ChannelPath(f.bin), []byte("Nightly please\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if err := f.wait(t); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	assertChannelPaths(t, f.rel, ChannelStable)
	if got, _ := os.ReadFile(ChannelPath(f.bin)); string(got) != "stable\n" {
		t.Fatalf("channel file %q", got)
	}
}

func TestFailedNightlyUpdateKeepsChannelFile(t *testing.T) {
	rel := &release{binary: []byte("TAMPERED"), checksums: sumOf([]byte("GENUINE")) + "  " + AssetName("amd64") + "\n"}
	f := newFixture(t, rel, "v1.5.0")
	if err := os.WriteFile(ChannelPath(f.bin), []byte("stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = f.u.SelfUpdate(mdChannel(ChannelNightly), &agentpb.SelfUpdatePayload{})
	if err := f.wait(t); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if got, _ := os.ReadFile(ChannelPath(f.bin)); string(got) != "stable\n" {
		t.Fatalf("a failed update must not move the channel: %q", got)
	}
}

func TestUnknownChannelRefused(t *testing.T) {
	f := newFixture(t, validRelease(), "v1.5.0")
	long := "nightly; rm -rf / && echo " + strings.Repeat("x", 40)
	cases := map[string]string{
		"Nightly": "Unknown update channel 'Nightly'.",
		"":        "Unknown update channel ''.",
		"preview": "Unknown update channel 'preview'.",
		long:      "Unknown update channel '" + long[:32] + "'.",
	}
	for value, want := range cases {
		resp, err := f.u.SelfUpdate(mdChannel(value), &agentpb.SelfUpdatePayload{})
		if err != nil || resp.Success || resp.Message != want {
			t.Fatalf("%q: %v %+v", value, err, resp)
		}
	}
	// A wrong secret gets the auth refusal before the channel is looked at.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, "wrong", ChannelHeader, "beta"))
	if resp, _ := f.u.SelfUpdate(ctx, &agentpb.SelfUpdatePayload{}); resp.Message != auth.InvalidSecretMessage {
		t.Fatalf("auth first: %+v", resp)
	}
	time.Sleep(50 * time.Millisecond)
	if f.rel.hits.Load() != 0 || len(f.fake.Calls()) != 0 {
		t.Fatal("a refused channel must not start anything")
	}
	if _, err := os.Stat(ChannelPath(f.bin)); !os.IsNotExist(err) {
		t.Fatal("a refused channel must not write the channel file")
	}
	// The lock was not taken.
	if resp, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{}); resp.Message != MsgInitiated {
		t.Fatalf("lock taken by a refused call: %+v", resp)
	}
	_ = f.wait(t)
}

func TestAlreadyRunningWithHeader(t *testing.T) {
	rel := &release{binary: []byte("X"), checksums: ""}
	f := newFixture(t, rel, "v1.5.0")
	f.u.Delay = 300 * time.Millisecond
	first, _ := f.u.SelfUpdate(mdChannel(ChannelNightly), &agentpb.SelfUpdatePayload{})
	second, _ := f.u.SelfUpdate(mdChannel(ChannelStable), &agentpb.SelfUpdatePayload{})
	third, _ := f.u.SelfUpdate(md(secret), &agentpb.SelfUpdatePayload{})
	if !strings.Contains(first.Message, "(channel=nightly)") {
		t.Fatalf("first %+v", first)
	}
	if !second.Success || second.Message != "Self-update sequence already in progress (channel=nightly)." {
		t.Fatalf("second must name the running update's channel: %+v", second)
	}
	if third.Message != MsgAlreadyRunning {
		t.Fatalf("without the header the reply stays as before: %+v", third)
	}
	_ = f.wait(t)
}

func TestChannelFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ChannelFileName)
	if got := ReadChannel(p); got != ChannelStable {
		t.Fatalf("missing file: %q", got)
	}
	for content, want := range map[string]string{
		"nightly\n":    ChannelNightly,
		"  nightly \n": ChannelNightly,
		"stable":       ChannelStable,
		"":             ChannelStable,
		"preview\n":    ChannelStable,
		"NIGHTLY\n":    ChannelStable,
		"nightly\nx\n": ChannelStable,
	} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadChannel(p); got != want {
			t.Fatalf("%q: got %q, want %q", content, got, want)
		}
	}
	if err := WriteChannel(p, "beta"); err == nil {
		t.Fatal("unknown channel must not be written")
	}
	if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteChannel(p, ChannelNightly); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "nightly\n" {
		t.Fatalf("written %q", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if ReleaseBaseURLFor(ChannelNightly) != "https://github.com/Ametas/route-agent/releases/download/nightly" ||
		ReleaseBaseURLFor(ChannelStable) != "https://github.com/Ametas/route-agent/releases/latest/download" {
		t.Fatal("default release URLs changed")
	}
}
