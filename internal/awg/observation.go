package awg

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// ConntrackPath is read first; `conntrack -L` is the fallback (awgFlows.ts:240-261).
const ConntrackPath = "/proc/net/nf_conntrack"

const (
	conntrackTimeout = 30 * time.Second
	// conntrackMaxOutput is the 32 MiB maxBuffer TS gave `conntrack -L` (awgFlows.ts:194): on
	// kernels without CONFIG_NF_CONNTRACK_PROCFS that command is the only source, and the runner's
	// default 8 MiB cap would cut a busy node's table short.
	conntrackMaxOutput = 32 << 20
)

// bigOutput returns the runner for `conntrack -L`: the real runner with the 32 MiB cap (runner.Cmd
// has no per-call cap, see the report), a substituted one (tests) as is.
func bigOutput(r runner.Runner) runner.Runner {
	if x, ok := r.(runner.Exec); ok {
		x.MaxOutput = conntrackMaxOutput
		return x
	}
	return r
}

// Peer is one row of `awg show <iface> dump` (awgFlows.ts:78-107).
type Peer struct {
	PublicKey          string
	Endpoint           string
	TunnelIPs          []string
	RxBytes, TxBytes   int64
	LatestHandshakeUTC int64
}

// Flow is one conntrack entry owned by a peer (awgFlows.ts:139-166).
type Flow struct {
	PublicKey       string
	TunnelIP        string
	Protocol        string
	DestinationIP   string
	DestinationPort uint32
}

// ParseDump is parseAwgDump: the interface line is skipped, rows need ≥8 columns, "(none)"
// endpoint → "", "(none)" allowed-ips → [], masks stripped, non-numeric counters → 0.
func ParseDump(stdout string) []Peer {
	var peers []Peer
	lines := strings.Split(jsjson.Trim(stdout), "\n")
	for _, raw := range lines[1:] {
		parts := Fields(raw)
		if len(parts) < 8 {
			continue
		}
		p := Peer{PublicKey: parts[0], TunnelIPs: []string{}}
		if parts[2] != "(none)" {
			p.Endpoint = parts[2]
		}
		if parts[3] != "(none)" {
			for _, ip := range strings.Split(parts[3], ",") {
				addr, _, _ := strings.Cut(ip, "/")
				p.TunnelIPs = append(p.TunnelIPs, addr)
			}
		}
		p.LatestHandshakeUTC, _ = ParseIntJS(parts[4])
		p.RxBytes, _ = ParseIntJS(parts[5])
		p.TxBytes, _ = ParseIntJS(parts[6])
		peers = append(peers, p)
	}
	return peers
}

var (
	reSrc   = regexp.MustCompile(`\bsrc=(\S+)`)
	reDst   = regexp.MustCompile(`\bdst=(\S+)`)
	reDport = regexp.MustCompile(`\bdport=(\d+)`)
	l4      = map[string]bool{"tcp": true, "udp": true, "icmp": true, "icmpv6": true, "udplite": true, "dccp": true, "sctp": true, "gre": true}
)

// protocolOf is the first of the first four tokens that names an L4 protocol: it handles the
// /proc format (`ipv4 2 tcp 6 …`) and the conntrack -L format (`tcp 6 …`).
func protocolOf(parts []string) string {
	if len(parts) > 4 {
		parts = parts[:4]
	}
	for _, t := range parts {
		if l4[t] {
			return t
		}
	}
	return "unknown"
}

