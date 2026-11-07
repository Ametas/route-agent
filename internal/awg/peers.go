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
