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

type release struct {
	binary    []byte
	checksums string
	hits      atomic.Int32
}

func (r *release) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		switch req.URL.Path {
		case "/" + ChecksumsAsset:
			_, _ = io.WriteString(w, r.checksums)
		case "/" + AssetName("amd64"):
			_, _ = w.Write(r.binary)
		default:
			http.NotFound(w, req)
		}
	})
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
		Auth:           auth.New(secret),
		Runner:         fake,
		Systemctl:      systemd.Systemctl{Runner: fake},
		HTTP:           srv.Client(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		BinaryPath:     bin,
		ReleaseBaseURL: srv.URL,
		Arch:           "amd64",
		Unit:           "route-agent",
		Delay:          10 * time.Millisecond,
		OnDone:         func(err error) { f.result <- err },
	}
	return f
}

func md(v string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, v))
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
	entries, _ := os.ReadDir(filepath.Dir(f.bin))
	if len(entries) != 2 {
		t.Fatalf("leftover files: %v", entries)
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
