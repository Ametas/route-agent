package awg

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// Fixtures from tests/awgFlows.test.ts.
var dump = strings.Join([]string{
	"privkey\tpubkey\t51820\toff",
	"peerA\t(none)\t203.0.113.10:51820\t100.64.0.2/32\t1788880000\t1500000\t900000\t25",
	"peerB\t(none)\t203.0.113.11:51820\t100.64.0.3/32,100.64.0.4/32\t1788880100\t80\t120\toff",
}, "\n")

var conntrackProc = strings.Join([]string{
	"ipv4     2 tcp      6 431999 ESTABLISHED src=100.64.0.2 dst=149.154.167.41 sport=51234 dport=443 src=149.154.167.41 dst=203.0.113.10 sport=443 dport=51234 [ASSURED] mark=0 use=1",
	"ipv4     2 udp      17 29 src=100.64.0.2 dst=198.51.100.7 sport=6881 dport=6881 src=198.51.100.7 dst=203.0.113.10 sport=6881 dport=6881 mark=0 use=1",
	"ipv4     2 tcp      6 300 ESTABLISHED src=10.9.9.9 dst=1.1.1.1 sport=40000 dport=443 src=1.1.1.1 dst=203.0.113.10 sport=443 dport=40000 [ASSURED] mark=0 use=1",
}, "\n")

var conntrackCLI = strings.Join([]string{
	"tcp      6 431999 ESTABLISHED src=100.64.0.2 dst=149.154.167.41 sport=51234 dport=443 src=149.154.167.41 dst=203.0.113.10 sport=443 dport=51234 [ASSURED] mark=0 use=1",
	"udp      17 29 src=100.64.0.2 dst=198.51.100.7 sport=6881 dport=6881 src=198.51.100.7 dst=203.0.113.10 sport=6881 dport=6881 mark=0 use=1",
}, "\n")

var owners = map[string]string{"100.64.0.2": "peerA"}

// Item 15: dump parsing.
func TestParseDump(t *testing.T) {
	peers := ParseDump(dump)
	if len(peers) != 2 || peers[0].PublicKey != "peerA" {
		t.Fatalf("%+v", peers)
	}
	if peers[0].RxBytes != 1500000 || peers[0].TxBytes != 900000 || peers[0].LatestHandshakeUTC != 1788880000 {
		t.Fatalf("%+v", peers[0])
	}
	if peers[0].Endpoint != "203.0.113.10:51820" {
		t.Fatal(peers[0].Endpoint)
	}
	if !reflect.DeepEqual(peers[1].TunnelIPs, []string{"100.64.0.3", "100.64.0.4"}) {
		t.Fatal(peers[1].TunnelIPs)
	}
	if p := ParseDump("iface\nnever\t(none)\t(none)\t100.64.0.9/32\t0\t0\t0\toff"); p[0].Endpoint != "" {
		t.Fatal("(none) endpoint")
	}
	if p := ParseDump("iface\nnokeys\t(none)\t-\t(none)\t0\t0\t0\toff"); len(p[0].TunnelIPs) != 0 || p[0].TunnelIPs == nil {
		t.Fatalf("(none) allowed ips: %#v", p[0].TunnelIPs)
	}
	if p := ParseDump("iface\nbroken\t(none)\t203.0.113.1:1"); len(p) != 0 {
		t.Fatal("truncated line accepted")
	}
	for _, junk := range []string{"", "\n", "только-интерфейс"} {
		if p := ParseDump(junk); len(p) != 0 {
			t.Fatalf("%q", junk)
		}
	}
	// parseInt semantics: leading digits; NaN → 0.
	p := ParseDump("iface\nk\t(none)\t(none)\t(none)\t12abc\tx\t-5\toff")
	if p[0].LatestHandshakeUTC != 12 || p[0].RxBytes != 0 || p[0].TxBytes != -5 {
		t.Fatalf("%+v", p[0])
	}
}

func TestParseConntrack(t *testing.T) {
	flows := ParseConntrack(conntrackProc, owners)
	if len(flows) != 2 {
		t.Fatalf("%+v", flows)
	}
	want := Flow{PublicKey: "peerA", TunnelIP: "100.64.0.2", Protocol: "tcp", DestinationIP: "149.154.167.41", DestinationPort: 443}
	if flows[0] != want || flows[1].Protocol != "udp" {
		t.Fatalf("%+v", flows)
	}
	cli := ParseConntrack(conntrackCLI, owners)
	if len(cli) != 2 || cli[0] != want || cli[1].Protocol != "udp" {
		t.Fatalf("%+v", cli)
	}
	if len(ParseConntrack(conntrackProc, map[string]string{})) != 0 {
		t.Fatal("empty owners")
	}
	junk := strings.Join([]string{"", "ipv4 2 icmp 1 29 type=8", "src=нет-остального", "ipv4 2 tcp 6 1 src=100.64.0.2"}, "\n")
	if len(ParseConntrack(junk, owners)) != 0 {
		t.Fatal("incomplete line accepted")
	}
	if f := ParseConntrack("weird 1 2 3 tcp src=100.64.0.2 dst=1.1.1.1 dport=1", owners); f[0].Protocol != "unknown" {
		t.Fatal("protocol is searched in the first four tokens only")
	}
}

