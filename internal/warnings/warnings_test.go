package warnings

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const quicRecvLine = "failed to sufficiently increase receive buffer size (was: 208 kiB, wanted: 7500 kiB, got: 416 kiB)"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func line(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "s"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Checklist 3, 5 (tests/journalWarnings.test.ts:31-64).
func TestPatternsAndClassify(t *testing.T) {
	want := "(?:failed to sufficiently increase receive buffer size)|(?:failed to sufficiently increase send buffer size)|" +
		"(?:failed to increase (?:receive|send) buffer size)|(?:nf_conntrack: table full)|(?:too many open files)|" +
		"(?:Out of socket memory)|(?:neighbour table overflow)"
	if got := GrepPattern(); got != want {
		t.Fatal(got)
	}
	for msg, kind := range map[string]string{
		quicRecvLine: "quic_recv_buffer",
		"failed to sufficiently increase send buffer size (x)":                   "quic_send_buffer",
		"failed to increase receive buffer size (wanted: 7500 kiB, got 208 kiB)": "quic_buffer_not_increased",
		"NF_CONNTRACK: TABLE FULL, dropping packet":                              "conntrack_table_full",
		"accept: Too many open files":                                            "too_many_open_files",
		"TCP: out of socket memory":                                              "out_of_socket_memory",
		"neighbour table overflow!":                                              "neighbour_table_overflow",
		"inbound/hysteria2[hy2-in]: new connection from 1.2.3.4":                 "",
	} {
		if got := Classify(msg); got != kind {
			t.Errorf("%q → %q want %q", msg, got, kind)
		}
	}
}

// tests/journalWarnings.test.ts:66-75: local time, absolute.
func TestSinceArgument(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 30, 15, 0, time.Local)
	if got := SinceArgument(24, now); got != "2026-09-03 12:30:15" {
		t.Fatal(got)
	}
	if got := SinceArgument(1, now); got != "2026-09-04 11:30:15" {
		t.Fatal(got)
	}
}

// Checklist 6, 7.
func TestParseJournalJSON(t *testing.T) {
	now := time.UnixMilli(1_790_000_000_123)
	long := strings.Repeat("Ж", 290) + " too many open files " + strings.Repeat("я", 50)
	out := strings.Join([]string{
		line(t, map[string]any{"MESSAGE": quicRecvLine, "_SYSTEMD_UNIT": "sing-box.service", "__REALTIME_TIMESTAMP": "1788000000000000"}),
		line(t, map[string]any{"MESSAGE": "unrelated", "_SYSTEMD_UNIT": "sing-box.service"}),
		"not json at all",
		"",
		"   ",
		"[1,2]",
		line(t, map[string]any{"MESSAGE": []int{110, 102}, "_SYSTEMD_UNIT": "caddy.service"}),
		line(t, map[string]any{"MESSAGE": "nf_conntrack: table full", "__REALTIME_TIMESTAMP": "garbage"}),
		line(t, map[string]any{"MESSAGE": long, "_SYSTEMD_UNIT": "caddy.service", "__REALTIME_TIMESTAMP": "1788000000999999"}),
	}, "\n")
	got := ParseJournalJSON(out, "kernel", now)
	want := []Entry{
		{Kind: "quic_recv_buffer", Source: "sing-box", Sample: quicRecvLine, AtUnixMs: 1788000000000},
		{Kind: "conntrack_table_full", Source: "kernel", Sample: "nf_conntrack: table full", AtUnixMs: now.UnixMilli()},
		{Kind: "too_many_open_files", Source: "caddy", Sample: string([]rune(long)[:300]), AtUnixMs: 1788000000999},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	// Only the first 2000 lines are read.
	var many []string
	for i := 0; i < 2100; i++ {
		many = append(many, line(t, map[string]any{"MESSAGE": "too many open files"}))
	}
	if n := len(ParseJournalJSON(strings.Join(many, "\n"), "x", now)); n != LineLimit {
		t.Fatal(n)
	}
	// A surrogate pair cut by the 300-unit limit.
	cut := ParseJournalJSON(line(t, map[string]any{"MESSAGE": "too many open files" + strings.Repeat("😀", 200)}), "x", now)
	if s := cut[0].Sample; !strings.HasSuffix(s, "�") || len([]rune(s)) != 19+140+1 {
		t.Fatalf("%q", s)
	}
}

// Checklist 8 (tests/journalWarnings.test.ts:116-146).
func TestAggregate(t *testing.T) {
	ws := Aggregate([]Entry{
		{Kind: "k", Source: "sing-box", Sample: "a", AtUnixMs: 1000},
		{Kind: "k", Source: "route-rear-singbox", Sample: "rear", AtUnixMs: 2000},
		{Kind: "k", Source: "sing-box", Sample: "свежее", AtUnixMs: 5000},
		{Kind: "k", Source: "sing-box", Sample: "old", AtUnixMs: 3000},
		{Kind: "k", Source: "route-rear-singbox", Sample: "tie-later", AtUnixMs: 2000},
	})
	want := []Warning{
		{Kind: "k", Source: "sing-box", Sample: "свежее", Occurrences: 3, LastSeenUnixMs: 5000},
		{Kind: "k", Source: "route-rear-singbox", Sample: "tie-later", Occurrences: 2, LastSeenUnixMs: 2000},
	}
	if !reflect.DeepEqual(ws, want) {
		t.Fatalf("%+v", ws)
	}
}

type recLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *recLog) Warn(msg string, _ ...any) {
	l.mu.Lock()
	l.msgs = append(l.msgs, msg)
	l.mu.Unlock()
}

