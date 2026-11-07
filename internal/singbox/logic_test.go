package singbox

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/runner"
)

// Ports of tests/firewallSyncPlan.test.ts, tests/singboxStats.test.ts,
// tests/listenerWarning.test.ts and the unit/version parts of tests/app.test.ts.

func TestPlanFirewallSync(t *testing.T) {
	type c struct {
		next, prev  []float64
		open, close []float64
		reload      bool
	}
	for i, tc := range []c{
		{[]float64{443, 8443}, []float64{443, 8443}, nil, nil, false},
		{[]float64{8443, 443}, []float64{443, 8443}, nil, nil, false},
		{[]float64{443, 8443}, []float64{443}, []float64{8443}, nil, true},
		{[]float64{443}, []float64{443, 8443}, nil, []float64{8443}, true},
		{[]float64{443, 8443}, nil, []float64{443, 8443}, nil, true},
		{nil, []float64{443, 8443}, nil, []float64{443, 8443}, true},
		{nil, nil, nil, nil, false},
	} {
		p := PlanFirewallSync(tc.next, tc.prev)
		if !reflect.DeepEqual(p.ToOpen, tc.open) || !reflect.DeepEqual(p.ToClose, tc.close) || p.NeedsReload != tc.reload {
			t.Errorf("case %d: %+v", i, p)
		}
	}
}

func TestExtractUDPTunnelPorts(t *testing.T) {
	got := ExtractUDPTunnelPorts(js(t, `{"inbounds":[
		{"type":"direct","tag":"quic-dispatch","listen":"0.0.0.0","listen_port":443},
		{"type":"tuic","listen":"127.0.0.1","listen_port":20001},
		{"type":"hysteria2","listen":"::1","listen_port":20002},
		{"type":"hysteria2","listen":"0.0.0.0","listen_port":2053},
		{"type":"tuic","listen_port":40013},
		{"listen":" localhost ","listen_port":1},
		{"port":"5000"},
		{"type":5,"listen_port":null,"port":6000},
		{"type":"tuic","listen_port":70000},
		{"type":"tuic","listen_port":2053},
		[1,2], null]}`))
	if !reflect.DeepEqual(got, []float64{2053, 40013, 5000, 6000}) {
		t.Fatalf("got %v (a missing or non-string type is counted, spec B R17)", got)
	}
}

func conn(id, user string, up, down float64) clashapi.Record {
	return clashapi.Record{ID: id, User: user, UploadBytes: up, DownloadBytes: down}
}

func TestComputeUserTrafficDeltas(t *testing.T) {
	empty := map[string]Totals{}
	d, next := ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 5e9, 9e9)}, empty, true)
	if len(d) != 0 || next["c1"] != (Totals{5e9, 9e9}) {
		t.Fatal("first pass records only the baseline")
	}
	_, n1 := ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 1000, 2000)}, empty, true)
	d, _ = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 1500, 2600)}, n1, false)
	if !reflect.DeepEqual(d, []UserDelta{{"user-a", 500, 600}}) {
		t.Fatalf("increment only: %v", d)
	}
	_, n1 = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 1000, 1000)}, empty, true)
	d, _ = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 1000, 1000), conn("c2", "user-a", 700, 300)}, n1, false)
	if !reflect.DeepEqual(d, []UserDelta{{"user-a", 700, 300}}) {
		t.Fatalf("new connection counts in full: %v", d)
	}
	_, n1 = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 400, 100)}, empty, true)
	d2, n2 := ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 900, 250)}, n1, false)
	d3, _ := ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 900, 250)}, n2, false)
	if !reflect.DeepEqual(d2, []UserDelta{{"user-a", 500, 150}}) || len(d3) != 0 {
		t.Fatal("not double-counted")
	}
	if d, n := ComputeUserTrafficDeltas(nil, n1, false); len(d) != 0 || len(n) != 0 {
		t.Fatal("absent ids are forgotten")
	}
	_, n1 = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 0, 0), conn("c2", "user-b", 0, 0), conn("c3", "user-a", 0, 0)}, empty, true)
	d, _ = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 100, 10), conn("c2", "user-b", 50, 5), conn("c3", "user-a", 200, 20)}, n1, false)
	if !reflect.DeepEqual(d, []UserDelta{{"user-a", 300, 30}, {"user-b", 50, 5}}) {
		t.Fatalf("per user, first-seen order: %v", d)
	}
	_, n1 = ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 5000, 5000)}, empty, true)
	if d, _ := ComputeUserTrafficDeltas([]clashapi.Record{conn("c1", "user-a", 100, 100)}, n1, false); len(d) != 0 {
		t.Fatal("backwards counter must give nothing")
	}
	if d, n := ComputeUserTrafficDeltas([]clashapi.Record{conn("", "user-a", 900, 900), conn("c2", "", 900, 900)}, empty, false); len(d) != 0 || len(n) != 0 {
		t.Fatal("empty id or user dropped")
	}
	// Cap: beyond MaxTrackedConnections ids are not remembered (and counted in full next time).
	many := make([]clashapi.Record, MaxTrackedConnections+5)
	for i := range many {
		many[i] = conn("id"+itoa(i), "u", 1, 1)
	}
	if _, n := ComputeUserTrafficDeltas(many, empty, true); len(n) != MaxTrackedConnections {
		t.Fatalf("tracked %d", len(n))
	}
}

func itoa(i int) string {
	b := []byte{}
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
		if i == 0 {
			return string(b)
		}
	}
}

const ssUDP = "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\n" +
	"UNCONN 0      0            0.0.0.0:443        0.0.0.0:*\n" +
	"UNCONN 0      0          127.0.0.1:20000      0.0.0.0:*\n"
