package singbox

import (
	"context"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// Egress firewall sync (src/utils/firewall.ts:140-196, spec B §1.3 step 9, spec D §8.1). It lives
// here because ApplyConfig is its only caller; group D owns ManageFirewall and the mesh port.

var udpTunnelTypes = map[string]bool{"hysteria2": true, "tuic": true}
var loopbackListen = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// ExtractUDPTunnelPorts is extractUdpTunnelPorts: listen ports of inbounds that face the
// outside over UDP. An inbound is skipped when its type is a string other than hysteria2/tuic
// (a missing or non-string type is NOT skipped, spec B R17) or when it listens on loopback;
// `Number(listen_port ?? port)` must be an integer in 1..65535. Deduplicated, in order.
func ExtractUDPTunnelPorts(cfg any) []float64 {
	inbounds, _ := inboundsOf(cfg)
	var out []float64
	seen := map[float64]bool{}
	for _, raw := range inbounds {
		in, ok := raw.(*jsjson.Object)
		if !ok {
			continue // arrays are typeof object too, but have no listen_port: NaN, skipped
		}
		if t, ok := in.Get("type"); ok {
			if ts, isStr := t.(string); isStr && !udpTunnelTypes[ts] {
				continue
			}
		}
		if l, ok := in.Get("listen"); ok {
			if ls, isStr := l.(string); isStr && loopbackListen[jsjson.Trim(ls)] {
				continue
			}
		}
		rawPort, ok := in.Get("listen_port")
		if !ok || rawPort == nil {
			rawPort, ok = in.Get("port")
			if !ok {
				continue // Number(undefined) is NaN
			}
		}
		port := jsjson.ToNumber(rawPort)
		if jsjson.IsInteger(port) && port > 0 && port <= 65535 && !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	return out
}

// FirewallPlan is planFirewallSync.
type FirewallPlan struct {
	ToOpen      []float64
	ToClose     []float64
	NeedsReload bool
}

// PlanFirewallSync: open what is new, close what is gone; reload only when either is non-empty.
func PlanFirewallSync(newPorts, previous []float64) FirewallPlan {
	inNew := map[float64]bool{}
	for _, p := range newPorts {
		inNew[p] = true
	}
	inPrev := map[float64]bool{}
	for _, p := range previous {
		inPrev[p] = true
	}
	var plan FirewallPlan
	for _, p := range newPorts {
		if !inPrev[p] {
			plan.ToOpen = append(plan.ToOpen, p)
		}
	}
	for _, p := range previous {
		if !inNew[p] {
			plan.ToClose = append(plan.ToClose, p)
		}
	}
	plan.NeedsReload = len(plan.ToOpen) > 0 || len(plan.ToClose) > 0
	return plan
}

// readActivePorts is readActivePortsCache: the numbers of the JSON array, [] on any error.
func (s *Service) readActivePorts() []float64 {
	raw, err := s.FS.ReadFile(ActivePortsCachePath)
	if err != nil {
		return nil
	}
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil
	}
	arr, _ := v.([]any)
	var out []float64
	for _, e := range arr {
		if n, ok := e.(float64); ok {
			out = append(out, n)
		}
	}
	return out
}

func (s *Service) ufw(ctx context.Context, args ...string) (runner.Result, error) {
	return s.Runner.Run(ctx, runner.Cmd{Name: "sudo", Args: append([]string{"ufw"}, args...), Timeout: ufwTimeout})
}

// syncEgressFirewall never fails the caller. Differences from TS: the allow/delete commands run
// one after another instead of in parallel (ufw is not safe for concurrent writers, spec B R17),
// and the cache is written atomically.
func (s *Service) syncEgressFirewall(ctx context.Context, cfg any) {
	if _, err := s.lookPath("ufw"); err != nil {
		s.Log.Warn("ufw is not installed on this system; skipping egress firewall synchronization")
		return
	}
	newPorts := ExtractUDPTunnelPorts(cfg)
	plan := PlanFirewallSync(newPorts, s.readActivePorts())
	if !plan.NeedsReload {
		s.Log.Debug("Egress firewall already in sync — skipping UFW reload", "ports", len(newPorts))
		return
	}
	for _, p := range plan.ToOpen {
		port := jsjson.FormatNumber(p)
		if res, err := s.ufw(ctx, "allow", port+"/udp"); err != nil {
			s.Log.Error("Failed to open UFW UDP port", "port", p, "err", runner.StderrOrMessage(err))
		} else {
			s.Log.Info("Opened UDP firewall port for egress tunnel inbound", "port", p, "stdout", res.Stdout, "stderr", res.Stderr)
		}
	}
	for _, p := range plan.ToClose {
		port := jsjson.FormatNumber(p)
		if res, err := s.ufw(ctx, "delete", "allow", port+"/udp"); err != nil {
			s.Log.Error("Failed to close UFW UDP port", "port", p, "err", runner.StderrOrMessage(err))
		} else {
			s.Log.Info("Closed stale UDP firewall port no longer used by egress config", "port", p, "stdout", res.Stdout, "stderr", res.Stderr)
		}
	}
	if res, err := s.ufw(ctx, "reload"); err != nil {
		s.Log.Error("Failed to reload UFW after egress firewall synchronization", "err", runner.StderrOrMessage(err))
	} else {
		if res.Stdout != "" {
			s.Log.Info("UFW reload stdout", "stdout", res.Stdout)
		}
		if res.Stderr != "" {
			s.Log.Warn("UFW reload stderr", "stderr", res.Stderr)
		}
	}
	ports := make([]any, len(newPorts))
	for i, p := range newPorts {
		ports[i] = p
	}
	if err := s.FS.WriteFile(ActivePortsCachePath, []byte(jsjson.Stringify(ports, "  ")), fsx.WriteOptions{}); err != nil {
		s.Log.Error("Unexpected error while synchronizing egress firewall state", "err", err.Error())
	}
}
