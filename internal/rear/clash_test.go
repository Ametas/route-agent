package rear

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
)

// clashServer is a fake rear Clash API recording every request.
type clashServer struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []recorded
	status   map[string]int    // "METHOD path" → status
	bodies   map[string]string // "METHOD path" → body
	putReply int
}

type recorded struct {
	method, path, auth, ctype, body string
}

func newClash(t *testing.T) *clashServer {
	c := &clashServer{status: map[string]int{}, bodies: map[string]string{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		c.mu.Lock()
		c.reqs = append(c.reqs, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)})
		st, body := c.status[key], c.bodies[key]
		c.mu.Unlock()
		if st == 0 {
			st = 200
			if r.Method == http.MethodPut {
				st = 204
			}
		}
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *clashServer) addr() string { return strings.TrimPrefix(c.URL, "http://") }

func (c *clashServer) requests() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorded(nil), c.reqs...)
}

func (h *harness) singboxRear(addr, secret string) {
	h.write("/etc/route-agent/rear.json", `{"experimental":{"clash_api":{"external_controller":" `+addr+` ","secret":"`+secret+`"}}}`)
}

func (h *harness) mihomoRear(addr, secret string) {
	h.write("/etc/route-agent/rear.yaml", `{"external-controller":"`+addr+`","secret":"`+secret+`"}`)
}

// 18. Active core and Clash API discovery.
func TestActiveClashAPI(t *testing.T) {
	h := newHarness(t, nil)
	if h.svc.ActiveClashAPI() != nil || h.svc.ActiveCore() != nil {
		t.Fatal("no rear on disk")
	}
	h.singboxRear("127.0.0.1:1", "s")
	api := h.svc.ActiveClashAPI()
	if api == nil || api.Core.ID != Singbox || api.Endpoint.Address != "127.0.0.1:1" || api.Endpoint.Secret != "s" {
		t.Fatalf("sing-box api %+v", api)
	}
	h.mihomoRear("127.0.0.1:2", "m")
	api = h.svc.ActiveClashAPI()
	if api == nil || api.Core.ID != Mihomo || api.Endpoint.Address != "127.0.0.1:2" || api.Core.WarpMembers.Direct != "DIRECT" {
		t.Fatalf("mihomo takes precedence: %+v", api)
	}
	// mihomo reads top-level fields only; nested sing-box shape is "no API".
	h.write("/etc/route-agent/rear.yaml", `{"experimental":{"clash_api":{"external_controller":"127.0.0.1:2"}}}`)
	if h.svc.ActiveClashAPI() != nil {
		t.Fatal("mihomo must not read the sing-box shape")
	}
	for _, bad := range []string{`{"external-controller":"   "}`, `external-controller: 127.0.0.1:2`, `null`, `[]`} {
		h.write("/etc/route-agent/rear.yaml", bad)
		if h.svc.ActiveClashAPI() != nil {
			t.Fatalf("%q must read as no rear", bad)
		}
	}
}

const proxiesPayload = `{"proxies":{
 "warp":{"type":"Selector","now":"wg-pool"},
 "wg-pool":{"history":[{"delay":10}]},
 "warp-auto":{"history":[{"delay":10}]},
 "warp-":{"history":[{"delay":10}]},
 "warp-a":{"history":[{"time":"2026-09-15T10:00:00.123456789Z","delay":0},{"time":"2026-09-15T10:00:01.5Z","delay":42}]},
 "warp-b":{"history":[{"delay":12},{"delay":0}]},
 "warp-c":{"history":[]},
 "warp-d":{"history":[{"delay":"7"}]}
}}`

// 20. Key parsing: warp-<id>, not the group, last history entry decides.
func TestParseWarpKeyHealth(t *testing.T) {
	keys := ParseWarpKeyHealth(mustParse(t, proxiesPayload))
	if len(keys) != 4 {
		t.Fatalf("keys %+v", keys)
	}
	a := keys[0]
	if a.EndpointTag != "warp-a" || !a.Alive || a.RttMs != 42 || a.MeasuredAtUnixMs != 1789466401500 {
		t.Fatalf("warp-a %+v", a)
	}
	for _, k := range keys[1:] {
		if k.Alive || k.RttMs != 0 || k.MeasuredAtUnixMs != 0 {
			t.Fatalf("%s must be dead: %+v", k.EndpointTag, k)
		}
	}
	if len(ParseWarpKeyHealth(mustParse(t, `{"proxies":"x"}`))) != 0 || len(ParseWarpKeyHealth(nil)) != 0 {
		t.Fatal("malformed payloads give no keys")
	}
}

