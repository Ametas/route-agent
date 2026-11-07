package diag

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
	"google.golang.org/protobuf/proto"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func report(t *testing.T, hubs ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"report": map[string]any{"mtr": map[string]any{}, "hubs": hubs}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tests/app.test.ts:1580-1594 (checklist 3).
func TestValidTarget(t *testing.T) {
	for in, want := range map[string]bool{
		"8.8.8.8": true, "example.com": true, "2001:db8::1": true, "sub.domain-name.example.com": true, " 1.1.1.1 ": true,
		"-oSomeFlag": false, "--report-cycles": false, "": false, "   ": false, "has a space": false, "rm;-rf": false,
		"$(whoami)": false, strings.Repeat("a", 254): false, strings.Repeat("a", 253): true, "a.": false,
	} {
		if got := ValidTarget(in); got != want {
			t.Errorf("%q → %v", in, got)
		}
	}
}

// tests/networkDiagnosticProbes.test.ts (checklist 2–4).
func TestProbes(t *testing.T) {
	base := []string{"--report", "--json", "--no-dns", "--report-cycles", "10"}
	if got := MtrArgs(Probe{"1.2.3.4", "icmp", 0}); !reflect.DeepEqual(got, append(base, "1.2.3.4")) {
		t.Fatal(got)
	}
	if got := MtrArgs(Probe{"1.2.3.4", "tcp", 443}); !reflect.DeepEqual(got, append(append([]string{}, base...), "--tcp", "--port", "443", "1.2.3.4")) {
		t.Fatal(got)
	}
	if got := MtrArgs(Probe{"1.2.3.4", "udp", 51821}); !reflect.DeepEqual(got, append(append([]string{}, base...), "--udp", "--port", "51821", "1.2.3.4")) {
		t.Fatal(got)
	}
	p, bad := NormalizeProbe(&agentpb.NetworkDiagnosticProbe{Target: " 1.2.3.4 ", Protocol: " TCP ", Port: 22})
	if bad != "" || p != (Probe{"1.2.3.4", "tcp", 22}) {
		t.Fatal(p, bad)
	}
	p, bad = NormalizeProbe(&agentpb.NetworkDiagnosticProbe{Target: "1.2.3.4", Protocol: "icmp", Port: 80})
	if bad != "" || p != (Probe{"1.2.3.4", "icmp", 0}) {
		t.Fatal(p, bad)
	}
	for _, c := range []struct {
		in   *agentpb.NetworkDiagnosticProbe
		echo Probe
		err  string
	}{
		{&agentpb.NetworkDiagnosticProbe{Target: "1.2.3.4", Protocol: "SCTP", Port: 1}, Probe{"1.2.3.4", "sctp", 1}, "Unsupported probe protocol: sctp."},
		{&agentpb.NetworkDiagnosticProbe{Target: "1.2.3.4"}, Probe{"1.2.3.4", "", 0}, "Unsupported probe protocol: (empty)."},
		{&agentpb.NetworkDiagnosticProbe{Target: "1.2.3.4", Protocol: "udp"}, Probe{"1.2.3.4", "udp", 0}, "Probe port must be 1-65535 for udp."},
		{&agentpb.NetworkDiagnosticProbe{Target: "1.2.3.4", Protocol: "tcp", Port: 70000}, Probe{"1.2.3.4", "tcp", 70000}, "Probe port must be 1-65535 for tcp."},
		{&agentpb.NetworkDiagnosticProbe{Target: " -oPonFire", Protocol: "bogus", Port: 22}, Probe{" -oPonFire", "bogus", 22}, "Invalid or unsafe target string."},
	} {
		p, bad := NormalizeProbe(c.in)
		if p != c.echo || bad != c.err {
			t.Errorf("%v → %+v %q", c.in, p, bad)
		}
	}
}

// Checklist 5.
func TestParseMtrJSON(t *testing.T) {
	r, err := ParseMtrJSON(report(t,
		map[string]any{"count": 1, "host": "10.0.0.1", "Loss%": 0, "Avg": 0.4},
		map[string]any{"count": 2, "host": "188.1.1.1", "Loss%": 10, "Avg": 3.1},
		map[string]any{"count": 3, "host": "???", "Loss%": 100, "Avg": 0},
		map[string]any{"count": "4", "host": "???", "Loss%": 100.0, "Avg": 0},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r.Reachable || r.LossPercent != 100 || r.AvgLatencyMs != 0 || r.LastHopNumber != 2 || r.LastHopAddress != "188.1.1.1" || len(r.LossyHops) != 3 {
		t.Fatalf("%+v", r)
	}
	if h := r.LossyHops[2]; h.HopNumber != 4 || h.Address != "???" || h.LossPercent != 100 {
		t.Fatalf("%v", h)
	}
	r, _ = ParseMtrJSON(report(t,
		map[string]any{"count": 1, "host": "192.168.1.1", "Loss%": 0.0, "Avg": 0.5},
		map[string]any{"count": 2, "host": "flaky-router.example", "Loss%": 20.0, "Avg": 5.2},
		map[string]any{"count": 3, "host": "1.1.1.1", "Loss%": 0.0, "Avg": 8.1},
	))
	if !r.Reachable || r.LossPercent != 0 || r.AvgLatencyMs != 8.1 || r.LastHopNumber != 3 || r.LastHopAddress != "1.1.1.1" ||
		len(r.LossyHops) != 1 || !proto.Equal(r.LossyHops[0], &agentpb.NetworkDiagnosticHop{HopNumber: 2, Address: "flaky-router.example", LossPercent: 20, AvgLatencyMs: 5.2}) {
		t.Fatalf("%+v", r)
	}
	// A hub without host: lossy address "???", not a reachable last hop.
	r, _ = ParseMtrJSON(`{"report":{"hubs":[{"count":1,"Loss%":50}]}}`)
	if r.Reachable || r.LossyHops[0].Address != "???" || r.LastHopNumber != 0 {
		t.Fatalf("%+v", r)
	}
	for raw, msg := range map[string]string{
		"not json":               "Failed to parse mtr JSON output: Unexpected token 'o', \"not json\" is not valid JSON",
		`{"report":{}}`:          "mtr JSON report did not contain any hubs (hops).",
		`{"report":{"hubs":[]}}`: "mtr JSON report did not contain any hubs (hops).",
		`null`:                   "mtr JSON report did not contain any hubs (hops).",
	} {
		if _, err := ParseMtrJSON(raw); err == nil || err.Error() != msg {
			t.Errorf("%s → %v", raw, err)
		}
	}
}

func authed(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// Checklist 1, 2, 4, 6, 7: concurrency, order, failure isolation, message.
func TestRunNetworkDiagnostic(t *testing.T) {
	ok := report(t, map[string]any{"count": 1, "host": "9.9.9.9", "Loss%": 0, "Avg": 1.5})
	var mu sync.Mutex
	running, peak := 0, 0
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		target := c.Args[len(c.Args)-1]
		switch target {
		case "8.8.8.8":
			return runner.FailWith(c, 1, "", "mtr: boom\n")
		case "2.2.2.2":
			return runner.Result{Stdout: "garbage"}, nil
		}
		return runner.Result{Stdout: ok}, nil
	}}
	svc := &Service{Auth: auth.New("s"), Runner: fake, Log: discard}

	bad, _ := svc.RunNetworkDiagnostic(authed("x"), &agentpb.NetworkDiagnosticRequest{Targets: []string{"1.1.1.1"}})
	if bad.Success || bad.Message != "Invalid orchestrator secret token." || len(bad.Results) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("%v", bad)
	}

	resp, err := svc.RunNetworkDiagnostic(context.Background(), &agentpb.NetworkDiagnosticRequest{
		OrchestratorSecret: "s",
		Targets:            []string{" 1.1.1.1 ", "8.8.8.8", "bad target"},
		Probes: []*agentpb.NetworkDiagnosticProbe{
			{Target: "3.3.3.3", Protocol: "TCP", Port: 443},
			{Target: "3.3.3.3", Protocol: "sctp", Port: 9},
			{Target: "2.2.2.2", Protocol: "udp", Port: 51821},
			{Target: "4.4.4.4", Protocol: "icmp", Port: 7},
		},
	})
	if err != nil || !resp.Success {
		t.Fatal(err, resp)
	}
	if resp.Message != "Network diagnostic completed for 7 target(s): 3 reachable, 4 unreachable/error." {
		t.Fatal(resp.Message)
	}
	type row struct {
		target, proto string
		port          uint32
		reach         bool
		err           string
	}
	var rows []row
	for _, r := range resp.Results {
		rows = append(rows, row{r.Target, r.Protocol, r.Port, r.Reachable, r.Error})
		if r.LossyHops == nil {
			t.Fatal("lossy_hops nil")
		}
	}
	want := []row{
		{"1.1.1.1", "icmp", 0, true, ""},
		{"8.8.8.8", "icmp", 0, false, "Command failed: mtr --report --json --no-dns --report-cycles 10 8.8.8.8\nmtr: boom\n"},
		{"bad target", "icmp", 0, false, "Invalid or unsafe target string."},
		{"3.3.3.3", "tcp", 443, true, ""},
		{"3.3.3.3", "sctp", 9, false, "Unsupported probe protocol: sctp."},
		{"2.2.2.2", "udp", 51821, false, "Failed to parse mtr JSON output: Unexpected token 'g', \"garbage\" is not valid JSON"},
		{"4.4.4.4", "icmp", 0, true, ""},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("\n%+v\n%+v", rows, want)
	}
	if r := resp.Results[0]; r.AvgLatencyMs != 1.5 || r.LastHopNumber != 1 || r.LastHopAddress != "9.9.9.9" {
		t.Fatalf("%v", r)
	}
	if peak != 5 {
		t.Fatalf("mtr runs must be concurrent: peak %d", peak)
	}
	for _, c := range fake.Calls() {
		if c.Name != "mtr" || c.Shell || c.Timeout != MtrTimeout {
			t.Fatalf("%+v", c)
		}
	}

	empty, _ := svc.RunNetworkDiagnostic(authed("s"), &agentpb.NetworkDiagnosticRequest{})
	if !empty.Success || empty.Message != "Network diagnostic completed for 0 target(s): 0 reachable, 0 unreachable/error." {
		t.Fatal(empty)
	}
}

// GetWarpKeyHealth: auth answer here, everything else in rear.WarpKeyHealthReport (group F tests).
func TestGetWarpKeyHealth(t *testing.T) {
	calls := 0
	svc := &Service{Auth: auth.New("s"), Log: discard, WarpKeyHealthReport: func(context.Context) *agentpb.WarpKeyHealthResponse {
		calls++
		return &agentpb.WarpKeyHealthResponse{Success: true, Message: "Reported health for 0 WARP endpoint(s)."}
	}}
	bad, _ := svc.GetWarpKeyHealth(authed("x"), &agentpb.WarpKeyHealthRequest{})
	if bad.Success || bad.Message != "Invalid orchestrator secret token." || bad.SkippedReason != "" || bad.Entries == nil || calls != 0 {
		t.Fatalf("%v", bad)
	}
	ok, _ := svc.GetWarpKeyHealth(context.Background(), &agentpb.WarpKeyHealthRequest{OrchestratorSecret: "s"})
	if !ok.Success || calls != 1 {
		t.Fatalf("%v", ok)
	}
}
