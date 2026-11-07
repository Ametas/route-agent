package shaper

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const secret = "0123456789abcdef0123456789abcdef"

// Verbatim kernel 6.6.87.2 output (tests/trafficShaper.test.ts).
const realTC = "filter protocol ip pref 1 u32 chain 0\n" +
	"filter protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1\n" +
	"filter protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 terminal flowid not_in_hw (rule hit 710 success 710)\n" +
	"  match 0a630000/ffffff00 at 12 (success 710 )\n" +
	" police 0x1 rate 8Mbit burst 100Kb mtu 2Kb action drop overhead 0b\n" +
	"\tref 1 bind 1  installed 5 sec used 0 sec firstused 5 sec\n" +
	"\n" +
	" Sent 3351952 bytes 2294 pkts (dropped 95, overlimits 95)\n"

const threeTC = `filter protocol ip pref 1 u32 chain 0
filter protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1
filter protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 100 bytes 2 pkts (dropped 1, overlimits 1)
filter protocol ip pref 2 u32 chain 0
filter protocol ip pref 2 u32 chain 0 fh 801: ht divisor 1
filter protocol ip pref 2 u32 chain 0 fh 801::800 order 2048 key ht 801 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 200 bytes 4 pkts (dropped 3, overlimits 3)
filter protocol ipv6 pref 3 u32 chain 0
filter protocol ipv6 pref 3 u32 chain 0 fh 802: ht divisor 1
filter protocol ipv6 pref 3 u32 chain 0 fh 802::800 order 2048 key ht 802 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 300 bytes 6 pkts (dropped 5, overlimits 5)
`

// Item 40.
func TestParseFilterCounters(t *testing.T) {
	c := ParseFilterCounters(realTC, []string{"10.99.0.0/24"})
	if !reflect.DeepEqual(c, []Counter{{"10.99.0.0/24", 3351952, 2294, 95}}) {
		t.Fatalf("%+v", c)
	}
	c = ParseFilterCounters(threeTC, []string{"198.51.100.0/24", "203.0.113.0/24", "2001:db8::/48"})
	if len(c) != 3 || c[0].DroppedPackets != 1 || c[1].DroppedPackets != 3 || c[2].Prefix != "2001:db8::/48" || c[2].DroppedPackets != 5 {
		t.Fatalf("%+v", c)
	}
	if c := ParseFilterCounters("filter protocol ip pref 7 u32 chain 0 fh 800::800 terminal flowid not_in_hw\n Sent 999 bytes 9 pkts (dropped 9, overlimits 9)\n", []string{"203.0.113.0/24"}); len(c) != 0 {
		t.Fatal("unknown pref attributed")
	}
	if c := ParseFilterCounters(" Sent 500 bytes 5 pkts (dropped 5, overlimits 5)\n", []string{"a/24"}); len(c) != 0 {
		t.Fatal("stray Sent")
	}
	if c := ParseFilterCounters("", []string{"x"}); len(c) != 0 {
		t.Fatal("empty")
	}
}

func TestIsSame(t *testing.T) {
	e := func(p, r, b string) Entry { return Entry{p, r, b} }
	base := State{Interface: "eth0", Entries: []Entry{e("a/24", "8mbit", "500k"), e("b/24", "8mbit", "500k")}}
	if !IsSame(&base, base) || IsSame(nil, base) {
		t.Fatal("basic")
	}
	for name, other := range map[string]State{
		"rate":      {Interface: "eth0", Entries: []Entry{e("a/24", "1mbit", "500k"), e("b/24", "8mbit", "500k")}},
		"burst":     {Interface: "eth0", Entries: []Entry{e("a/24", "8mbit", "2m"), e("b/24", "8mbit", "500k")}},
		"interface": {Interface: "ens3", Entries: base.Entries},
		"order":     {Interface: "eth0", Entries: []Entry{base.Entries[1], base.Entries[0]}},
		"added":     {Interface: "eth0", Entries: append(append([]Entry{}, base.Entries...), e("c/24", "8mbit", "500k"))},
	} {
		if IsSame(&base, other) {
			t.Errorf("%s must differ", name)
		}
	}
	inc := base
	inc.Incomplete = true
	if IsSame(&inc, base) {
		t.Fatal("an incomplete layout must be rebuilt")
	}
}