// Checklist 2, 4: exact argv, sequential, bounded; exit 1 + empty stdout silent; output on a
// failure parsed; a killed pass does not sink the other.
func TestCollectJournalWarnings(t *testing.T) {
	c := cfg(t)
	now := time.Date(2026, 9, 4, 12, 30, 15, 0, time.Local)
	entry := line(t, map[string]any{"MESSAGE": quicRecvLine, "_SYSTEMD_UNIT": "sing-box.service", "__REALTIME_TIMESTAMP": "1788000000000000"})
	var calls []runner.Cmd
	var mu sync.Mutex
	run := &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) {
		mu.Lock()
		calls = append(calls, cmd)
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			return runner.FailWith(cmd, 1, entry+"\n", "")
		}
		return runner.Result{Stdout: entry + "\n"}, nil
	}}
	log := &recLog{}
	ws := CollectJournalWarnings(context.Background(), run, c, log, 24, now)
	common := []string{"--since", "2026-09-03 12:30:15", "--grep", GrepPattern(), "--case-sensitive=false", "--output=json", "--no-pager", "--lines=2000"}
	want0 := append([]string{"-u", "sing-box", "-u", "route-rear-singbox", "-u", "caddy"}, common...)
	want1 := append([]string{"-k"}, common...)
	if len(calls) != 2 || calls[0].Name != "journalctl" || !reflect.DeepEqual(calls[0].Args, want0) || !reflect.DeepEqual(calls[1].Args, want1) {
		t.Fatalf("%+v", calls)
	}
	for _, cmd := range calls {
		if cmd.Shell || cmd.Timeout != PassTimeout {
			t.Fatalf("%+v", cmd)
		}
	}
	if len(ws) != 1 || ws[0].Kind != "quic_recv_buffer" || ws[0].Occurrences != 2 {
		t.Fatalf("output attached to a non-zero exit must be parsed and counted: %+v", ws)
	}
	if len(log.msgs) != 0 {
		t.Fatal(log.msgs)
	}

	// Exit 1 with no output = nothing found, silently.
	run = &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) { return runner.FailWith(cmd, 1, "", "") }}
	log = &recLog{}
	if ws := CollectJournalWarnings(context.Background(), run, c, log, 24, now); len(ws) != 0 || len(log.msgs) != 0 {
		t.Fatal(ws, log.msgs)
	}
	// Killed pass → its own warning; the kernel pass still counts. Other failure → generic warning.
	kernel := line(t, map[string]any{"MESSAGE": "nf_conntrack: table full"})
	n := 0
	run = &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) {
		n++
		if n == 1 {
			return runner.Result{}, &runner.Error{Cmd: cmd.Display(), Killed: true, Code: -1, Signal: "SIGTERM"}
		}
		return runner.Result{Stdout: kernel}, nil
	}}
	log = &recLog{}
	ws = CollectJournalWarnings(context.Background(), run, c, log, 24, now)
	if len(ws) != 1 || ws[0].Source != "kernel" || !reflect.DeepEqual(log.msgs, []string{"Journal warning scan pass hit the time limit — reporting without it"}) {
		t.Fatal(ws, log.msgs)
	}
	run = &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) {
		return runner.Result{}, &runner.Error{Cmd: "journalctl", Spawn: io.EOF, SpawnCode: "ENOENT", Code: -1}
	}}
	log = &recLog{}
	_ = CollectJournalWarnings(context.Background(), run, c, log, 24, now)
	if len(log.msgs) != 2 || log.msgs[0] != "Journal warning scan pass failed" {
		t.Fatal(log.msgs)
	}
	// Both passes, worst case (timeout + the runner's SIGTERM→SIGKILL grace), fit 80 % of the
	// orchestrator's 60 s deadline (tests/journalWarnings.test.ts:232-253).
	if worst := 2 * (PassTimeout + runner.DefaultKillGrace); worst >= 48*time.Second {
		t.Fatal(worst)
	}
}