// ParseConntrack is parseConntrack: only lines whose FIRST src= is a known tunnel IP; the first
// dst=/dport= pair (the original direction — the second one is post-NAT and points at the node).
func ParseConntrack(contents string, owner map[string]string) []Flow {
	var flows []Flow
	for _, line := range strings.Split(contents, "\n") {
		if line == "" {
			continue
		}
		src := reSrc.FindStringSubmatch(line)
		if src == nil {
			continue
		}
		pk, ok := owner[src[1]]
		if !ok {
			continue
		}
		dst := reDst.FindStringSubmatch(line)
		dport := reDport.FindStringSubmatch(line)
		if dst == nil || dport == nil {
			continue
		}
		port, _ := ParseIntJS(dport[1])
		flows = append(flows, Flow{
			PublicKey: pk, TunnelIP: src[1], Protocol: protocolOf(Fields(line)),
			DestinationIP: dst[1], DestinationPort: uint32(port),
		})
	}
	return flows
}

// Observation is collectAwgObservation's result.
type Observation struct {
	Peers              []Peer
	Flows              []Flow
	ConntrackAvailable bool
}

// Collect is collectAwgObservation: always on awg0 (spec D §5.1, R5), whatever AWG_CONFIG_PATH
// says. A missing interface is an empty, successful observation (the orchestrator's peer integrity
// check relies on it).
func (s *Service) Collect(ctx context.Context) Observation {
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: "awg", Args: []string{"show", NATIface, "dump"}, Timeout: ShowTimeout})
	if err != nil {
		s.Log.Debug("AWG interface not available, no observation to report", "err", err.Error(), "iface", NATIface)
		return Observation{}
	}
	peers := ParseDump(res.Stdout)
	owner := map[string]string{}
	for _, p := range peers {
		for _, ip := range p.TunnelIPs {
			owner[ip] = p.PublicKey
		}
	}
	if len(owner) == 0 {
		return Observation{Peers: peers}
	}
	contents, ok := s.readConntrack(ctx)
	if !ok {
		return Observation{Peers: peers}
	}
	return Observation{Peers: peers, Flows: ParseConntrack(contents, owner), ConntrackAvailable: true}
}

func (s *Service) readConntrack(ctx context.Context) (string, bool) {
	raw, err := s.FS.ReadFile(ConntrackPath)
	if err == nil {
		return string(raw), true
	}
	s.Log.Debug("Kernel conntrack procfs unavailable, falling back to the conntrack utility", "err", err.Error())
	res, err := bigOutput(s.Runner).Run(ctx, runner.Cmd{Name: "conntrack", Args: []string{"-L"}, Timeout: conntrackTimeout})
	if err != nil {
		s.Log.Debug("Kernel conntrack table unreadable — reporting peer counters only", "err", err.Error())
		return "", false
	}
	return res.Stdout, true
}

// GetAwgObservation implements the RPC (spec D §5). Auth: metadata or body secret.
func (s *Service) GetAwgObservation(ctx context.Context, req *agentpb.AwgObservationRequest) (*agentpb.AwgObservationResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetAwgObservation request blocked")
		return &agentpb.AwgObservationResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	obs := s.Collect(ctx)
	resp := &agentpb.AwgObservationResponse{Success: true, ConntrackAvailable: obs.ConntrackAvailable}
	if len(obs.Peers) == 0 {
		resp.Message = "AWG interface is not active on this node."
	} else {
		resp.Message = strconv.Itoa(len(obs.Peers)) + " peer(s), " + strconv.Itoa(len(obs.Flows)) + " flow(s)."
	}
	for _, p := range obs.Peers {
		resp.Peers = append(resp.Peers, &agentpb.AwgPeerCounters{
			PublicKey: p.PublicKey, Endpoint: p.Endpoint, TunnelIps: p.TunnelIPs,
			RxBytes: p.RxBytes, TxBytes: p.TxBytes, LatestHandshakeUnix: p.LatestHandshakeUTC,
		})
	}
	for _, f := range obs.Flows {
		resp.Flows = append(resp.Flows, &agentpb.AwgFlow{
			PublicKey: f.PublicKey, TunnelIp: f.TunnelIP, Protocol: f.Protocol,
			DestinationIp: f.DestinationIP, DestinationPort: f.DestinationPort,
		})
	}
	return resp, nil
}