// Item 41: the order Node 22's localeCompare gives (generated with node -e, en-US ICU).
func TestNormalizeEntriesLocaleOrder(t *testing.T) {
	in := []string{"2001:db8:1::/48", "b::/16", "100.0.0.0/8", "A::/16", "1.2.3.0/24", "2001:db8::/48",
		"a::/16", "10.0.0.0/8", "1::/64", "192.168.10.0/24", "192.168.1.0/24", "::/0", "FD00::/8", "fd00::/8"}
	want := []string{"::/0", "1::/64", "1.2.3.0/24", "10.0.0.0/8", "100.0.0.0/8", "192.168.1.0/24", "192.168.10.0/24",
		"2001:db8::/48", "2001:db8:1::/48", "a::/16", "A::/16", "b::/16", "fd00::/8", "FD00::/8"}
	var es []Entry
	for _, p := range in {
		es = append(es, Entry{Prefix: p})
	}
	var got []string
	for _, e := range NormalizeEntries(es) {
		got = append(got, e.Prefix)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

type tcEnv struct {
	fake  *runner.Fake
	svc   *Service
	root  string
	fail  map[string]string
	route string
	show  string
}

func newTC(t *testing.T) *tcEnv {
	e := &tcEnv{root: t.TempDir(), fail: map[string]string{}, route: "default via 203.0.113.1 dev eth0 proto dhcp metric 100\n"}
	e.fake = &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if msg, ok := e.fail[c.Display()]; ok {
			return runner.FailWith(c, 2, "", msg)
		}
		switch {
		case c.Display() == "ip -o route show default":
			if e.route == "" {
				return runner.Result{}, nil
			}
			return runner.Result{Stdout: e.route}, nil
		case strings.HasPrefix(c.Display(), "tc -s filter show"):
			return runner.Result{Stdout: e.show}, nil
		}
		return runner.Result{}, nil
	}}
	e.svc = &Service{Auth: auth.New(secret), Runner: e.fake, FS: fsx.FS{Root: e.root}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return e
}

func (e *tcEnv) state() string {
	b, err := os.ReadFile(filepath.Join(e.root, DefaultStatePath))
	if err != nil {
		return ""
	}
	return string(b)
}

func md() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, secret))
}

func req(entries ...*agentpb.TrafficThrottleEntry) *agentpb.TrafficThrottleRequest {
	return &agentpb.TrafficThrottleRequest{Entries: entries}
}

func ent(p, r, b string) *agentpb.TrafficThrottleEntry {
	return &agentpb.TrafficThrottleEntry{Prefix: p, Rate: r, Burst: b}
}

// Items 37, 38, 34: rebuild sequence, state bytes, Unchanged without tc calls, counters first.
func TestApplyRebuildThenUnchanged(t *testing.T) {
	e := newTC(t)
	r, _ := e.svc.ApplyTrafficThrottle(md(), req(ent(" 2001:db8::/48 ", "1mbit", "500k"), ent("203.0.113.0/24", "5mbit", "500k"), ent("  ", "", "")))
	if !r.Success || r.Message != "Applied" || !r.Changed || r.Applied != 2 || len(r.Counters) != 0 {
		t.Fatalf("%+v", r)
	}
	want := []string{
		"ip -o route show default",
		"tc qdisc replace dev eth0 handle ffff: ingress",
		"tc filter del dev eth0 parent ffff:",
		"tc filter add dev eth0 parent ffff: protocol ipv6 prio 1 u32 match ip6 src 2001:db8::/48 police rate 1mbit burst 500k conform-exceed drop/ok",
		"tc filter add dev eth0 parent ffff: protocol ip prio 2 u32 match ip src 203.0.113.0/24 police rate 5mbit burst 500k conform-exceed drop/ok",
	}
	if got := e.fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", strings.Join(got, "\n"))
	}
	wantState := "{\n  \"interface\": \"eth0\",\n  \"entries\": [\n    {\n      \"prefix\": \"2001:db8::/48\",\n      \"rate\": \"1mbit\",\n      \"burst\": \"500k\"\n    },\n" +
		"    {\n      \"prefix\": \"203.0.113.0/24\",\n      \"rate\": \"5mbit\",\n      \"burst\": \"500k\"\n    }\n  ]\n}\n"
	if e.state() != wantState {
		t.Fatalf("%q", e.state())
	}

	n := len(e.fake.Calls())
	e.show = threeTC
	r, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("203.0.113.0/24", "5mbit", "500k"), ent("2001:db8::/48", "1mbit", "500k")))
	if !r.Success || r.Message != "Unchanged" || r.Changed || r.Applied != 2 {
		t.Fatalf("%+v", r)
	}
	if len(r.Counters) != 2 || r.Counters[0].Prefix != "2001:db8::/48" || r.Counters[1].DroppedPackets != 3 {
		t.Fatalf("counters %+v", r.Counters)
	}
	after := e.fake.Displays()[n:]
	if !reflect.DeepEqual(after, []string{"tc -s filter show dev eth0 parent ffff:", "ip -o route show default"}) {
		t.Fatalf("%v", after)
	}
}

