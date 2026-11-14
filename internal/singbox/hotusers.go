package singbox

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Ametas/route-agent/internal/jsjson"
)

// UsersAPISocketPath is the fork's users-api unix socket (no TCP port on purpose).
const UsersAPISocketPath = "/run/sing-box/users.sock"

// hotSwappable are the inbound types the fork can re-roster live. The list is closed: another
// type would get a 404 only AFTER part of the plan was applied (hotUsers.ts:40-47).
//
// naive — fork 1.14.2-2+: its login lives in `username`, which users-api accepts as an alias for
// `name`, so the roster goes out as it is in the config. An older fork answers 404 and the apply
// falls back to a reload; the orchestrator sends naive only to nodes on the new fork.
var hotSwappable = map[string]bool{"vless": true, "tuic": true, "hysteria2": true, "naive": true}

// usersAPIService is {type:'users-api', tag:'users', path:<socket>} with that key order.
func usersAPIService() *jsjson.Object {
	o := jsjson.NewObject()
	o.Set("type", "users-api")
	o.Set("tag", "users")
	o.Set("path", UsersAPISocketPath)
	return o
}

// typeError reproduces the TypeError TS hit on a property read of null.
func typeError(prop string) error {
	return errors.New("Cannot read properties of null (reading '" + prop + "')")
}

func field(v any, k string) (any, bool) {
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil, false
	}
	return o.Get(k)
}

// WithUsersAPIService is withUsersApiService (hotUsers.ts:77-86): append the users-api service
// to `services` (created as the LAST key when absent, kept in place when present), unless one of
// type users-api is already there (idempotent: it runs on every push). It is applied before the
// on-disk comparison, which would otherwise never match.
func WithUsersAPIService(cfg any) (any, error) {
	if cfg == nil {
		return nil, typeError("services")
	}
	var services []any
	if v, ok := field(cfg, "services"); ok {
		if arr, ok := v.([]any); ok {
			services = arr
		}
	}
	for _, s := range services {
		if s == nil {
			return nil, typeError("type")
		}
		if t, _ := field(s, "type"); t == "users-api" {
			return cfg, nil
		}
	}
	out := jsjson.Spread(cfg)
	next := append(append([]any{}, services...), usersAPIService())
	out.Set("services", next)
	return out, nil
}

// RosterUpdate is one PUT /inbounds/<tag>/users with the FULL roster.
type RosterUpdate struct {
	Tag   string
	Users []any
}

func inboundsOf(cfg any) ([]any, bool) {
	v, ok := field(cfg, "inbounds")
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	return arr, ok
}

func stripUsers(cfg any, inbounds []any) *jsjson.Object {
	stripped := make([]any, len(inbounds))
	for i, in := range inbounds {
		c := jsjson.Spread(in)
		c.Delete("users")
		stripped[i] = c
	}
	o := jsjson.Spread(cfg)
	o.Set("inbounds", stripped)
	return o
}

// usersJSON is JSON.stringify(inbound.users ?? null); a null inbound is the TS TypeError.
func usersJSON(inbound any) (string, error) {
	if inbound == nil {
		return "", typeError("users")
	}
	u, _ := field(inbound, "users")
	return jsjson.Stringify(u, ""), nil
}

// PlanRosterUpdate is planRosterUpdate (hotUsers.ts:207-233). ok=false means "not a pure roster
// change, reload instead": no inbounds on either side, any difference outside `users` (compact,
// order-sensitive JSON.stringify of both configs without users), or a changed inbound that is
// not a hot-swappable type, has no tag or has non-array users. An empty plan with ok=true means
// no roster differs. err is only the TS TypeError on a null inbound.
func PlanRosterUpdate(next, current any) (plan []RosterUpdate, ok bool, err error) {
	ni, ok1 := inboundsOf(next)
	ci, ok2 := inboundsOf(current)
	if !ok1 || !ok2 {
		return nil, false, nil
	}
	if jsjson.Stringify(stripUsers(next, ni), "") != jsjson.Stringify(stripUsers(current, ci), "") {
		return nil, false, nil
	}
	plan = []RosterUpdate{}
	for i, in := range ni {
		a, err := usersJSON(in)
		if err != nil {
			return nil, false, err
		}
		b, err := usersJSON(ci[i])
		if err != nil {
			return nil, false, err
		}
		if a == b {
			continue
		}
		typ, _ := field(in, "type")
		if t, isStr := typ.(string); !isStr || !hotSwappable[t] {
			return nil, false, nil
		}
		tag, _ := field(in, "tag")
		t, isStr := tag.(string)
		if !isStr || t == "" {
			return nil, false, nil
		}
		users, _ := field(in, "users")
		arr, isArr := users.([]any)
		if !isArr {
			return nil, false, nil
		}
		plan = append(plan, RosterUpdate{Tag: t, Users: arr})
	}
	return plan, true, nil
}

