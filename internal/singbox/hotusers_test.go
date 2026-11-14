package singbox

import (
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/jsjson"
)

// Ports of tests/hotUsers.test.ts and tests/hotSwapBlocker.test.ts.

func js(t *testing.T, s string) any {
	t.Helper()
	v, err := jsjson.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const baseCfg = `{"log":{"level":"warn"},"inbounds":[
 {"type":"tuic","tag":"lightning-tuic","listen":"::","listen_port":8443,"users":[{"name":"a","uuid":"u-a","password":"p-a"}]},
 {"type":"hysteria2","tag":"lightning-hy2","listen":"::","listen_port":8444,"users":[{"name":"a","password":"p-a"}]}],
 "outbounds":[{"type":"direct","tag":"direct"}]}`

// set replaces the value at a dotted path like "inbounds.0.users".
func set(t *testing.T, v any, path string, val any) any {
	t.Helper()
	parts := strings.Split(path, ".")
	cur := v
	for i, p := range parts {
		last := i == len(parts)-1
		switch c := cur.(type) {
		case *jsjson.Object:
			if last {
				c.Set(p, val)
				return v
			}
			cur, _ = c.Get(p)
		case []any:
			idx := 0
			for _, ch := range p {
				idx = idx*10 + int(ch-'0')
			}
			if last {
				c[idx] = val
				return v
			}
			cur = c[idx]
		}
	}
	return v
}

func TestWithUsersAPIService(t *testing.T) {
	once, err := WithUsersAPIService(js(t, baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	s := jsjson.Stringify(once, "")
	if !strings.HasSuffix(s, `,"services":[{"type":"users-api","tag":"users","path":"/run/sing-box/users.sock"}]}`) {
		t.Fatalf("services must be appended as the last key with type,tag,path order: %s", s)
	}
	twice, _ := WithUsersAPIService(once)
	if jsjson.Stringify(twice, "") != s {
		t.Fatal("service duplicated on a repeated push")
	}
	other, _ := WithUsersAPIService(js(t, `{"services":[{"type":"derp","tag":"derp-in"}],"log":{}}`))
	if got := jsjson.Stringify(other, ""); got != `{"services":[{"type":"derp","tag":"derp-in"},{"type":"users-api","tag":"users","path":"/run/sing-box/users.sock"}],"log":{}}` {
		t.Fatalf("existing services must keep their position and content: %s", got)
	}
	if _, err := WithUsersAPIService(nil); err == nil || err.Error() != "Cannot read properties of null (reading 'services')" {
		t.Fatal(err)
	}
}

func TestPlanRosterUpdate(t *testing.T) {
	base := js(t, baseCfg)
	plan := func(next any) ([]RosterUpdate, bool) {
		p, ok, err := PlanRosterUpdate(next, base)
		if err != nil {
			t.Fatal(err)
		}
		return p, ok
	}

	next := set(t, js(t, baseCfg), "inbounds.0.users", js(t, `[{"name":"a","uuid":"u-a","password":"p-a"},{"name":"b","uuid":"u-b","password":"p-b"}]`))
	if p, ok := plan(next); !ok || len(p) != 1 || p[0].Tag != "lightning-tuic" || len(p[0].Users) != 2 {
		t.Fatalf("add user: %v %v", p, ok)
	}
	next = set(t, js(t, baseCfg), "inbounds.0.users", []any{})
	if p, ok := plan(next); !ok || len(p) != 1 || len(p[0].Users) != 0 {
		t.Fatalf("remove user: %v %v", p, ok)
	}
	next = set(t, set(t, js(t, baseCfg), "inbounds.0.users", []any{}), "inbounds.1.users", []any{})
	if p, ok := plan(next); !ok || len(p) != 2 || p[0].Tag != "lightning-tuic" || p[1].Tag != "lightning-hy2" {
		t.Fatalf("both: %v", p)
	}
	if p, ok := plan(js(t, baseCfg)); !ok || p == nil || len(p) != 0 {
		t.Fatalf("identical must be an empty plan, not a refusal: %v %v", p, ok)
	}
	refused := map[string]any{
		"port change":     set(t, js(t, baseCfg), "inbounds.0.listen_port", 9443.0),
		"outbounds":       set(t, js(t, baseCfg), "outbounds", js(t, `[{"type":"direct","tag":"direct"},{"type":"block","tag":"block"}]`)),
		"added inbound":   set(t, js(t, baseCfg), "inbounds", append(func() []any { v, _ := js(t, baseCfg).(*jsjson.Object).Get("inbounds"); return v.([]any) }(), js(t, `{"type":"tuic","tag":"extra","users":[]}`))),
		"users-api added": func() any { v, _ := WithUsersAPIService(js(t, baseCfg)); return v }(),
		"no inbounds":     js(t, `{"log":{"level":"warn"}}`),
	}
	for name, n := range refused {
		if _, ok := plan(n); ok {
			t.Errorf("%s: must reload", name)
		}
	}
	if _, ok, _ := PlanRosterUpdate(base, js(t, `{"log":{"level":"warn"}}`)); ok {
		t.Error("no inbounds on disk")
	}
	ss := `{"inbounds":[{"type":"shadowsocks","tag":"ss-in","listen":"::","listen_port":8500,"users":[{"name":"a"}]}]}`
	if _, ok, _ := PlanRosterUpdate(set(t, js(t, ss), "inbounds.0.users", js(t, `[{"name":"b"}]`)), js(t, ss)); ok {
		t.Error("shadowsocks")
	}
	// naive goes out as it is: the fork's users-api takes `username` as an alias for `name`.
	naive := `{"inbounds":[{"type":"naive","tag":"naive-in-f1","listen":"127.0.0.1","listen_port":25001,"network":"udp","users":[{"username":"a","password":"p-a"}]}]}`
	if p, ok, _ := PlanRosterUpdate(set(t, js(t, naive), "inbounds.0.users", js(t, `[{"username":"b","password":"p-b"}]`)), js(t, naive)); !ok || len(p) != 1 || p[0].Tag != "naive-in-f1" || jsjson.Stringify(p[0].Users, "") != `[{"username":"b","password":"p-b"}]` {
		t.Errorf("naive: %v %v", p, ok)
	}
	untagged := `{"inbounds":[{"type":"tuic","listen":"::","listen_port":8443,"users":[{"name":"a"}]}]}`
	if _, ok, _ := PlanRosterUpdate(set(t, js(t, untagged), "inbounds.0.users", js(t, `[{"name":"b"}]`)), js(t, untagged)); ok {
		t.Error("untagged")
	}
	nonArray := `{"inbounds":[{"type":"tuic","tag":"t","users":[{"name":"a"}]}]}`
	if _, ok, _ := PlanRosterUpdate(set(t, js(t, nonArray), "inbounds.0.users", "x"), js(t, nonArray)); ok {
		t.Error("non-array users")
	}
	// Key order counts: JSON.stringify of {a,b} and {b,a} differ.
	if _, ok, _ := PlanRosterUpdate(js(t, `{"log":{},"inbounds":[]}`), js(t, `{"inbounds":[],"log":{}}`)); ok {
		t.Error("key order")
	}
}

const blockerCfg = `{"log":{"level":"warn"},"inbounds":[
 {"type":"vless","tag":"vless-in","listen":"::","users":[{"name":"a","uuid":"uuid-a"}]},
 {"type":"tuic","tag":"tuic-in","listen":"::","users":[{"name":"a","uuid":"uuid-a","password":"PRIVATE-KEY-DO-NOT-LOG"}]}],
 "outbounds":[{"type":"direct","tag":"direct"}]}`

func TestExplainHotSwapBlocker(t *testing.T) {
	cur := js(t, blockerCfg)
	next := set(t, js(t, blockerCfg), "inbounds.1.listen", "127.0.0.1")
	if b := ExplainHotSwapBlocker(next, cur); b != "разошлось вне наборов абонентов: inbounds[1].listen" {
		t.Fatal(b)
	}
	if b := ExplainHotSwapBlocker(js(t, `{"log":{"level":"warn"},"inbounds":[],"outbounds":[]}`), js(t, `{"inbounds":[],"log":{"level":"warn"},"outbounds":[]}`)); b != "разошлось вне наборов абонентов: (корень){порядок ключей}" {
		t.Fatal(b)
	}
	withSvc := set(t, js(t, blockerCfg), "services", js(t, `[{"type":"users-api"}]`))
	if b := ExplainHotSwapBlocker(withSvc, cur); b != "разошлось вне наборов абонентов: (корень){добавлено: services, убрано: —}" {
		t.Fatal(b)
	}
	secret := set(t, set(t, js(t, blockerCfg), "inbounds.1.users.0.password", "NEW-SECRET"), "outbounds.0.tag", "renamed")
	b := ExplainHotSwapBlocker(secret, cur)
	if strings.Contains(b, "PRIVATE-KEY") || strings.Contains(b, "NEW-SECRET") || strings.Contains(b, "renamed") || !strings.Contains(b, "outbounds[0].tag") {
		t.Fatalf("values must never be printed: %s", b)
	}
	if ExplainHotSwapBlocker(js(t, `{"log":{}}`), cur) != "в присланном конфиге нет inbounds" ||
		ExplainHotSwapBlocker(cur, js(t, `{"log":{}}`)) != "в конфиге на диске нет inbounds" {
		t.Fatal("missing inbounds")
	}
	if b := ExplainHotSwapBlocker(js(t, `{"inbounds":[{"type":"shadowsocks","tag":"ss-in","users":[{"name":"a"}]}]}`), js(t, `{"inbounds":[{"type":"shadowsocks","tag":"ss-in","users":[]}]}`)); b != "инбаунд 0 типа shadowsocks не умеет горячую замену" {
		t.Fatal(b)
	}
	if b := ExplainHotSwapBlocker(js(t, `{"inbounds":[{"tag":"x","users":[1]}]}`), js(t, `{"inbounds":[{"tag":"x","users":[]}]}`)); b != "инбаунд 0 типа undefined не умеет горячую замену" {
		t.Fatal(b)
	}
	if DescribeConfigDiff(js(t, blockerCfg), js(t, blockerCfg), "") != "" {
		t.Fatal("identical")
	}
	if d := DescribeConfigDiff(js(t, `{"a":[1,2]}`), js(t, `{"a":[1]}`), ""); d != "a[длина 1 → 2]" {
		t.Fatal(d)
	}
	if d := DescribeConfigDiff(js(t, `{"a":{"b":1}}`), js(t, `{"a":[1]}`), ""); d != "a" {
		t.Fatal(d)
	}
}
