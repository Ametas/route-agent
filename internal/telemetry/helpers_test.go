package telemetry

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/mesh"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/singbox"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "telemetry-secret"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// answers scripts the fake runner by command display; a missing entry fails with exit 1.
type answers struct {
	mu    sync.Mutex
	out   map[string]runner.FakeResult
	delay map[string]time.Duration
	// running counts concurrent calls per display; peak is the maximum seen.
	running map[string]int
	peak    map[string]int
	calls   map[string]int
}

func newAnswers() *answers {
	return &answers{out: map[string]runner.FakeResult{}, delay: map[string]time.Duration{},
		running: map[string]int{}, peak: map[string]int{}, calls: map[string]int{}}
}

func (a *answers) ok(display, stdout string) {
	a.mu.Lock()
	a.out[display] = runner.FakeResult{Result: runner.Result{Stdout: stdout}}
	a.mu.Unlock()
}

func (a *answers) fail(display string) {
	a.mu.Lock()
	delete(a.out, display)
	a.mu.Unlock()
}

func (a *answers) slow(display string, d time.Duration) {
	a.mu.Lock()
	a.delay[display] = d
	a.mu.Unlock()
}

func (a *answers) count(display string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[display]
}

func (a *answers) maxConcurrent(display string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.peak[display]
}

func (a *answers) handle(c runner.Cmd) (runner.Result, error) {
	d := c.Display()
	a.mu.Lock()
	a.calls[d]++
	a.running[d]++
	if a.running[d] > a.peak[d] {
		a.peak[d] = a.running[d]
	}
	delay := a.delay[d]
	r, ok := a.out[d]
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running[d]--
		a.mu.Unlock()
	}()
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		return runner.FailWith(c, 1, "", "failed")
	}
	return r.Result, r.Err
}

type fakeHealth struct{ s string }

func (f fakeHealth) Status() string { return f.s }

// clock is a settable fake clock.
type clock struct{ ns atomic.Int64 }

func newClock(t time.Time) *clock { c := &clock{}; c.ns.Store(t.UnixNano()); return c }
func (c *clock) now() time.Time   { return time.Unix(0, c.ns.Load()) }
func (c *clock) add(d time.Duration) {
	c.ns.Add(int64(d))
}

type rig struct {
	ans  *answers
	fake *runner.Fake
	fs   fsx.FS
	cfg  *config.Config
	clk  *clock
	col  *Collector
}

func newRig(t *testing.T) *rig {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": secret})
	if err != nil {
		t.Fatal(err)
	}
	ans := newAnswers()
	fake := &runner.Fake{Handler: ans.handle}
	fs := fsx.FS{Root: t.TempDir()}
	clk := newClock(time.Unix(1_790_000_000, 0))
	sb := &singbox.Service{Runner: fake, FS: fs, Systemctl: systemd.Systemctl{Runner: fake}, Cfg: cfg, Log: discard}
	ms := &mesh.Status{Runner: fake, Cfg: cfg, Now: clk.now, LinkExists: SysfsLinkExists(fs)}
	col := &Collector{
		Runner: fake, FS: fs, Cfg: cfg, Log: discard, AwgHealth: fakeHealth{"nominal"}, Mesh: ms,
		SingboxVersion: sb.Version, Now: clk.now,
	}
	return &rig{ans: ans, fake: fake, fs: fs, cfg: cfg, clk: clk, col: col}
}

func (r *rig) frame(t *testing.T) *frameResult {
	t.Helper()
	f, next := r.col.Frame(context.Background(), CPUStats{}, 2*time.Second)
	return &frameResult{f: f, next: next}
}
