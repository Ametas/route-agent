// Package shaper implements ApplyTrafficThrottle (docs/go-rewrite/spec/d-awg-mesh-firewall-shaping.md
// §9): a tc ingress policer per source prefix on the default-route interface, with the drop
// counters read before any change (src/utils/trafficShaper.ts, src/services/trafficThrottle.service.ts).
package shaper

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// DefaultStatePath is the throttle state file (trafficShaper.ts:38).
const DefaultStatePath = "/etc/route-agent/traffic-throttle.json"

const (
	ingress   = "ffff:"
	tcTimeout = 10 * time.Second
	ipTimeout = 10 * time.Second
)

// Entry is one requested limit.
type Entry struct {
	Prefix, Rate, Burst string
}

// Counter is one prefix's policer counters.
type Counter struct {
	Prefix                                 string
	SentBytes, SentPackets, DroppedPackets int64
}

// State is the persisted layout: the interface and the entries in prio order. Incomplete marks a
// layout where some filter failed to install (Go only, see apply).
type State struct {
	Interface  string
	Entries    []Entry
	Incomplete bool
}

// Service implements ApplyTrafficThrottle.
type Service struct {
	Auth   *auth.Verifier
	Runner runner.Runner
	FS     fsx.FS
	Log    *slog.Logger
	// StatePath overrides DefaultStatePath (tests use FS.Root instead).
	StatePath string
	// mu serializes deliveries: tc rebuilds must not interleave (spec D R17). Throttle is its own
	// area — no other RPC touches the ingress qdisc — so it is not in internal/locks.
	mu sync.Mutex
}

func (s *Service) statePath() string {
	if s.StatePath == "" {
		return DefaultStatePath
	}
	return s.StatePath
}

func (s *Service) tc(ctx context.Context, args ...string) (runner.Result, error) {
	return s.Runner.Run(ctx, runner.Cmd{Name: "tc", Args: args, Timeout: tcTimeout})
}

// NormalizeEntries sorts by prefix the way `x.prefix.localeCompare(y.prefix)` does in Node 22
// (ICU root collation, en-US default locale: ':' sorts before '.', '/' and digits; lower case
// before upper case). x/text/collate implements the same CLDR root order; it was checked against
// Node on 20 000 random strings of the prefix alphabet. Stable, duplicates kept (Array.sort is
// stable).
func NormalizeEntries(entries []Entry) []Entry {
	out := append([]Entry(nil), entries...)
	c := collate.New(language.Und)
	sort.SliceStable(out, func(i, j int) bool { return c.CompareString(out[i].Prefix, out[j].Prefix) < 0 })
	return out
}

// IsSame is isSameThrottle: same interface and the same prefix/rate/burst at every index. A
// layout recorded as incomplete is never "the same": the next delivery retries the failed filters.
func IsSame(a *State, b State) bool {
	if a == nil || a.Incomplete || a.Interface != b.Interface || len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		if a.Entries[i] != b.Entries[i] {
			return false
		}
	}
	return true
}

var (
	rePref = regexp.MustCompile(`\bpref\s+(\d+)\s+u32\b`)
	reSent = regexp.MustCompile(`\bSent\s+(\d+)\s+bytes\s+(\d+)\s+pkts?\s+\(dropped\s+(\d+)`)
	reDev  = regexp.MustCompile(`\bdev\s+(\S+)`)
)

// ParseFilterCounters is parseFilterCounters: each filter prints several lines with the same
// `pref N u32`; the next `Sent … (dropped …)` line belongs to prefixes[N-1]. An unknown pref is
// skipped (never attributed to another prefix); a Sent line with no pref before it is ignored.
func ParseFilterCounters(output string, prefixes []string) []Counter {
	counters := []Counter{}
	cur := -1
	for _, line := range strings.Split(output, "\n") {
		if m := rePref.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				n = 0
			}
			cur = n
			continue
		}
		m := reSent.FindStringSubmatch(line)
		if m == nil || cur < 0 {
			continue
		}
		if cur >= 1 && cur <= len(prefixes) && prefixes[cur-1] != "" {
			counters = append(counters, Counter{Prefix: prefixes[cur-1], SentBytes: num(m[1]), SentPackets: num(m[2]), DroppedPackets: num(m[3])})
		}
		cur = -1
	}
	return counters
}

func num(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 1<<63 - 1
	}
	return n
}