// Items 35, 34: empty list clears (changed=true) and returns the pre-read counters.
func TestClear(t *testing.T) {
	e := newTC(t)
	_, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("10.99.0.0/24", "8mbit", "500k")))
	e.show = realTC
	n := len(e.fake.Calls())
	r, _ := e.svc.ApplyTrafficThrottle(md(), req())
	if !r.Success || r.Message != "Cleared" || !r.Changed || r.Applied != 0 || len(r.Counters) != 1 || r.Counters[0].SentBytes != 3351952 {
		t.Fatalf("%+v", r)
	}
	if got := e.fake.Displays()[n:]; !reflect.DeepEqual(got, []string{"tc -s filter show dev eth0 parent ffff:", "tc qdisc del dev eth0 ingress"}) {
		t.Fatalf("%v", got)
	}
	if e.state() != "" {
		t.Fatal("state not removed")
	}
	// Steady state: nothing stored → resolve the interface, still Cleared/changed.
	r, _ = e.svc.ApplyTrafficThrottle(md(), req())
	if !r.Success || !r.Changed || r.Message != "Cleared" {
		t.Fatalf("%+v", r)
	}
}

// Item 36.
func TestIncompleteEntries(t *testing.T) {
	e := newTC(t)
	r, _ := e.svc.ApplyTrafficThrottle(md(), req(ent("a/24", "8mbit", ""), ent("b/24", "", "5k"), ent("c/24", "1mbit", "5k")))
	if r.Success || r.Message != "Entries without rate/burst: a/24, b/24" || r.Changed || r.Applied != 0 {
		t.Fatalf("%+v", r)
	}
	for _, c := range e.fake.Displays() {
		if !strings.HasPrefix(c, "tc -s") && strings.HasPrefix(c, "tc ") {
			t.Fatalf("touched tc: %s", c)
		}
	}
}

// Item 39 (kept): no default route → success:true.
func TestNoDefaultRoute(t *testing.T) {
	e := newTC(t)
	e.route = ""
	r, _ := e.svc.ApplyTrafficThrottle(md(), req(ent("a/24", "8mbit", "5k")))
	if !r.Success || r.Message != "No default-route interface found" || r.Changed || r.Applied != 0 {
		t.Fatalf("%+v", r)
	}
}

