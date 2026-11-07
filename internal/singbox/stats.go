package singbox

import (
	"context"
	"math"
	"sync"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// MaxTrackedConnections caps the per-connection baseline (singboxStats.ts:40).
const MaxTrackedConnections = 100_000

const msgNoClashTraffic = "clash_api block absent from the applied sing-box config"

// Totals is a connection's last seen byte counters.
type Totals struct{ Upload, Download float64 }

// UserDelta is one user's increment since the previous pass.
type UserDelta struct {
	UserUUID string
	Uplink   float64
	Downlink float64
}

// ComputeUserTrafficDeltas is computeUserTrafficDeltas (singboxStats.ts:59-103), pure: records
// with an empty id or user are neither counted nor remembered; up to MaxTrackedConnections ids
// are remembered for the next pass; the first pass only records the baseline; increments are
// never negative; output is per user in first-seen order; ids absent from this pass are forgotten.
func ComputeUserTrafficDeltas(records []clashapi.Record, previous map[string]Totals, firstPass bool) ([]UserDelta, map[string]Totals) {
	next := map[string]Totals{}
	idx := map[string]int{}
	deltas := []UserDelta{}
	for _, r := range records {
		if r.ID == "" || r.User == "" {
			continue
		}
		if len(next) < MaxTrackedConnections {
			next[r.ID] = Totals{Upload: r.UploadBytes, Download: r.DownloadBytes}
		}
		if firstPass {
			continue
		}
		prev := previous[r.ID]
		up := math.Max(0, r.UploadBytes-prev.Upload)
		down := math.Max(0, r.DownloadBytes-prev.Download)
		if up == 0 && down == 0 {
			continue
		}
		i, ok := idx[r.User]
		if !ok {
			i = len(deltas)
			idx[r.User] = i
			deltas = append(deltas, UserDelta{UserUUID: r.User})
		}
		deltas[i].Uplink += up
		deltas[i].Downlink += down
	}
	return deltas, next
}

// trafficState is the process-lifetime baseline. The mutex serializes whole passes: two
// concurrent calls no longer both diff against the same old baseline and double-report
// (spec B R9).
type trafficState struct {
	mu     sync.Mutex
	last   map[string]Totals
	primed bool
}

// queryUserTrafficDeltas: nil, nil when the applied config has no clash_api (state untouched);
// a fetch error also leaves the state untouched.
func (s *Service) queryUserTrafficDeltas(ctx context.Context) ([]UserDelta, error) {
	s.traffic.mu.Lock()
	defer s.traffic.mu.Unlock()
	ep, err := clashapi.ReadEndpoint(s.FS, s.configPath())
	if err != nil {
		s.Log.Debug("Could not read live sing-box config to resolve the clash_api endpoint", "err", err.Error())
	}
	if ep == nil {
		return nil, nil
	}
	records, err := s.Clash.Fetch(ctx, *ep)
	if err != nil {
		return nil, err
	}
	deltas, next := ComputeUserTrafficDeltas(records, s.traffic.last, !s.traffic.primed)
	s.traffic.last = next
	s.traffic.primed = true
	return deltas, nil
}

// GetSingBoxUserTraffic: auth M|B; "no clash_api" is success with a specific message (a
// fleet-wide regression must stay visible); a fetch failure is success:false with its text.
func (s *Service) GetSingBoxUserTraffic(ctx context.Context, req *agentpb.SingBoxUserTrafficRequest) (*agentpb.SingBoxUserTrafficResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetSingBoxUserTraffic request blocked")
		return &agentpb.SingBoxUserTrafficResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	deltas, err := s.queryUserTrafficDeltas(ctx)
	if err != nil {
		s.Log.Warn("Failed to fetch sing-box per-user traffic stats", "err", err.Error())
		return &agentpb.SingBoxUserTrafficResponse{Success: false, Message: err.Error()}, nil
	}
	if deltas == nil {
		return &agentpb.SingBoxUserTrafficResponse{Success: true, Message: msgNoClashTraffic}, nil
	}
	entries := make([]*agentpb.SingBoxUserTrafficEntry, 0, len(deltas))
	for _, d := range deltas {
		entries = append(entries, &agentpb.SingBoxUserTrafficEntry{
			UserUuid:      d.UserUUID,
			UplinkBytes:   jsjson.ToInt64(d.Uplink),
			DownlinkBytes: jsjson.ToInt64(d.Downlink),
		})
	}
	return &agentpb.SingBoxUserTrafficResponse{Success: true, Message: "OK", Entries: entries}, nil
}