// readState is readState + JSON.parse: nil when missing or unparsable. Values of the wrong type
// read as "" (never equal to a desired layout, so it is rebuilt). ifaceSet tells whether the
// "interface" key holds a string (clearThrottle's `state?.interface ?? resolve()`).
func (s *Service) readState() (st *State, ifaceSet bool) {
	raw, err := s.FS.ReadFile(s.statePath())
	if err != nil {
		return nil, false
	}
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil, false
	}
	obj, ok := v.(*jsjson.Object)
	if !ok {
		return nil, false
	}
	st = &State{}
	if iv, ok := obj.Get("interface"); ok {
		st.Interface, ifaceSet = iv.(string)
	}
	if inc, ok := obj.Get("incomplete"); ok {
		st.Incomplete, _ = inc.(bool)
	}
	ev, ok := obj.Get("entries")
	arr, isArr := ev.([]any)
	if !ok || !isArr {
		st.Entries = nil
		st.Incomplete = true // !Array.isArray(a.entries) → never the same
		return st, ifaceSet
	}
	for _, e := range arr {
		eo, _ := e.(*jsjson.Object)
		str := func(k string) string {
			v, _ := eo.Get(k)
			s, _ := v.(string)
			return s
		}
		st.Entries = append(st.Entries, Entry{Prefix: str("prefix"), Rate: str("rate"), Burst: str("burst")})
	}
	return st, ifaceSet
}

// writeState writes JSON.stringify(state, null, 2) + "\n" (atomic; same bytes as TS for a
// complete layout).
func (s *Service) writeState(st State) error {
	obj := jsjson.NewObject()
	obj.Set("interface", st.Interface)
	entries := make([]any, len(st.Entries))
	for i, e := range st.Entries {
		eo := jsjson.NewObject()
		eo.Set("prefix", e.Prefix)
		eo.Set("rate", e.Rate)
		eo.Set("burst", e.Burst)
		entries[i] = eo
	}
	obj.Set("entries", entries)
	if st.Incomplete {
		obj.Set("incomplete", true)
	}
	if err := s.FS.WriteFile(s.statePath(), []byte(jsjson.Stringify(obj, "  ")+"\n"), fsx.WriteOptions{}); err != nil {
		return errors.New(fsx.NodeError("open", s.statePath(), err))
	}
	return nil
}

// resolveIface is resolveClientFacingInterface: `ip -o route show default`, first `dev X`.
func (s *Service) resolveIface(ctx context.Context) string {
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: "ip", Args: []string{"-o", "route", "show", "default"}, Timeout: ipTimeout})
	if err != nil {
		s.Log.Error("Failed to resolve the default-route interface", "err", err.Error())
		return ""
	}
	if m := reDev.FindStringSubmatch(res.Stdout); m != nil {
		return m[1]
	}
	return ""
}

// ReadCounters is readThrottleCounters: [] without a stored layout; tc failure → warn, [].
func (s *Service) ReadCounters(ctx context.Context) []Counter {
	st, _ := s.readState()
	if st == nil || len(st.Entries) == 0 {
		return []Counter{}
	}
	res, err := s.tc(ctx, "-s", "filter", "show", "dev", st.Interface, "parent", ingress)
	if err != nil {
		s.Log.Warn("Failed to read traffic-throttle counters", "err", err.Error())
		return []Counter{}
	}
	prefixes := make([]string, len(st.Entries))
	for i, e := range st.Entries {
		prefixes[i] = e.Prefix
	}
	return ParseFilterCounters(res.Stdout, prefixes)
}

// Result is applyThrottle's outcome.
type Result struct {
	Changed bool
	Applied int
	Message string
}

