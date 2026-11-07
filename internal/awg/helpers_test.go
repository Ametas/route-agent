package awg

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/firewall"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "0123456789abcdef0123456789abcdef"

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func authed() context.Context { return metadataCtx(secret) }

func metadataCtx(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// script answers commands by their Display(): an exact entry, else the longest matching prefix
// entry, else success with empty output.
type script struct {
	mu    sync.Mutex
	exact map[string]outcome
	pre   map[string]outcome
}

type outcome struct {
	stdout, stderr string
	code           int // non-zero → failure
}

func newScript() *script { return &script{exact: map[string]outcome{}, pre: map[string]outcome{}} }

func (s *script) on(display string, o outcome)      { s.mu.Lock(); s.exact[display] = o; s.mu.Unlock() }
func (s *script) onPrefix(prefix string, o outcome) { s.mu.Lock(); s.pre[prefix] = o; s.mu.Unlock() }
func (s *script) fail(display, stderr string)       { s.on(display, outcome{stderr: stderr, code: 1}) }
func (s *script) failPrefix(prefix, stderr string) {
	s.onPrefix(prefix, outcome{stderr: stderr, code: 1})
}
func (s *script) ok(display, stdout string) { s.on(display, outcome{stdout: stdout}) }

func (s *script) handle(c runner.Cmd) (runner.Result, error) {
	s.mu.Lock()
	o, found := s.exact[c.Display()]
	if !found {
		best := ""
		for p, po := range s.pre {
			if strings.HasPrefix(c.Display(), p) && len(p) > len(best) {
				best, o, found = p, po, true
			}
		}
	}
	s.mu.Unlock()
	if found && o.code != 0 {
		return runner.FailWith(c, o.code, o.stdout, o.stderr)
	}
	return runner.Result{Stdout: o.stdout}, nil
}

type env struct {
	t    *testing.T
	root string
	fs   fsx.FS
	run  *runner.Fake
	sc   *script
	svc  *Service
	cfg  *config.Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	sc := newScript()
	fake := &runner.Fake{Handler: sc.handle}
	cfg := &config.Config{AwgConfigPath: DefaultConfigPath, AwgToolsBinaryPath: "/usr/local/bin/awg"}
	f := fsx.FS{Root: root}
	fw := &firewall.Service{Auth: auth.New(secret), Runner: fake, Log: quietLog(),
		LookPath: func(string) (string, error) { return "/usr/sbin/ufw", nil }}
	svc := &Service{Auth: auth.New(secret), Runner: fake, FS: f, Systemctl: systemd.Systemctl{Runner: fake},
		Cfg: cfg, Log: quietLog(), Firewall: fw}
	return &env{t: t, root: root, fs: f, run: fake, sc: sc, svc: svc, cfg: cfg}
}

func (e *env) read(p string) string {
	e.t.Helper()
	b, err := os.ReadFile(e.fs.Path(p))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) write(p, content string) {
	e.t.Helper()
	full := e.fs.Path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) calls() []string { return e.run.Displays() }

func has(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func hasPrefix(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func index(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0) }