const ssEmpty = "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\n"

func TestListenerHelpers(t *testing.T) {
	ports := PublicListenPorts(js(t, `{"inbounds":[{"type":"hysteria2","listen":"::","listen_port":443},
		{"type":"vless","listen":"127.0.0.1","listen_port":20000},{"type":"tuic","listen_port":8443},{"listen_port":"9"},{"listen_port":1.5}]}`))
	if !reflect.DeepEqual(ports, []float64{443, 8443}) {
		t.Fatal(ports)
	}
	got := ParseListeningPorts(ssUDP)
	sort.Float64s(got)
	if !reflect.DeepEqual(got, []float64{443, 20000}) {
		t.Fatal(got)
	}
}

func TestDetectCoreNotListening(t *testing.T) {
	h := newHarness(t)
	ss := ssUDP
	var ssErr error
	h.fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		if c.Name == "ss" {
			if ssErr != nil {
				return runner.Result{}, ssErr
			}
			return runner.Result{Stdout: ss}, nil
		}
		return runner.Result{}, nil
	}
	h.svc.Now = func() time.Time { return time.UnixMilli(1_800_000_000_000) }

	if w := h.svc.DetectCoreNotListening(context.Background()); w != nil {
		t.Fatal("no config → nil")
	}
	h.writeConfig(`{"inbounds":[{"listen":"127.0.0.1","listen_port":29000}]}`)
	if w := h.svc.DetectCoreNotListening(context.Background()); w != nil || len(h.fake.Calls()) != 0 {
		t.Fatal("no public inbound: nil and ss not even run")
	}
	h.writeConfig(`{"inbounds":[{"listen":"::","listen_port":443}]}`)
	if w := h.svc.DetectCoreNotListening(context.Background()); w != nil {
		t.Fatal("all listening → nil")
	}
	ds := h.fake.Displays()
	sort.Strings(ds)
	if strings.Join(ds, "|") != "ss -lnt|ss -lnu" {
		t.Fatalf("ss commands: %q", ds)
	}
	ss = ssEmpty
	h.writeConfig(`{"inbounds":[{"listen":"::","listen_port":443},{"listen":"","listen_port":8443},{"listen":"0.0.0.0","listen_port":20000}]}`)
	w := h.svc.DetectCoreNotListening(context.Background())
	if w == nil || w.Kind != "core_not_listening" || w.Source != "sing-box" || w.Occurrences != 3 || w.LastSeenUnixMs != 1_800_000_000_000 ||
		w.Sample != "конфиг объявляет порты 443, 8443, 20000, но никто не слушает 443, 8443, 20000" {
		t.Fatalf("%+v", w)
	}
	ssErr = errors.New("ss: command not found")
	if w := h.svc.DetectCoreNotListening(context.Background()); w != nil {
		t.Fatal("ss unavailable → nil")
	}
	h.writeConfig("")
	ssErr = nil
	if w := h.svc.DetectCoreNotListening(context.Background()); w != nil {
		t.Fatal("empty config → nil")
	}
}

func TestParseVersionOutput(t *testing.T) {
	cases := map[string]string{
		"sing-box version 1.14.0-hotusers\n\nEnvironment: go1.24\n": "1.14.0-hotusers",
		"sing-box version 1.12.3\n":                                 "1.12.3",
		"Version  1.15.0-beta.1 extra":                              "1.15.0-beta.1",
		"  something odd \nmore":                                    "something odd",
		"":                                                          "not_installed",
	}
	for in, want := range cases {
		if got := ParseVersionOutput(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	h.version = "sing-box version 1.14.0-hotusers\n"
	if v := h.svc.Version(context.Background()); v != "not_installed" || len(h.fake.Calls()) != 0 {
		t.Fatal("no unit file → not_installed without running the binary")
	}
	h.writeUnit()
	if v := h.svc.Version(context.Background()); v != "1.14.0-hotusers" || !h.svc.IsFork(context.Background()) {
		t.Fatal(v)
	}
	if got := h.fake.Displays()[0]; got != "/usr/local/bin/sing-box version" {
		t.Fatal(got)
	}
	h.versionErr = true
	if v := h.svc.Version(context.Background()); v != "not_installed" {
		t.Fatal(v)
	}
}

// Checklist 36 (sing-box part) + app.test.ts ensureSingboxSystemdUnit.
func TestEnsureUnit(t *testing.T) {
	h := newHarness(t)
	want := "[Unit]\nDescription=sing-box service\nAfter=network.target nss-lookup.target\n\n[Service]\n" +
		"ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json\nRestart=always\nRestartSec=5\n" +
		"ExecReload=/bin/sh -c \"/usr/local/bin/sing-box check -c /etc/sing-box/config.json && /bin/kill -HUP $MAINPID\"\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	if !EnsureUnit(context.Background(), h.fs, h.svc.Systemctl, h.svc.Log, h.svc.Cfg, "") {
		t.Fatal("first call must write")
	}
	b, _ := os.ReadFile(h.fs.Path(DefaultUnitPath))
	if string(b) != want {
		t.Fatalf("unit:\n%s", b)
	}
	if got := h.fake.Displays(); len(got) != 1 || got[0] != "systemctl daemon-reload" {
		t.Fatal(got)
	}
	if EnsureUnit(context.Background(), h.fs, h.svc.Systemctl, h.svc.Log, h.svc.Cfg, "") || len(h.fake.Calls()) != 1 {
		t.Fatal("second call must neither write nor daemon-reload")
	}
}