// 20, 21. Decision per core.
func TestDecideWarpSelection(t *testing.T) {
	sb := WarpMembers{Pool: "wg-pool", Direct: "direct"}
	mh := WarpMembers{Pool: "warp-auto", Direct: "DIRECT"}
	if d := DecideWarpSelection(mustParse(t, proxiesPayload), sb); d == nil || d.Desired != "wg-pool" || d.AliveKeys != 1 || d.TotalKeys != 4 {
		t.Fatalf("alive pool %+v", d)
	}
	dead := `{"proxies":{"warp":{"now":"warp-auto"},"warp-auto":{"history":[{"delay":5}]},"warp-x":{"history":[{"delay":0}]}}}`
	if d := DecideWarpSelection(mustParse(t, dead), mh); d == nil || d.Desired != "DIRECT" || d.AliveKeys != 0 || d.TotalKeys != 1 {
		t.Fatalf("dead pool %+v", d)
	}
	for _, p := range []string{`{"proxies":{"warp-a":{"history":[{"delay":5}]}}}`, `{}`, `null`, `{"proxies":[]}`} {
		if DecideWarpSelection(mustParse(t, p), sb) != nil {
			t.Fatalf("no selector must mean no action: %s", p)
		}
	}
	if now, ok := CurrentSelection(mustParse(t, proxiesPayload)); !ok || now != "wg-pool" {
		t.Fatal("current selection")
	}
	if _, ok := CurrentSelection(mustParse(t, `{"proxies":{"warp":{"now":5}}}`)); ok {
		t.Fatal("non-string now")
	}
}

// 19, 21. Guard pass over HTTP: GET with bearer only when a secret exists; PUT only on a change.
func TestGuardOnce(t *testing.T) {
	c := newClash(t)
	h := newHarness(t, nil)
	if to, err := h.svc.GuardOnce(context.Background()); to != "" || err != nil {
		t.Fatal("no rear: no action")
	}
	h.singboxRear(c.addr(), "")
	c.bodies["GET /proxies"] = proxiesPayload // now == desired
	if to, err := h.svc.GuardOnce(context.Background()); to != "" || err != nil {
		t.Fatalf("to=%q err=%v", to, err)
	}
	if rs := c.requests(); len(rs) != 1 || rs[0].auth != "" {
		t.Fatalf("requests %+v", rs)
	}

	h.mihomoRear(c.addr(), "sec")
	c.bodies["GET /proxies"] = `{"proxies":{"warp":{"now":"warp-auto"},"warp-k":{"history":[{"delay":0}]}}}`
	to, err := h.svc.GuardOnce(context.Background())
	if to != "DIRECT" || err != nil {
		t.Fatalf("to=%q err=%v", to, err)
	}
	rs := c.requests()
	put := rs[len(rs)-1]
	if put.method != "PUT" || put.path != "/proxies/warp" || put.body != `{"name":"DIRECT"}` || put.ctype != "application/json" || put.auth != "Bearer sec" {
		t.Fatalf("put %+v", put)
	}
	if rs[len(rs)-2].auth != "Bearer sec" {
		t.Fatal("GET must carry the bearer")
	}

	c.status["PUT /proxies/warp"] = 400
	if _, err := h.svc.GuardOnce(context.Background()); err == nil || err.Error() != "clash api responded 400 selecting DIRECT" {
		t.Fatalf("err %v", err)
	}
	c.status["GET /proxies"] = 503
	if _, err := h.svc.GuardOnce(context.Background()); err == nil || err.Error() != "clash api responded 503 for /proxies" {
		t.Fatalf("err %v", err)
	}
}

// 19. The loop: interval from config, no immediate first tick.
func TestGuardLoop(t *testing.T) {
	h := newHarness(t, map[string]string{"REAR_WARP_GUARD_INTERVAL_MS": "1500"})
	l := h.svc.GuardLoop()
	if l.Interval != 1500*time.Millisecond || l.FirstDelay != 0 || l.Tick == nil {
		t.Fatalf("loop %+v", l)
	}
	if newHarness(t, nil).svc.GuardLoop().Interval != 30*time.Second {
		t.Fatal("default interval 30 s")
	}
	l.Tick(context.Background()) // a pass without rear must not panic
}