// Apply is applyThrottle (trafficShaper.ts:178-218). Differences (deviations.md): a layout where
// some filter failed is stored as incomplete, so the next identical delivery rebuilds instead of
// answering "Unchanged" forever (spec D R10); a layout that moved to another interface removes the
// ingress qdisc it left on the old one.
func (s *Service) Apply(ctx context.Context, entries []Entry) (Result, error) {
	iface := s.resolveIface(ctx)
	if iface == "" {
		// Reported as success:true by the handler — kept (spec D R10): the orchestrator only logs it.
		return Result{Message: "No default-route interface found"}, nil
	}
	desired := State{Interface: iface, Entries: NormalizeEntries(entries)}
	prev, _ := s.readState()
	if IsSame(prev, desired) {
		return Result{Applied: len(desired.Entries), Message: "Unchanged"}, nil
	}
	if prev != nil && prev.Interface != "" && prev.Interface != iface {
		_, _ = s.tc(ctx, "qdisc", "del", "dev", prev.Interface, "ingress")
	}
	if _, err := s.tc(ctx, "qdisc", "replace", "dev", iface, "handle", ingress, "ingress"); err != nil {
		return Result{}, err
	}
	_, _ = s.tc(ctx, "filter", "del", "dev", iface, "parent", ingress) // no filters yet → error, ignored

	applied := 0
	for i, e := range desired.Entries {
		proto, match := "ip", "ip"
		if strings.Contains(e.Prefix, ":") {
			proto, match = "ipv6", "ip6"
		}
		_, err := s.tc(ctx, "filter", "add", "dev", iface, "parent", ingress,
			"protocol", proto, "prio", strconv.Itoa(i+1),
			"u32", "match", match, "src", e.Prefix,
			"police", "rate", e.Rate, "burst", e.Burst, "conform-exceed", "drop/ok")
		if err != nil {
			s.Log.Warn("Failed to install a throttle filter", "err", err.Error(), "prefix", e.Prefix)
			continue
		}
		applied++
	}
	desired.Incomplete = applied < len(desired.Entries)
	if err := s.writeState(desired); err != nil {
		return Result{}, err
	}
	s.Log.Info("Traffic throttle applied", "iface", iface, "applied", applied)
	return Result{Changed: true, Applied: applied, Message: "Applied"}, nil
}

// Clear is clearThrottle: the stored interface (else the default-route one), `tc qdisc del dev X
// ingress` (errors ignored), state file removed.
func (s *Service) Clear(ctx context.Context) error {
	st, ifaceSet := s.readState()
	iface := ""
	if st != nil && ifaceSet {
		iface = st.Interface
	} else {
		iface = s.resolveIface(ctx)
	}
	if iface == "" {
		return nil
	}
	_, _ = s.tc(ctx, "qdisc", "del", "dev", iface, "ingress")
	if err := os.Remove(s.FS.Path(s.statePath())); err != nil && !os.IsNotExist(err) {
		return errors.New(fsx.NodeError("rm", s.statePath(), err))
	}
	return nil
}

func toProto(cs []Counter) []*agentpb.TrafficThrottleCounter {
	out := make([]*agentpb.TrafficThrottleCounter, 0, len(cs))
	for _, c := range cs {
		out = append(out, &agentpb.TrafficThrottleCounter{Prefix: c.Prefix, SentBytes: c.SentBytes, SentPackets: c.SentPackets, DroppedPackets: c.DroppedPackets})
	}
	return out
}

// ApplyTrafficThrottle implements the RPC (spec D §9.2). Auth: metadata or body secret.
func (s *Service) ApplyTrafficThrottle(ctx context.Context, req *agentpb.TrafficThrottleRequest) (*agentpb.TrafficThrottleResponse, error) {
	fail := func(msg string, counters []Counter) *agentpb.TrafficThrottleResponse {
		return &agentpb.TrafficThrottleResponse{Success: false, Message: msg, Counters: toProto(counters)}
	}
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized ApplyTrafficThrottle request blocked")
		return fail(auth.InvalidSecretMessage, nil), nil
	}
	var entries []Entry
	for _, e := range req.GetEntries() {
		en := Entry{Prefix: jsjson.Trim(e.GetPrefix()), Rate: jsjson.Trim(e.GetRate()), Burst: jsjson.Trim(e.GetBurst())}
		if en.Prefix != "" {
			entries = append(entries, en)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	work := context.WithoutCancel(ctx)

	counters := s.ReadCounters(work) // first: a rebuild resets them
	if len(entries) == 0 {
		if err := s.Clear(work); err != nil {
			s.Log.Warn("Failed to apply the traffic throttle", "err", err.Error())
			return fail(err.Error(), nil), nil
		}
		return &agentpb.TrafficThrottleResponse{Success: true, Message: "Cleared", Changed: true, Counters: toProto(counters)}, nil
	}
	var incomplete []string
	for _, e := range entries {
		if e.Rate == "" || e.Burst == "" {
			incomplete = append(incomplete, e.Prefix)
		}
	}
	if len(incomplete) > 0 {
		return fail("Entries without rate/burst: "+strings.Join(incomplete, ", "), counters), nil
	}
	res, err := s.Apply(work, entries)
	if err != nil {
		s.Log.Warn("Failed to apply the traffic throttle", "err", err.Error())
		return fail(err.Error(), nil), nil
	}
	return &agentpb.TrafficThrottleResponse{Success: true, Message: res.Message, Changed: res.Changed, Applied: int32(res.Applied), Counters: toProto(counters)}, nil
}
