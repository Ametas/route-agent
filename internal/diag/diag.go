// Package diag implements RunNetworkDiagnostic (spec E §4: mtr probes from the node) and
// GetWarpKeyHealth (spec E §5: the rear's own WARP endpoint measurements, read through its Clash
// API by group F's rear.Service).
package diag

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const (
	// MtrReportCycles is --report-cycles.
	MtrReportCycles = 10
	// MtrTimeout bounds one mtr run; all probes run concurrently, so the RPC takes about this
	// long at worst — coupled to the orchestrator's 60 s deadline (GrpcNetworkDiagnosticClient.ts).
	MtrTimeout = 45 * time.Second

	unresolvedHost = "???"
	invalidTarget  = "Invalid or unsafe target string."
)

var safeTarget = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.:-]{0,252}[a-zA-Z0-9])?$`)

// utf16Len is a JS string's length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// ValidTarget is isValidDiagnosticTarget (networkDiagnostic.service.ts:26-34): trimmed length
// 1..253, no leading '-', and the safe host/IP pattern (it is an mtr argument).
func ValidTarget(target string) bool {
	t := jsjson.Trim(target)
	if l := utf16Len(t); l == 0 || l > 253 {
		return false
	}
	if strings.HasPrefix(t, "-") {
		return false
	}
	return safeTarget.MatchString(t)
}

// Probe is a normalized, valid probe.
type Probe struct {
	Target   string
	Protocol string
	Port     uint32
}

// NormalizeProbe is normalizeProbe (:129-148). On failure it returns the echo values (label,
// normalized protocol, port as sent) and the error text; checks in order: target, protocol, port.
func NormalizeProbe(p *agentpb.NetworkDiagnosticProbe) (Probe, string) {
	label := p.GetTarget()
	protocol := strings.ToLower(jsjson.Trim(p.GetProtocol()))
	port := p.GetPort()
	bad := Probe{Target: label, Protocol: protocol, Port: port}
	if !ValidTarget(label) {
		return bad, invalidTarget
	}
	if protocol != "icmp" && protocol != "tcp" && protocol != "udp" {
		shown := protocol
		if shown == "" {
			shown = "(empty)"
		}
		return bad, "Unsupported probe protocol: " + shown + "."
	}
	if protocol != "icmp" && (port < 1 || port > 65535) {
		return bad, "Probe port must be 1-65535 for " + protocol + "."
	}
	if protocol == "icmp" {
		port = 0
	}
	return Probe{Target: jsjson.Trim(label), Protocol: protocol, Port: port}, ""
}

// MtrArgs is buildMtrArgs.
func MtrArgs(p Probe) []string {
	args := []string{"--report", "--json", "--no-dns", "--report-cycles", strconv.Itoa(MtrReportCycles)}
	if p.Protocol != "icmp" {
		args = append(args, "--"+p.Protocol, "--port", strconv.FormatUint(uint64(p.Port), 10))
	}
	return append(args, p.Target)
}

// Report is the parsed mtr report.
type Report struct {
	Reachable      bool
	LossPercent    float64
	AvgLatencyMs   float64
	LossyHops      []*agentpb.NetworkDiagnosticHop
	LastHopNumber  uint32
	LastHopAddress string
}

// jsNum is `Number(v) || 0` (undefined → 0 as well).
func jsNum(v any, present bool) float64 {
	if !present {
		return 0
	}
	n := jsjson.ToNumber(v)
	if math.IsNaN(n) {
		return 0
	}
	return n
}

func field(hub any, key string) (any, bool) {
	o, ok := hub.(*jsjson.Object)
	if !ok {
		return nil, false
	}
	return o.Get(key)
}

// hostOf is String(hub?.host ?? def).
func hostOf(hub any, def string) string {
	v, ok := field(hub, "host")
	if !ok || v == nil {
		return def
	}
	return jsjson.String(v)
}

func num(hub any, key string) float64 {
	v, ok := field(hub, key)
	return jsNum(v, ok)
}

// ParseMtrJSON is parseMtrJsonReport (:72-122).
func ParseMtrJSON(raw string) (Report, error) {
	parsed, err := jsjson.Parse(raw)
	if err != nil {
		return Report{}, &diagError{"Failed to parse mtr JSON output: " + err.Error()}
	}
	rep, _ := field(parsed, "report")
	hubsV, _ := field(rep, "hubs")
	hubs, ok := hubsV.([]any)
	if !ok || len(hubs) == 0 {
		return Report{}, &diagError{"mtr JSON report did not contain any hubs (hops)."}
	}
	r := Report{LossyHops: []*agentpb.NetworkDiagnosticHop{}}
	for _, hub := range hubs {
		loss := num(hub, "Loss%")
		if loss > 0 {
			r.LossyHops = append(r.LossyHops, &agentpb.NetworkDiagnosticHop{
				HopNumber:    jsjson.ToUint32(num(hub, "count")),
				Address:      hostOf(hub, unresolvedHost),
				LossPercent:  loss,
				AvgLatencyMs: num(hub, "Avg"),
			})
		}
	}
	last := hubs[len(hubs)-1]
	lastHost := jsjson.Trim(hostOf(last, ""))
	r.Reachable = lastHost != "" && lastHost != unresolvedHost
	r.LossPercent = num(last, "Loss%")
	r.AvgLatencyMs = num(last, "Avg")
	for _, hub := range hubs {
		host := jsjson.Trim(hostOf(hub, ""))
		if host == "" || host == unresolvedHost || num(hub, "Loss%") >= 100 {
			continue
		}
		r.LastHopNumber = jsjson.ToUint32(num(hub, "count"))
		r.LastHopAddress = host
	}
	return r, nil
}

type diagError struct{ msg string }

func (e *diagError) Error() string { return e.msg }

// Service holds the RunNetworkDiagnostic and GetWarpKeyHealth handlers.
type Service struct {
	Auth   *auth.Verifier
	Runner runner.Runner
	Log    *slog.Logger
	// WarpKeyHealthReport is (*rear.Service).WarpKeyHealthReport: the whole GetWarpKeyHealth
	// body after authentication (core-aware, architecture §6 item 7).
	WarpKeyHealthReport func(ctx context.Context) *agentpb.WarpKeyHealthResponse
}

func result(target string, r Report, errText, protocol string, port uint32) *agentpb.NetworkDiagnosticTargetResult {
	hops := r.LossyHops
	if hops == nil {
		hops = []*agentpb.NetworkDiagnosticHop{}
	}
	return &agentpb.NetworkDiagnosticTargetResult{
		Target: target, Reachable: r.Reachable, LossPercent: r.LossPercent, AvgLatencyMs: r.AvgLatencyMs,
		LossyHops: hops, Error: errText, Protocol: protocol, Port: port,
		LastHopNumber: r.LastHopNumber, LastHopAddress: r.LastHopAddress,
	}
}

func (s *Service) diagnose(ctx context.Context, p Probe) *agentpb.NetworkDiagnosticTargetResult {
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: "mtr", Args: MtrArgs(p), Timeout: MtrTimeout})
	var rep Report
	if err == nil {
		rep, err = ParseMtrJSON(res.Stdout)
	}
	if err != nil {
		s.Log.Warn("mtr network diagnostic failed for target", "target", p.Target, "protocol", p.Protocol, "port", p.Port, "err", err.Error())
		return result(p.Target, Report{}, err.Error(), p.Protocol, p.Port)
	}
	return result(p.Target, rep, "", p.Protocol, p.Port)
}

// RunNetworkDiagnostic is runNetworkDiagnosticHandler (:248-284). Auth M|B (failure: status OK,
// success:false). Every legacy target (ICMP) and every probe runs concurrently, results in
// request order, targets first; a failed probe is a result with its error, never an RPC failure.
//
// The probes outlive a cancelled client no longer: mtr is killed with the RPC's context (TS left
// the processes running).
func (s *Service) RunNetworkDiagnostic(ctx context.Context, req *agentpb.NetworkDiagnosticRequest) (*agentpb.NetworkDiagnosticResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized RunNetworkDiagnostic request blocked")
		return &agentpb.NetworkDiagnosticResponse{Success: false, Message: auth.InvalidSecretMessage, Results: []*agentpb.NetworkDiagnosticTargetResult{}}, nil
	}
	targets, probes := req.GetTargets(), req.GetProbes()
	results := make([]*agentpb.NetworkDiagnosticTargetResult, len(targets)+len(probes))
	var wg sync.WaitGroup
	for i, t := range targets {
		if !ValidTarget(t) {
			s.Log.Warn("Rejected invalid/unsafe RunNetworkDiagnostic target", "target", t)
			results[i] = result(t, Report{}, invalidTarget, "icmp", 0)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.diagnose(ctx, Probe{Target: jsjson.Trim(t), Protocol: "icmp"})
		}()
	}
	for j, raw := range probes {
		i := len(targets) + j
		p, bad := NormalizeProbe(raw)
		if bad != "" {
			s.Log.Warn("Rejected invalid RunNetworkDiagnostic probe", "target", p.Target, "protocol", p.Protocol, "port", p.Port)
			results[i] = result(p.Target, Report{}, bad, p.Protocol, p.Port)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.diagnose(ctx, p)
		}()
	}
	wg.Wait()
	reachable := 0
	for _, r := range results {
		if r.Reachable {
			reachable++
		}
	}
	total := len(results)
	s.Log.Info("RunNetworkDiagnostic completed", "total", total, "reachable", reachable)
	return &agentpb.NetworkDiagnosticResponse{
		Success: true,
		Message: "Network diagnostic completed for " + strconv.Itoa(total) + " target(s): " + strconv.Itoa(reachable) +
			" reachable, " + strconv.Itoa(total-reachable) + " unreachable/error.",
		Results: results,
	}, nil
}

// GetWarpKeyHealth is getWarpKeyHealthHandler (warpKeyHealth.service.ts:26-84): auth M|B
// (failure: status OK, success:false, no skipped reason), then rear.Service.WarpKeyHealthReport.
func (s *Service) GetWarpKeyHealth(ctx context.Context, req *agentpb.WarpKeyHealthRequest) (*agentpb.WarpKeyHealthResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetWarpKeyHealth request blocked")
		return &agentpb.WarpKeyHealthResponse{Success: false, Message: auth.InvalidSecretMessage, Entries: []*agentpb.WarpKeyHealthEntry{}}, nil
	}
	return s.WarpKeyHealthReport(ctx), nil
}