// 22–24. GetRearConnections.
func TestGetRearConnections(t *testing.T) {
	c := newClash(t)
	h := newHarness(t, nil)
	resp, _ := h.svc.GetRearConnections(mdCtx("bad"), &agentpb.SingBoxConnectionsRequest{})
	if resp.Success || resp.Message != "Invalid orchestrator secret token." {
		t.Fatalf("auth %+v", resp)
	}
	resp, _ = h.svc.GetRearConnections(context.Background(), &agentpb.SingBoxConnectionsRequest{OrchestratorSecret: secret})
	if !resp.Success || resp.Message != "Rear core is not active on this node." || len(resp.Connections) != 0 {
		t.Fatalf("no rear %+v", resp)
	}
	h.mihomoRear(c.addr(), "")
	c.bodies["GET /connections"] = `{"connections":[]}`
	resp, _ = h.svc.GetRearConnections(mdCtx(secret), &agentpb.SingBoxConnectionsRequest{})
	if !resp.Success || resp.Message != "Rear core is not active on this node." {
		t.Fatalf("empty %+v", resp)
	}
	c.bodies["GET /connections"] = `{"connections":[
	 {"id":"1","upload":10,"download":"20","start":"2026-09-15T10:00:00.123456789Z","metadata":{"network":"tcp","sourceIP":"10.0.0.2","destinationIP":"1.1.1.1","destinationPort":"443","host":"","sniffHost":"x.com","inboundName":"vless-in","inboundUser":"link"}},
	 {"id":"2","metadata":{"host":"h.com","sniffHost":"s.com","destinationPort":"0x1A"}},
	 {"id":"3"}, 7]}`
	resp, _ = h.svc.GetRearConnections(mdCtx(secret), &agentpb.SingBoxConnectionsRequest{})
	if !resp.Success || resp.Message != "OK" || len(resp.Connections) != 4 {
		t.Fatalf("resp %+v", resp)
	}
	a := resp.Connections[0]
	if a.User != "link" || a.SourceIp != "10.0.0.2" || a.DestinationIp != "1.1.1.1" || a.DestinationDomain != "x.com" || a.DestinationPort != 443 ||
		a.UploadBytes != 10 || a.DownloadBytes != 20 || a.Network != "tcp" || a.InboundTag != "vless-in" || a.StartedAtUnixMs != 1789466400123 || a.Closed {
		t.Fatalf("a %+v", a)
	}
	if b := resp.Connections[1]; b.DestinationDomain != "h.com" || b.DestinationPort != 26 {
		t.Fatalf("b %+v", b)
	}
	if z := resp.Connections[3]; z.User != "" || z.DestinationPort != 0 || z.StartedAtUnixMs != 0 {
		t.Fatalf("junk %+v", z)
	}
	c.status["GET /connections"] = 401
	resp, _ = h.svc.GetRearConnections(mdCtx(secret), &agentpb.SingBoxConnectionsRequest{})
	if resp.Success || resp.Message != "rear clash api responded 401" || len(resp.Connections) != 0 {
		t.Fatalf("non-2xx %+v", resp)
	}
}

// 25. GetWarpKeyHealth body: core-aware (mihomo rear is measured — deviation from TS, R2).
func TestWarpKeyHealthReport(t *testing.T) {
	c := newClash(t)
	h := newHarness(t, nil)
	r := h.svc.WarpKeyHealthReport(context.Background())
	if !r.Success || r.SkippedReason != "rear_not_running" || r.Message != "Rear sing-box is not configured on this node." {
		t.Fatalf("no rear %+v", r)
	}
	h.write("/etc/route-agent/rear.yaml", `{}`)
	r = h.svc.WarpKeyHealthReport(context.Background())
	if !r.Success || r.SkippedReason != "rear_not_running" || r.Message != "Rear sing-box config carries no clash_api endpoint." {
		t.Fatalf("no endpoint %+v", r)
	}
	h.mihomoRear(c.addr(), "")
	c.bodies["GET /proxies"] = proxiesPayload
	r = h.svc.WarpKeyHealthReport(context.Background())
	if !r.Success || r.SkippedReason != "" || r.Message != "Reported health for 4 WARP endpoint(s)." || len(r.Entries) != 4 ||
		r.Entries[0].EndpointTag != "warp-a" || !r.Entries[0].Alive || r.Entries[0].RttMs != 42 || r.Entries[0].MeasuredAtUnixMs != 1789466401500 {
		t.Fatalf("mihomo rear %+v", r)
	}
	c.Close()
	r = h.svc.WarpKeyHealthReport(context.Background())
	if r.Success || r.SkippedReason != "clash_api_unreachable" || r.Message != "Could not read rear clash api: fetch failed" {
		t.Fatalf("down %+v", r)
	}
}