const rootPath = "(корень)"

func orRoot(p string) string {
	if p == "" {
		return rootPath
	}
	return p
}

func isContainer(v any) bool {
	switch v.(type) {
	case *jsjson.Object, []any:
		return true
	}
	return false
}

// DescribeConfigDiff is describeConfigDiff (hotUsers.ts:124-169): the PATH of the first
// difference ("" when none) — never a value: configs hold reality keys and passwords.
func DescribeConfigDiff(next, current any, p string) string {
	if !isContainer(next) && !isContainer(current) && next == current {
		return ""
	}
	if !isContainer(next) || !isContainer(current) {
		return orRoot(p)
	}
	na, nIsArr := next.([]any)
	ca, cIsArr := current.([]any)
	if nIsArr != cIsArr {
		return orRoot(p)
	}
	if nIsArr {
		if len(na) != len(ca) {
			return p + "[длина " + strconv.Itoa(len(ca)) + " → " + strconv.Itoa(len(na)) + "]"
		}
		for i := range na {
			if d := DescribeConfigDiff(na[i], ca[i], p+"["+strconv.Itoa(i)+"]"); d != "" {
				return d
			}
		}
		return ""
	}
	no, co := next.(*jsjson.Object), current.(*jsjson.Object)
	nk, ck := no.Keys(), co.Keys()
	if strings.Join(nk, ",") != strings.Join(ck, ",") {
		added := missingFrom(nk, ck)
		removed := missingFrom(ck, nk)
		if len(added) > 0 || len(removed) > 0 {
			return orRoot(p) + "{добавлено: " + joinOrDash(added) + ", убрано: " + joinOrDash(removed) + "}"
		}
		return orRoot(p) + "{порядок ключей}"
	}
	for _, k := range nk {
		nv, _ := no.Get(k)
		cv, _ := co.Get(k)
		sub := k
		if p != "" {
			sub = p + "." + k
		}
		if d := DescribeConfigDiff(nv, cv, sub); d != "" {
			return d
		}
	}
	return ""
}

func missingFrom(keys, other []string) []string {
	set := make(map[string]bool, len(other))
	for _, k := range other {
		set[k] = true
	}
	var out []string
	for _, k := range keys {
		if !set[k] {
			out = append(out, k)
		}
	}
	return out
}

func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	return strings.Join(xs, "|")
}

// ExplainHotSwapBlocker is explainHotSwapBlocker (hotUsers.ts:171-198): why PlanRosterUpdate
// refused, for the log line — paths and types only, never values.
func ExplainHotSwapBlocker(next, current any) string {
	ni, ok := inboundsOf(next)
	if !ok {
		return "в присланном конфиге нет inbounds"
	}
	ci, ok := inboundsOf(current)
	if !ok {
		return "в конфиге на диске нет inbounds"
	}
	if d := DescribeConfigDiff(stripUsers(next, ni), stripUsers(current, ci), ""); d != "" {
		return "разошлось вне наборов абонентов: " + d
	}
	for i, in := range ni {
		if in == nil {
			continue
		}
		var cur any
		if i < len(ci) {
			cur = ci[i]
		}
		a, _ := usersJSON(in)
		cu, _ := field(cur, "users")
		if a == jsjson.Stringify(cu, "") {
			continue
		}
		typ, has := field(in, "type")
		if t, isStr := typ.(string); !isStr || !hotSwappable[t] {
			name := "undefined"
			if has {
				name = jsjson.String(typ)
			}
			return "инбаунд " + strconv.Itoa(i) + " типа " + name + " не умеет горячую замену"
		}
		if tag, _ := field(in, "tag"); tag == nil || tag == "" {
			return "у инбаунда " + strconv.Itoa(i) + " нет тега"
		} else if _, isStr := tag.(string); !isStr {
			return "у инбаунда " + strconv.Itoa(i) + " нет тега"
		}
		if users, _ := field(in, "users"); users == nil {
			return "у инбаунда " + strconv.Itoa(i) + " набор абонентов не массив"
		} else if _, isArr := users.([]any); !isArr {
			return "у инбаунда " + strconv.Itoa(i) + " набор абонентов не массив"
		}
	}
	return "причина не определена"
}
