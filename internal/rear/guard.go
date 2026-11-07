package rear

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/lifecycle"
)

// WarpSelectorTag is the selector the orchestrator generates in both cores.
const WarpSelectorTag = "warp"

// GuardDecision is what the `warp` selector should point at.
type GuardDecision struct {
	Desired   string
	AliveKeys int
	TotalKeys int
}

// DecideWarpSelection is decideWarpSelection (rearWarpGuard.ts:50-67): nil when there is no
// `warp` selector (WARP not enabled — "no keys → direct" would be wrong there); otherwise the pool
// member while at least one key is alive, else direct.
func DecideWarpSelection(payload any, m WarpMembers) *GuardDecision {
	proxies, ok := get(payload, "proxies").(*jsjson.Object)
	if !ok || !proxies.Has(WarpSelectorTag) {
		return nil
	}
	keys := ParseWarpKeyHealth(payload)
	alive := 0
	for _, k := range keys {
		if k.Alive {
			alive++
		}
	}
	d := &GuardDecision{Desired: m.Direct, AliveKeys: alive, TotalKeys: len(keys)}
	if alive > 0 {
		d.Desired = m.Pool
	}
	return d
}

// CurrentSelection is currentSelection: proxies.warp.now when it is a string.
func CurrentSelection(payload any) (string, bool) {
	now, ok := get(get(get(payload, "proxies"), WarpSelectorTag), "now").(string)
	return now, ok
}

// selectWarpMember is `PUT /proxies/warp {"name":"<member>"}` (204 on success), 5 s.
func (s *Service) selectWarpMember(ctx context.Context, ep clashapi.Endpoint, member string) error {
	body := jsjson.NewObject()
	body.Set("name", member)
	err := s.clash(clashTimeout, false).Put(ctx, ep, "/proxies/"+WarpSelectorTag, []byte(jsjson.Stringify(body, "")))
	if err != nil {
		if code, ok := clashapi.StatusCode(err); ok {
			return errors.New("clash api responded " + strconv.Itoa(code) + " selecting " + member)
		}
		return err
	}
	return nil
}

// GuardOnce is guardOnce (rearWarpGuard.service.ts:30-65): it returns the member it switched to,
// or "" when nothing was done. It never edits the config: a reload resets the selector to its
// default and the next pass re-evaluates.
func (s *Service) GuardOnce(ctx context.Context) (string, error) {
	api := s.ActiveClashAPI()
	if api == nil {
		return "", nil
	}
	payload, err := s.fetchProxies(ctx, api.Endpoint)
	if err != nil {
		return "", err
	}
	members := api.Core.WarpMembers
	d := DecideWarpSelection(payload, members)
	if d == nil {
		return "", nil
	}
	if cur, ok := CurrentSelection(payload); ok && cur == d.Desired {
		return "", nil
	}
	if err := s.selectWarpMember(ctx, api.Endpoint, d.Desired); err != nil {
		return "", err
	}
	// TS compared with the literal "direct", so mihomo's "DIRECT" fallback was logged as
	// "restored" (spec F R6); compare with the core's own member.
	msg := "Rear WARP branch restored: live keys are back"
	if d.Desired == members.Direct {
		msg = "Rear WARP branch fell back to direct: no live keys left"
	}
	s.Log.Warn(msg, "selector", WarpSelectorTag, "to", d.Desired, "aliveKeys", d.AliveKeys, "totalKeys", d.TotalKeys)
	return d.Desired, nil
}

// GuardLoop is startRearWarpGuard: a pass every REAR_WARP_GUARD_INTERVAL_MS, the first one an
// interval after start (no immediate tick), passes never overlap (lifecycle.Loop); a failed pass
// is only a debug line (the rear may be restarting).
func (s *Service) GuardLoop() lifecycle.Loop {
	interval := time.Duration(s.Cfg.RearWarpGuardIntervalMs * float64(time.Millisecond))
	return lifecycle.Loop{
		Name:     "rear-warp-guard",
		Interval: interval,
		Tick: func(ctx context.Context) {
			if _, err := s.GuardOnce(ctx); err != nil {
				s.Log.Debug("Rear WARP guard pass failed", "err", err.Error())
			}
		},
	}
}