// 26. Empty rule providers.
func TestEmptyRuleProviders(t *testing.T) {
	got := EmptyRuleProviders(mustParse(t, `{"providers":{
	 "b-key":{"name":"zeta","ruleCount":0},"a-key":{"ruleCount":-1},"full":{"name":"full","ruleCount":5},
	 "str":{"name":"str","ruleCount":"0"},"nul":{"name":"nul","ruleCount":null},"x":7,"Alpha":{"name":"","ruleCount":0}}}`))
	if strings.Join(got, ",") != "Alpha,a-key,zeta" {
		t.Fatalf("got %v", got)
	}
	for _, p := range []string{`null`, `{"providers":null}`, `{"providers":"x"}`, `[]`} {
		if len(EmptyRuleProviders(mustParse(t, p))) != 0 {
			t.Fatalf("%s", p)
		}
	}
}

func TestDetectEmptyRuleProviders(t *testing.T) {
	c := newClash(t)
	h := newHarness(t, nil)
	h.svc.Now = func() time.Time { return time.UnixMilli(1700000000000) }
	if h.svc.DetectEmptyRuleProviders(context.Background()) != nil {
		t.Fatal("no rear, no warning")
	}
	h.mihomoRear(c.addr(), "")
	c.bodies["GET /providers/rules"] = `{"providers":{"ru":{"name":"geosite-ru","ruleCount":0},"ai":{"name":"category-ai-!cn","ruleCount":0}}}`
	w := h.svc.DetectEmptyRuleProviders(context.Background())
	if w == nil || w.Kind != "rear_rule_provider_empty" || w.Source != "mihomo" || w.Sample != "наборы без правил: category-ai-!cn, geosite-ru" ||
		w.Occurrences != 2 || w.LastSeenUnixMs != 1700000000000 {
		t.Fatalf("warning %+v", w)
	}
	// The client sends "Bearer " (empty secret); the server side trims the trailing space.
	if rs := c.requests(); rs[len(rs)-1].auth != "Bearer" {
		t.Fatalf("Authorization must always be sent: %q", rs[len(rs)-1].auth)
	}
	c.status["GET /providers/rules"] = 404
	if h.svc.DetectEmptyRuleProviders(context.Background()) != nil {
		t.Fatal("failure must be silent")
	}
	c.status["GET /providers/rules"] = 200
	c.bodies["GET /providers/rules"] = `not json`
	if h.svc.DetectEmptyRuleProviders(context.Background()) != nil {
		t.Fatal("bad JSON must be silent")
	}
}

// Hooks for group C's uploads.
func TestRearHooks(t *testing.T) {
	h := newHarness(t, map[string]string{"REAR_MIHOMO_UNIT_FILE_PATH": "/etc/systemd/system/my-rear.service"})
	if u, ok := h.svc.MihomoUnit(); u != "my-rear" || ok {
		t.Fatalf("unit %q %v", u, ok)
	}
	h.write("/etc/systemd/system/my-rear.service", "x")
	if u, ok := h.svc.MihomoUnit(); u != "my-rear" || !ok {
		t.Fatal("provisioned unit; name derived from the configured path (spec F R8)")
	}

	h.inactive["my-rear"] = true
	h.svc.ReloadAfterRuleSet(context.Background(), "geosite-ru")
	if h.ranPrefix("systemctl reload") {
		t.Fatal("inactive rear must not be reloaded")
	}
	h.inactive["my-rear"] = false
	h.svc.ReloadAfterRuleSet(context.Background(), "geosite-ru")
	if !h.ran("systemctl reload my-rear") {
		t.Fatalf("calls %v", h.displays())
	}

	if _, _, rejected := h.svc.VerifyMihomoConfig(context.Background(), "/tmp/cand"); rejected {
		t.Fatal("no live config: nothing to check")
	}
	h.write("/etc/route-agent/rear.yaml", "{}")
	h.mihomoT = func(c runner.Cmd) error {
		if c.Name != "/tmp/cand" || c.Args[2] != h.path("/etc/route-agent") || c.Args[4] != h.path("/etc/route-agent/rear.yaml") {
			t.Errorf("cmd %+v", c)
		}
		return errAt("unknown field")
	}
	cfg, text, rejected := h.svc.VerifyMihomoConfig(context.Background(), "/tmp/cand")
	if !rejected || cfg != "/etc/route-agent/rear.yaml" || text != "unknown field" {
		t.Fatalf("%q %q %v", cfg, text, rejected)
	}
}

type errAt string

func (e errAt) Error() string { return string(e) }
