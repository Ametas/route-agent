package awg

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/locks"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHealth(e *env) (*Health, *clock) {
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	return &Health{Runner: e.run, FS: e.fs, Systemctl: e.svc.Systemctl, Cfg: e.cfg, Log: quietLog(), Now: c.now}, c
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// Item 12 (strings) and 13 (cooldown), mirroring tests/app.test.ts:764-793.
func TestHealthCheckFlow(t *testing.T) {
	e := newEnv(t)
	h, c := newHealth(e)
	if h.Status() != "unknown" {
		t.Fatalf("before the first check: %q", h.Status())
	}

	// Not configured: no file, unit not enabled.
	e.sc.on("systemctl is-enabled route-awg@awg0", outcome{stdout: "disabled\n", code: 1})
	h.Check(context.Background())
	if h.Status() != "inactive" {
		t.Fatalf("%q", h.Status())
	}
	// Enabled unit alone counts as configured.
	e.sc.ok("systemctl is-enabled route-awg@awg0", "enabled\n")
	h.Check(context.Background())
	if h.Status() != "nominal" {
		t.Fatalf("%q", h.Status())
	}

	e.write(DefaultConfigPath, "[Interface]\n")
	e.sc.fail("ip link show awg0", "Device \"awg0\" does not exist.")
	h.Check(context.Background())
	if st := h.State(); st.AutoRestarts != 1 || h.Status() != "crashed (restarted 0m ago)" {
		t.Fatalf("%+v %q", st, h.Status())
	}
	c.add(time.Minute)
	h.Check(context.Background()) // within the cooldown
	if st := h.State(); st.AutoRestarts != 1 || h.Status() != "crashed (restarted 1m ago)" {
		t.Fatalf("%+v %q", st, h.Status())
	}
	if n := countCalls(e.calls(), "systemctl restart route-awg@awg0"); n != 1 {
		t.Fatalf("restarts %d", n)
	}
	e.sc.ok("ip link show awg0", "7: awg0: <POINTOPOINT,NOARP,UP,LOWER_UP>")
	c.add(2 * time.Minute)
	h.Check(context.Background())
	if !strings.HasPrefix(h.Status(), "nominal (restarted") || h.Status() != "nominal (restarted 3m ago)" {
		t.Fatalf("%q", h.Status())
	}
	c.add(2 * time.Hour)
	if h.Status() != "nominal (restarted 2h ago)" {
		t.Fatalf("%q", h.Status())
	}
	c.add(23 * time.Hour)
	if h.Status() != "nominal" {
		t.Fatalf(">24 h: %q", h.Status())
	}
	// The mesh interface is never touched (item 14).
	for _, call := range e.calls() {
		if strings.Contains(call, "awgmesh0") {
			t.Fatalf("mesh touched: %s", call)
		}
	}
}

// Deviation: a FAILED restart also starts the cooldown (TS retried on every telemetry tick).
func TestHealthFailedRestartCooldown(t *testing.T) {
	e := newEnv(t)
	h, c := newHealth(e)
	e.write(DefaultConfigPath, "[Interface]\n")
	e.sc.fail("ip link show awg0", "no device")
	e.sc.fail("systemctl restart route-awg@awg0", "boom")
	h.Check(context.Background())
	c.add(30 * time.Second)
	h.Check(context.Background())
	if n := countCalls(e.calls(), "systemctl restart route-awg@awg0"); n != 1 {
		t.Fatalf("restarts within cooldown after a failure: %d", n)
	}
	if h.Status() != "crashed" || h.State().AutoRestarts != 0 {
		t.Fatalf("%q %+v", h.Status(), h.State())
	}
	c.add(5 * time.Minute)
	h.Check(context.Background())
	if n := countCalls(e.calls(), "systemctl restart route-awg@awg0"); n != 2 {
		t.Fatalf("retry after the cooldown: %d", n)
	}
}

func TestFormatStatus(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		st   HealthState
		want string
	}{
		{HealthState{}, "unknown"},
		{HealthState{Checked: true}, "inactive"},
		{HealthState{Checked: true, Configured: true, Alive: true}, "nominal"},
		{HealthState{Checked: true, Configured: true}, "crashed"},
		{HealthState{Checked: true, Configured: true, LastAutoRestart: now.Add(-59*time.Minute - 59*time.Second)}, "crashed (restarted 59m ago)"},
		{HealthState{Checked: true, Configured: true, LastAutoRestart: now.Add(-60 * time.Minute)}, "crashed (restarted 1h ago)"},
		{HealthState{Checked: true, Configured: true, LastAutoRestart: now.Add(-48 * time.Hour)}, "crashed (restarted 48h ago)"},
		{HealthState{Checked: true, Configured: true, Alive: true, LastAutoRestart: now.Add(-23 * time.Hour)}, "nominal (restarted 23h ago)"},
		{HealthState{Checked: true, Configured: true, Alive: true, LastAutoRestart: now.Add(-24 * time.Hour)}, "nominal"},
		{HealthState{Checked: true, Configured: true, Alive: true, LastAutoRestart: now.Add(time.Second)}, "nominal (restarted 0m ago)"},
	}
	for _, c := range cases {
		if got := FormatStatus(c.st, now); got != c.want {
			t.Errorf("%+v: %q want %q", c.st, got, c.want)
		}
	}
}

// Tick never runs under an apply holding the AWG area.
func TestHealthTickSkipsWhileAWGBusy(t *testing.T) {
	e := newEnv(t)
	reg := locks.New()
	h, _ := newHealth(e)
	h.Locks = reg
	_, unlock, err := reg.Lock(context.Background(), locks.AWG)
	if err != nil {
		t.Fatal(err)
	}
	h.Tick(context.Background())
	if len(e.calls()) != 0 || h.State().Checked {
		t.Fatalf("tick ran under the AWG lock: %v", e.calls())
	}
	unlock()
	h.Tick(context.Background())
	if !h.State().Checked {
		t.Fatal("tick did not run once the area was free")
	}
}
