package rear

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// --- transport ------------------------------------------------------------------------------

// clash is the shared Clash API client (internal/clashapi) with the rear's bounds: timeout per
// call (5 s, 3 s for the rule-provider probe), 64 MiB body cap, the agent's HTTP client.
// Transport error texts are undici's (clashapi), so log and response lines read as before; a
// non-2xx answer is a *clashapi.StatusError that each caller words itself.
func (s *Service) clash(timeout time.Duration, alwaysBearer bool) clashapi.Client {
	return clashapi.Client{HTTP: s.httpClient(), Timeout: timeout, MaxBody: maxClashBody, AlwaysBearer: alwaysBearer}
}

// --- GetRearConnections ---------------------------------------------------------------------

const msgRearNotActive = "Rear core is not active on this node."

// GetRearConnections implements the RPC (spec F §4.1): auth M|B; a snapshot of the rear's live
// connections. "Not active" is also the message for an active rear with zero connections (R17).
func (s *Service) GetRearConnections(ctx context.Context, req *agentpb.SingBoxConnectionsRequest) (*agentpb.SingBoxConnectionsResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetRearConnections request blocked")
		return &agentpb.SingBoxConnectionsResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	records, err := s.collectConnections(ctx)
	if err != nil {
		s.Log.Warn("Failed to fetch rear connections", "err", err.Error())
		return &agentpb.SingBoxConnectionsResponse{Success: false, Message: err.Error()}, nil
	}
	msg := "OK"
	if len(records) == 0 {
		msg = msgRearNotActive
	}
	return &agentpb.SingBoxConnectionsResponse{Success: true, Message: msg, Connections: clashapi.ToProto(records)}, nil
}

func (s *Service) collectConnections(ctx context.Context) ([]clashapi.Record, error) {
	api := s.ActiveClashAPI()
	if api == nil {
		s.Log.Debug("Rear core is not active on this node, no connections to report")
		return nil, nil
	}
	payload, err := s.clash(clashTimeout, false).Get(ctx, api.Endpoint, "/connections")
	if err != nil {
		if code, ok := clashapi.StatusCode(err); ok {
			return nil, errors.New("rear clash api responded " + strconv.Itoa(code))
		}
		return nil, err
	}
	return ParseMihomoConnections(payload), nil
}

// ParseMihomoConnections is parseMihomoConnections (rearConnections.ts:69-96): mihomo's shape
// (inboundName/inboundUser, host over sniffHost, string port); a bad field zeroes only itself.
func ParseMihomoConnections(payload any) []clashapi.Record {
	list, ok := get(payload, "connections").([]any)
	if !ok {
		return []clashapi.Record{}
	}
	out := make([]clashapi.Record, 0, len(list))
	for _, raw := range list {
		meta := get(raw, "metadata")
		domain := asString(get(meta, "host"))
		if domain == "" {
			domain = asString(get(meta, "sniffHost"))
		}
		out = append(out, clashapi.Record{
			ID:                asString(get(raw, "id")),
			User:              asString(get(meta, "inboundUser")),
			SourceIP:          asString(get(meta, "sourceIP")),
			DestinationIP:     asString(get(meta, "destinationIP")),
			DestinationDomain: domain,
			DestinationPort:   asNumber(get(meta, "destinationPort")),
			UploadBytes:       asNumber(get(raw, "upload")),
			DownloadBytes:     asNumber(get(raw, "download")),
			Network:           asString(get(meta, "network")),
			InboundTag:        asString(get(meta, "inboundName")),
			StartedAtUnixMs:   parseTime(get(raw, "start")),
			Closed:            false,
		})
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// asNumber: a finite number, or a string through JS Number(); anything else 0.
func asNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		if finite(t) {
			return t
		}
	case string:
		if n := config.JSNumber(t); finite(n) {
			return n
		}
	}
	return 0
}

// parseTime is Date.parse of a non-empty string (ISO-8601, as both cores emit), else 0.
func parseTime(v any) float64 {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	ms, ok := clashapi.DateParse(s)
	if !ok {
		return 0
	}
	return ms
}