// Deviation (spec D R10): a failed filter does not freeze the layout as "Unchanged" — it is
// retried, but an identical delivery rebuilds it at most once per IncompleteRetryInterval.
func TestFailedFilterIsRetried(t *testing.T) {
	e := newTC(t)
	clock := time.UnixMilli(1_788_000_000_000)
	e.svc.Now = func() time.Time { return clock }
	bad := "tc filter add dev eth0 parent ffff: protocol ip prio 2 u32 match ip src bad police rate 1mbit burst 5k conform-exceed drop/ok"
	e.fail[bad] = "Illegal \"match\""
	rq := req(ent("10.0.0.0/8", "1mbit", "5k"), ent("bad", "1mbit", "5k"))
	r, _ := e.svc.ApplyTrafficThrottle(md(), rq)
	if !r.Success || r.Applied != 1 || !r.Changed || r.Message != "Applied" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(e.state(), `"incomplete": true`) || !strings.Contains(e.state(), `"lastAttemptMs": 1788000000000`) {
		t.Fatalf("%s", e.state())
	}

	// Within the interval: TS's Unchanged answer, no tc rebuild (drop counters survive).
	for _, d := range []time.Duration{time.Second, IncompleteRetryInterval - time.Millisecond} {
		clock = time.UnixMilli(1_788_000_000_000).Add(d)
		n := len(e.fake.Calls())
		r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
		if !r.Success || r.Message != "Unchanged" || r.Changed || r.Applied != 2 {
			t.Fatalf("+%v: %+v", d, r)
		}
		for _, c := range e.fake.Displays()[n:] {
			if strings.HasPrefix(c, "tc qdisc") || strings.HasPrefix(c, "tc filter add") || strings.HasPrefix(c, "tc filter del") {
				t.Fatalf("+%v: rebuilt (%s)", d, c)
			}
		}
	}

	// The bound survives a restart: a fresh Service reads the stamp from the state file.
	e.svc = &Service{Auth: e.svc.Auth, Runner: e.fake, FS: e.svc.FS, Log: e.svc.Log, Now: e.svc.Now}
	r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
	if r.Message != "Unchanged" {
		t.Fatalf("after restart: %+v", r)
	}

	// Interval elapsed and the prefix still rejected: rebuilt once more, new stamp.
	clock = time.UnixMilli(1_788_000_000_000).Add(IncompleteRetryInterval)
	r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
	if r.Message != "Applied" || !r.Changed || r.Applied != 1 || !strings.Contains(e.state(), `"lastAttemptMs": 1788000600000`) {
		t.Fatalf("retry: %+v %s", r, e.state())
	}
	r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
	if r.Message != "Unchanged" {
		t.Fatalf("second retry not deferred: %+v", r)
	}

	// Next retry after the interval succeeds: complete layout, stamp gone, then plain Unchanged.
	delete(e.fail, bad)
	clock = clock.Add(IncompleteRetryInterval)
	r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
	if r.Message != "Applied" || r.Applied != 2 || strings.Contains(e.state(), "incomplete") || strings.Contains(e.state(), "lastAttemptMs") {
		t.Fatalf("not retried: %+v %s", r, e.state())
	}
	r, _ = e.svc.ApplyTrafficThrottle(md(), rq)
	if r.Message != "Unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestIncompleteRetryBound(t *testing.T) {
	e := newTC(t)
	t0 := time.UnixMilli(1_788_000_000_000)
	clock := t0
	e.svc.Now = func() time.Time { return clock }
	bad := "tc filter add dev eth0 parent ffff: protocol ip prio 1 u32 match ip src bad police rate 1mbit burst 5k conform-exceed drop/ok"
	e.fail[bad] = "Illegal \"match\""
	e.fail[strings.Replace(bad, "1mbit", "2mbit", 1)] = "Illegal \"match\""
	_, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("bad", "1mbit", "5k")))

	// A changed layout is not held back by the stamp.
	clock = t0.Add(time.Minute)
	r, _ := e.svc.ApplyTrafficThrottle(md(), req(ent("bad", "2mbit", "5k")))
	if r.Message != "Applied" || !r.Changed {
		t.Fatalf("changed layout deferred: %+v", r)
	}

	// Clock went backwards: the retry is allowed rather than stuck until the clock catches up.
	clock = t0.Add(-time.Hour)
	r, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("bad", "2mbit", "5k")))
	if r.Message != "Applied" {
		t.Fatalf("backwards clock: %+v", r)
	}

	// A state file from an earlier build (incomplete, no stamp) is retried once, then stamped.
	if err := os.WriteFile(filepath.Join(e.root, DefaultStatePath),
		[]byte(`{"interface":"eth0","entries":[{"prefix":"bad","rate":"2mbit","burst":"5k"}],"incomplete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("bad", "2mbit", "5k")))
	if r.Message != "Applied" {
		t.Fatalf("unstamped: %+v", r)
	}
	r, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("bad", "2mbit", "5k")))
	if r.Message != "Unchanged" || r.Applied != 1 || r.Changed {
		t.Fatalf("stamped: %+v", r)
	}

	if RetryDeferred(nil, State{}, t0) {
		t.Fatal("nil state")
	}
}

// Deviation: moving to another interface removes the old ingress qdisc.
func TestInterfaceMove(t *testing.T) {
	e := newTC(t)
	_, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("a/24", "1mbit", "5k")))
	e.route = "default via 1.1.1.1 dev ens3\n"
	n := len(e.fake.Calls())
	_, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("a/24", "1mbit", "5k")))
	got := e.fake.Displays()[n:]
	if len(got) < 3 || got[2] != "tc qdisc del dev eth0 ingress" || got[3] != "tc qdisc replace dev ens3 handle ffff: ingress" {
		t.Fatalf("%v", got)
	}
}

// Item 34: an exception discards the counters.
func TestQdiscFailure(t *testing.T) {
	e := newTC(t)
	_, _ = e.svc.ApplyTrafficThrottle(md(), req(ent("a/24", "1mbit", "5k")))
	e.show = "filter protocol ip pref 1 u32\n Sent 1 bytes 1 pkt (dropped 1, overlimits 1)\n"
	e.fail["tc qdisc replace dev eth0 handle ffff: ingress"] = "RTNETLINK answers: Operation not permitted"
	r, _ := e.svc.ApplyTrafficThrottle(md(), req(ent("b/24", "1mbit", "5k")))
	want := "Command failed: tc qdisc replace dev eth0 handle ffff: ingress\nRTNETLINK answers: Operation not permitted"
	if r.Success || r.Message != want || len(r.Counters) != 0 || r.Changed {
		t.Fatalf("%+v", r)
	}
}

func TestUnauthorized(t *testing.T) {
	e := newTC(t)
	r, _ := e.svc.ApplyTrafficThrottle(context.Background(), &agentpb.TrafficThrottleRequest{OrchestratorSecret: "x"})
	if r.Success || r.Message != "Invalid orchestrator secret token." || len(e.fake.Calls()) != 0 {
		t.Fatalf("%+v", r)
	}
	// Body secret accepted.
	r, _ = e.svc.ApplyTrafficThrottle(context.Background(), &agentpb.TrafficThrottleRequest{OrchestratorSecret: secret})
	if !r.Success || r.Message != "Cleared" {
		t.Fatalf("%+v", r)
	}
}