func authed(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// Checklist 1, 9, 12.
func TestPullNodeWarnings(t *testing.T) {
	c := cfg(t)
	c.NodeWarningLookbackHours = 12
	entry := line(t, map[string]any{"MESSAGE": "Too many open files", "_SYSTEMD_UNIT": "caddy.service", "__REALTIME_TIMESTAMP": "1000000"})
	run := &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) {
		if cmd.Args[0] == "-k" {
			return runner.FailWith(cmd, 1, "", "")
		}
		return runner.Result{Stdout: entry}, nil
	}}
	listen := &agentpb.NodeWarning{Kind: "core_not_listening", Source: "sing-box", Sample: "s", Occurrences: 1, LastSeenUnixMs: 1}
	empty := &agentpb.NodeWarning{Kind: "rear_rule_provider_empty", Source: "mihomo", Sample: "e", Occurrences: 2, LastSeenUnixMs: 2}
	svc := &Service{
		Auth: auth.New("s"), Runner: run, Cfg: c, Log: discard,
		CoreNotListening:   func(context.Context) *agentpb.NodeWarning { return listen },
		EmptyRuleProviders: func(context.Context) *agentpb.NodeWarning { return empty },
	}

	bad, err := svc.PullNodeWarnings(authed("x"), &agentpb.NodeWarningsRequest{})
	if err != nil || bad.Success || bad.Message != "Invalid orchestrator secret token." || len(bad.Warnings) != 0 || bad.LookbackHours != 0 {
		t.Fatalf("%v %v", err, bad)
	}
	if len(run.Calls()) != 0 {
		t.Fatal("scan ran unauthenticated")
	}

	resp, err := svc.PullNodeWarnings(context.Background(), &agentpb.NodeWarningsRequest{OrchestratorSecret: "s"})
	if err != nil || !resp.Success || resp.Message != "3 warning class(es) found." || resp.LookbackHours != 12 {
		t.Fatalf("%v %v", err, resp)
	}
	kinds := []string{}
	for _, w := range resp.Warnings {
		kinds = append(kinds, w.Kind+"@"+w.Source)
	}
	if !reflect.DeepEqual(kinds, []string{"core_not_listening@sing-box", "rear_rule_provider_empty@mihomo", "too_many_open_files@caddy"}) {
		t.Fatal(kinds)
	}
	if w := resp.Warnings[2]; w.Sample != "Too many open files" || w.Occurrences != 1 || w.LastSeenUnixMs != 1000 {
		t.Fatalf("%v", w)
	}

	svc.CoreNotListening = func(context.Context) *agentpb.NodeWarning { return nil }
	svc.EmptyRuleProviders = func(context.Context) *agentpb.NodeWarning { return nil }
	svc.Runner = &runner.Fake{Handler: func(cmd runner.Cmd) (runner.Result, error) { return runner.FailWith(cmd, 1, "", "") }}
	resp, _ = svc.PullNodeWarnings(authed("s"), &agentpb.NodeWarningsRequest{})
	if !resp.Success || resp.Message != "No degradation warnings in the journal." || resp.Warnings == nil || len(resp.Warnings) != 0 {
		t.Fatalf("%v", resp)
	}
}