// --- WARP key health (GetWarpKeyHealth, group E; the guard) ---------------------------------

// WarpKeyHealth is one WARP endpoint as measured by the rear core.
type WarpKeyHealth struct {
	EndpointTag      string
	Alive            bool
	RttMs            float64
	MeasuredAtUnixMs float64
}

// Proto converts the way protobufjs did (uint32 by ToUint32, int64 by truncation).
func (w WarpKeyHealth) Proto() *agentpb.WarpKeyHealthEntry {
	return &agentpb.WarpKeyHealthEntry{
		EndpointTag: w.EndpointTag, Alive: w.Alive,
		RttMs: jsjson.ToUint32(w.RttMs), MeasuredAtUnixMs: jsjson.ToInt64(w.MeasuredAtUnixMs),
	}
}

const warpTagPrefix = "warp-"

// warpGroupTags are groups whose tag looks like a key's (mihomo's fallback group).
var warpGroupTags = map[string]bool{"warp-auto": true}

// ParseWarpKeyHealth is parseWarpKeyHealth (warpKeyHealth.ts:63-83): every proxy tagged
// `warp-<id>` (not a group), in key order; alive iff the LAST history entry has a numeric
// delay > 0.
func ParseWarpKeyHealth(payload any) []WarpKeyHealth {
	proxies, ok := get(payload, "proxies").(*jsjson.Object)
	if !ok {
		return []WarpKeyHealth{}
	}
	out := []WarpKeyHealth{}
	for _, tag := range proxies.Keys() {
		if !strings.HasPrefix(tag, warpTagPrefix) || len(tag) == len(warpTagPrefix) || warpGroupTags[tag] {
			continue
		}
		entry, _ := proxies.Get(tag)
		rec := WarpKeyHealth{EndpointTag: tag}
		if hist, ok := get(entry, "history").([]any); ok && len(hist) > 0 {
			last := hist[len(hist)-1]
			if d, ok := get(last, "delay").(float64); ok && finite(d) && d > 0 {
				rec.Alive, rec.RttMs = true, d
				if t, ok := get(last, "time").(string); ok {
					if ms, ok := clashapi.DateParse(t); ok {
						rec.MeasuredAtUnixMs = ms
					}
				}
			}
		}
		out = append(out, rec)
	}
	return out
}

// fetchProxies is GET /proxies, bearer only with a secret, 5 s.
func (s *Service) fetchProxies(ctx context.Context, ep clashapi.Endpoint) (any, error) {
	// The non-2xx text is clashapi's own: "clash api responded <code> for /proxies".
	return s.clash(clashTimeout, false).Get(ctx, ep, "/proxies")
}

// FetchWarpKeyHealth is fetchWarpKeyHealth: GET /proxies of ep and ParseWarpKeyHealth.
func (s *Service) FetchWarpKeyHealth(ctx context.Context, ep clashapi.Endpoint) ([]WarpKeyHealth, error) {
	payload, err := s.fetchProxies(ctx, ep)
	if err != nil {
		return nil, err
	}
	recs := ParseWarpKeyHealth(payload)
	s.Log.Debug("Read WARP key health from the rear clash api", "count", len(recs))
	return recs, nil
}

// Strings of GetWarpKeyHealth the orchestrator reads (skipped reasons) or logs.
const (
	SkippedRearNotRunning      = "rear_not_running"
	SkippedClashAPIUnreachable = "clash_api_unreachable"
	msgRearNotConfigured       = "Rear sing-box is not configured on this node."
	msgNoClashEndpoint         = "Rear sing-box config carries no clash_api endpoint."
)

