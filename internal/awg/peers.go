package awg

import (
	"context"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// HandshakeFreshSec: a peer counts as active/live when its last handshake is at most this old
// (telemetry.ts:184, MESH_HANDSHAKE_FRESH_SEC :397). Inclusive.
const HandshakeFreshSec = 180

// LivePeers is the shared dump scan of getAwgActivePeersCount and parseMeshPeerDump
// (telemetry.ts:167-200, 418-430): skip the interface line, rows with ≥5 columns, live when the
// handshake column parses to h > 0 and nowSec−h ≤ 180. Keys in dump order.
func LivePeers(stdout string, nowSec int64) []string {
	keys := []string{}
	lines := strings.Split(jsjson.Trim(stdout), "\n")
	for _, line := range lines[1:] {
		parts := Fields(line)
		if len(parts) < 5 {
			continue
		}
		if hs, ok := ParseIntJS(parts[4]); ok && hs > 0 && nowSec-hs <= HandshakeFreshSec {
			keys = append(keys, parts[0])
		}
	}
	return keys
}

// PeerStat is one peer row of `awg show <iface> dump` — the MeshTunnelPeerStat telemetry field
// (2026-11-14): handshake (unix seconds, 0 — never), transfer counters and endpoint ("(none)" → "").
type PeerStat struct {
	PublicKey          string
	LatestHandshakeSec int64
	RxBytes            uint64
	TxBytes            uint64
	Endpoint           string
}

// PeerStats parses every peer row of a dump (rows shorter than 5 fields are skipped, as in
// LivePeers); a field that does not parse as a positive integer reads as 0.
func PeerStats(stdout string) []PeerStat {
	stats := []PeerStat{}
	lines := strings.Split(jsjson.Trim(stdout), "\n")
	count := func(parts []string, i int) int64 {
		if i >= len(parts) {
			return 0
		}
		if n, ok := ParseIntJS(parts[i]); ok && n > 0 {
			return n
		}
		return 0
	}
	for _, line := range lines[1:] {
		parts := Fields(line)
		if len(parts) < 5 {
			continue
		}
		endpoint := parts[2]
		if endpoint == "(none)" {
			endpoint = ""
		}
		stats = append(stats, PeerStat{
			PublicKey:          parts[0],
			LatestHandshakeSec: count(parts, 4),
			RxBytes:            uint64(count(parts, 5)),
			TxBytes:            uint64(count(parts, 6)),
			Endpoint:           endpoint,
		})
	}
	return stats
}

// ActivePeers is the awg_active_peers telemetry reader: `awg show awg0 dump` (interface name
// hard-coded as in TS, spec D R5; `awg` from PATH), live peers counted; any failure is 0. It runs
// one bounded command and never heals anything, so telemetry may call it every tick.
func ActivePeers(ctx context.Context, r runner.Runner, now time.Time) uint32 {
	res, err := r.Run(ctx, runner.Cmd{Name: "awg", Args: []string{"show", NATIface, "dump"}, Timeout: ShowTimeout})
	if err != nil {
		return 0
	}
	return uint32(len(LivePeers(res.Stdout, now.Unix())))
}
