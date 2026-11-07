package rear

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/metadata"

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

	mu       sync.Mutex
	inactive map[string]bool        // unit → is-active answers "inactive"
	fail     map[string]string      // command display → stderr of a failure
	mihomoT  func(runner.Cmd) error // `mihomo -t` outcome
	sbValid  func(cfg any) (bool, string)
	sbChecks int
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
	h := &harness{t: t, fs: fsx.FS{Root: t.TempDir()}, inactive: map[string]bool{}, fail: map[string]string{}}
	h.fake = &runner.Fake{Handler: h.handle}
	h.svc = &Service{
		Auth: auth.New(secret), Runner: h.fake, FS: h.fs, Systemctl: systemd.Systemctl{Runner: h.fake}, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ValidateSingbox: func(_ context.Context, c any) (bool, string) {
			h.mu.Lock()
			h.sbChecks++
			f := h.sbValid
			h.mu.Unlock()
			if f != nil {
				return f(c)
			}
			return true, ""
		},
	}
	return h
}

func (h *harness) handle(c runner.Cmd) (runner.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if stderr, ok := h.fail[c.Display()]; ok {
		return runner.FailWith(c, 1, "", stderr)
	}
	if c.Name == "systemctl" && len(c.Args) == 2 && c.Args[0] == "is-active" {
		if h.inactive[c.Args[1]] {
			return runner.FailWith(c, 3, "inactive\n", "")
		}
		return runner.Result{Stdout: "active\n"}, nil
	}
	if len(c.Args) > 0 && c.Args[0] == "-t" && h.mihomoT != nil {
		if err := h.mihomoT(c); err != nil {
			return runner.FailWith(c, 1, "", err.Error())
		}
	}
	return runner.Result{}, nil
}

func (h *harness) path(p string) string { return h.fs.Path(p) }

func (h *harness) write(p, content string) {
	h.t.Helper()
	real := h.path(p)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read(p string) (string, bool) {
	b, err := os.ReadFile(h.path(p))
	return string(b), err == nil
}

func (h *harness) exists(p string) bool {
	_, err := os.Stat(h.path(p))
	return err == nil
}

// installBinaries puts both core binaries on disk (the "is it installed" precondition).
func (h *harness) installBinaries() {
	h.write("/usr/local/bin/sing-box", "bin")
	h.write("/usr/local/bin/mihomo", "bin")
}

func (h *harness) displays() []string { return h.fake.Displays() }

func (h *harness) ran(display string) bool {
	for _, d := range h.displays() {
		if d == display {
			return true
		}
	}
	return false
}

func (h *harness) index(display string) int {
	for i, d := range h.displays() {
		if d == display {
			return i
		}
	}
	return -1
}

func (h *harness) ranPrefix(prefix string) bool {
	for _, d := range h.displays() {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

func mdCtx(s string) context.Context {
	if s == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}