// WarpKeyHealthReport is the body of GetWarpKeyHealth after authentication (group E owns the RPC
// and its auth answer). Unlike TS it is core-aware: TS pre-checked the SING-BOX rear config path,
// so on a mihomo rear it always answered rear_not_running and dead keys were never retired
// (spec F R2, architecture §6 item 7). The texts stay as they were.
func (s *Service) WarpKeyHealthReport(ctx context.Context) *agentpb.WarpKeyHealthResponse {
	if s.ActiveCore() == nil {
		return &agentpb.WarpKeyHealthResponse{Success: true, Message: msgRearNotConfigured, SkippedReason: SkippedRearNotRunning}
	}
	api := s.ActiveClashAPI()
	if api == nil {
		return &agentpb.WarpKeyHealthResponse{Success: true, Message: msgNoClashEndpoint, SkippedReason: SkippedRearNotRunning}
	}
	recs, err := s.FetchWarpKeyHealth(ctx, api.Endpoint)
	if err != nil {
		s.Log.Warn("Failed to read WARP key health from the rear", "err", err.Error(), "core", string(api.Core.ID))
		return &agentpb.WarpKeyHealthResponse{
			Success: false, Message: "Could not read rear clash api: " + err.Error(), SkippedReason: SkippedClashAPIUnreachable,
		}
	}
	entries := make([]*agentpb.WarpKeyHealthEntry, 0, len(recs))
	for _, r := range recs {
		entries = append(entries, r.Proto())
	}
	return &agentpb.WarpKeyHealthResponse{
		Success: true, Message: "Reported health for " + strconv.Itoa(len(entries)) + " WARP endpoint(s).", Entries: entries,
	}
}

// --- empty rule providers (PullNodeWarnings, group E) ---------------------------------------

// RuleProviderEmptyKind is the warning kind the orchestrator dedups on.
const RuleProviderEmptyKind = "rear_rule_provider_empty"

// EmptyRuleProviders is emptyRuleProviders (rearRuleProviderWarning.ts:49-65): providers whose
// numeric ruleCount is <= 0 (a string "0" or null is not understood and not counted), by name,
// falling back to the key, sorted in UTF-16 code-unit order (JS default sort).
func EmptyRuleProviders(payload any) []string {
	providers := get(payload, "providers")
	var keys []string
	var vals []any
	switch p := providers.(type) {
	case *jsjson.Object:
		for _, k := range p.Keys() {
			v, _ := p.Get(k)
			keys, vals = append(keys, k), append(vals, v)
		}
	case []any: // Object.entries of an array: index keys
		for i, v := range p {
			keys, vals = append(keys, strconv.Itoa(i)), append(vals, v)
		}
	}
	empty := []string{}
	for i, raw := range vals {
		var count any
		var name any
		switch r := raw.(type) {
		case *jsjson.Object:
			count, _ = r.Get("ruleCount")
			name, _ = r.Get("name")
		case []any: // typeof [] === 'object', but it has neither field
		default:
			continue
		}
		n, ok := count.(float64)
		if !ok || n > 0 {
			continue
		}
		if s, ok := name.(string); ok && s != "" {
			empty = append(empty, s)
		} else {
			empty = append(empty, keys[i])
		}
	}
	sort.SliceStable(empty, func(a, b int) bool { return utf16Less(empty[a], empty[b]) })
	return empty
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// DetectEmptyRuleProviders is detectEmptyRearRuleProviders, the rear part of PullNodeWarnings:
// nil when there is no rear, the probe fails in any way (silently, 3 s bound), or every provider
// has rules. `Authorization: Bearer <secret>` is sent even with an empty secret, as in TS.
func (s *Service) DetectEmptyRuleProviders(ctx context.Context) *agentpb.NodeWarning {
	api := s.ActiveClashAPI()
	if api == nil {
		return nil
	}
	payload, err := s.clash(ruleProvidersTimeout, true).Get(ctx, api.Endpoint, "/providers/rules")
	if err != nil {
		return nil
	}
	empty := EmptyRuleProviders(payload)
	if len(empty) == 0 {
		return nil
	}
	s.Log.Warn("Rear rule providers loaded empty — their rules match nothing", "providers", empty)
	return &agentpb.NodeWarning{
		Kind:           RuleProviderEmptyKind,
		Source:         api.Core.Label,
		Sample:         "наборы без правил: " + strings.Join(empty, ", "),
		Occurrences:    uint32(len(empty)),
		LastSeenUnixMs: s.now().UnixMilli(),
	}
}