// Items 16-19 through the RPC.
func TestGetAwgObservation(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		e := newEnv(t)
		r, _ := e.svc.GetAwgObservation(context.Background(), &agentpb.AwgObservationRequest{OrchestratorSecret: "nope"})
		if r.Success || r.Message != "Invalid orchestrator secret token." || len(e.calls()) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("body secret accepted, interface missing", func(t *testing.T) {
		e := newEnv(t)
		e.cfg.AwgConfigPath = "/x/wg7.conf" // ignored: always awg0
		e.sc.fail("awg show awg0 dump", "Unable to access interface: No such device")
		r, _ := e.svc.GetAwgObservation(context.Background(), &agentpb.AwgObservationRequest{OrchestratorSecret: secret})
		if !r.Success || r.Message != "AWG interface is not active on this node." || len(r.Peers) != 0 || r.ConntrackAvailable {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("procfs first", func(t *testing.T) {
		e := newEnv(t)
		e.sc.ok("awg show awg0 dump", dump)
		e.write(ConntrackPath, conntrackProc)
		r, _ := e.svc.GetAwgObservation(authed(), &agentpb.AwgObservationRequest{})
		if !r.Success || r.Message != "2 peer(s), 2 flow(s)." || !r.ConntrackAvailable || len(r.Flows) != 2 {
			t.Fatalf("%+v", r)
		}
		if hasPrefix(e.calls(), "conntrack") {
			t.Fatal("conntrack -L ran although procfs was readable")
		}
		if r.Peers[1].Endpoint != "203.0.113.11:51820" || r.Peers[0].LatestHandshakeUnix != 1788880000 {
			t.Fatalf("%+v", r.Peers)
		}
	})
	t.Run("conntrack -L fallback", func(t *testing.T) {
		e := newEnv(t)
		e.sc.ok("awg show awg0 dump", dump)
		e.sc.ok("conntrack -L", conntrackCLI)
		r, _ := e.svc.GetAwgObservation(authed(), &agentpb.AwgObservationRequest{})
		if !r.ConntrackAvailable || len(r.Flows) != 2 || r.Message != "2 peer(s), 2 flow(s)." {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("neither", func(t *testing.T) {
		e := newEnv(t)
		e.sc.ok("awg show awg0 dump", dump)
		e.sc.fail("conntrack -L", "conntrack v1.4.6: Operation failed")
		r, _ := e.svc.GetAwgObservation(authed(), &agentpb.AwgObservationRequest{})
		if !r.Success || r.ConntrackAvailable || len(r.Peers) != 2 || r.Message != "2 peer(s), 0 flow(s)." {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no tunnel ips: conntrack not read", func(t *testing.T) {
		e := newEnv(t)
		e.sc.ok("awg show awg0 dump", "iface\nk\t(none)\t(none)\t(none)\t0\t0\t0\toff")
		e.write(ConntrackPath, conntrackProc)
		r, _ := e.svc.GetAwgObservation(authed(), &agentpb.AwgObservationRequest{})
		if r.ConntrackAvailable || len(r.Peers) != 1 || r.Message != "1 peer(s), 0 flow(s)." {
			t.Fatalf("%+v", r)
		}
		if hasPrefix(e.calls(), "conntrack") {
			t.Fatal("conntrack read")
		}
	})
}

// Telemetry reader: awg_active_peers and the shared live-peer scan (tests/meshPeerDump.test.ts).
func TestLivePeers(t *testing.T) {
	const now = 1_790_400_000
	d := strings.Join([]string{
		"PRIVKEY=\tPUBKEY=\t51821\toff",
		"live1=\t(none)\t146.19.128.73:51821\t100.100.0.5/32\t" + itoa(now-27) + "\t118000\t164000\t25",
		"never=\t(none)\t195.209.218.173:51821\t100.100.0.3/32\t0\t0\t114000\t25",
		"stale=\t(none)\t77.221.151.250:51821\t100.100.0.2/32\t" + itoa(now-181) + "\t0\t114000\t25",
		"edge=\t(none)\t2.27.54.246:51821\t100.100.0.4/32\t" + itoa(now-180) + "\t10\t20\t25",
	}, "\n")
	if got := LivePeers(d, now); !reflect.DeepEqual(got, []string{"live1=", "edge="}) {
		t.Fatal(got)
	}
	if got := LivePeers("PRIVKEY=\tPUBKEY=\t51821\toff\n", now); len(got) != 0 || got == nil {
		t.Fatalf("%#v", got)
	}
	lines := strings.Split(d, "\n")
	if got := LivePeers("iface\nbroken=\t(none)\n"+lines[1], now); !reflect.DeepEqual(got, []string{"live1="}) {
		t.Fatal(got)
	}

	e := newEnv(t)
	e.sc.ok("awg show awg0 dump", d)
	if n := ActivePeers(context.Background(), e.run, timeUnix(now)); n != 2 {
		t.Fatal(n)
	}
	e.sc.fail("awg show awg0 dump", "no device")
	if n := ActivePeers(context.Background(), e.run, timeUnix(now)); n != 0 {
		t.Fatal(n)
	}
}
