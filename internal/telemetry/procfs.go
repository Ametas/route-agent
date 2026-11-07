package telemetry

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
)

// jsSpace is JavaScript's \s (WhiteSpace ∪ LineTerminator), for regexps ported from TS.
const jsSpace = `[\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]`

var jsSpaceRun = regexp.MustCompile(jsSpace + `+`)

// splitJS is s.split(/\s+/).
func splitJS(s string) []string { return jsSpaceRun.Split(s, -1) }

// CPUStats is the /proc/stat baseline of one stream (CpuStats).
type CPUStats struct {
	Idle  float64
	Total float64
}

// CPUUsage is getCpuUsage (telemetry.ts:16-33), kept number for number: total is the sum of
// EVERY column of the first /proc/stat line (guest and guest_nice are counted twice, the kernel
// already includes them in user/nice), idle is column 4 only (iowait counts as busy), and
// usage = (Δtotal − Δidle) / Δtotal × 100 against prev. A zero prev gives the average since
// boot — the first frame of every stream and every GetTelemetry. A read error gives 0 and keeps
// prev. (Spec E R7: these are the numbers the orchestrator has always shown; changing the formula
// would shift every node's history, so it is not "clearly a bug" and stays.)
func CPUUsage(statText string, readErr error, prev CPUStats) (float64, CPUStats) {
	if readErr != nil {
		return 0, prev
	}
	first, _, _ := strings.Cut(statText, "\n")
	cols := splitJS(first)
	total := 0.0
	idle := math.NaN() // times[3] of a short line is undefined → NaN
	for i, c := range cols[1:] {
		n := config.JSNumber(c)
		total += n
		if i == 3 {
			idle = n
		}
	}
	diffIdle := idle - prev.Idle
	diffTotal := total - prev.Total
	next := CPUStats{Idle: idle, Total: total}
	if diffTotal == 0 {
		return 0, next
	}
	return (diffTotal - diffIdle) / diffTotal * 100, next
}

var (
	reMemTotal = regexp.MustCompile(`MemTotal:` + jsSpace + `+(\d+)`)
	reMemAvail = regexp.MustCompile(`MemAvailable:` + jsSpace + `+(\d+)`)
)

// MemUsage is getMemoryUsage (telemetry.ts:38-51): (MemTotal − MemAvailable) / MemTotal × 100;
// 0 when either line is missing or the file is unreadable (MemTotal 0 gives NaN, as in TS).
func MemUsage(meminfo string, readErr error) float64 {
	if readErr != nil {
		return 0
	}
	t := reMemTotal.FindStringSubmatch(meminfo)
	a := reMemAvail.FindStringSubmatch(meminfo)
	if t == nil || a == nil {
		return 0
	}
	total, _ := strconv.ParseFloat(t[1], 64)
	avail, _ := strconv.ParseFloat(a[1], 64)
	return (total - avail) / total * 100
}

// lineCount is text.trim().split('\n').length − 1 (the header line excluded).
func lineCount(text string) int {
	return len(strings.Split(jsjson.Trim(text), "\n")) - 1
}

// ConnectionCount is getConnectionCount (telemetry.ts:56-66): lines of /proc/net/tcp plus lines
// of /proc/net/udp, headers excluded — IPv4 only, every TCP state, every UDP socket; any read
// error gives 0.
func ConnectionCount(fs fsx.FS) uint32 {
	tcp, err := fs.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0
	}
	udp, err := fs.ReadFile("/proc/net/udp")
	if err != nil {
		return 0
	}
	return uint32(max(0, lineCount(string(tcp))+lineCount(string(udp))))
}

// HostNetwork is the host traffic of the default-route interface (hostNetwork.ts).
type HostNetwork struct {
	Iface   string
	RxBytes uint64
	TxBytes uint64
}

// parseIntHex is parseInt(s, 16): leading hex digits after optional whitespace and sign.
func parseIntHex(s string) (int64, bool) {
	s = strings.TrimLeftFunc(s, jsjson.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	end := 0
	for end < len(s) && strings.IndexByte("0123456789abcdefABCDEF", s[end]) >= 0 {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(s[:end], 16, 64)
	if err != nil {
		n = math.MaxInt64
	}
	v := int64(min(n, math.MaxInt64))
	if neg {
		v = -v
	}
	return v, true
}

// DefaultRouteInterface is defaultRouteInterface (hostNetwork.ts:16-27): the /proc/net/route
// row with destination 00000000 and the RTF_UP flag with the lowest metric, ties keep the first.
// IPv4 only. "" when there is none.
func DefaultRouteInterface(routeText string) string {
	best, bestMetric, found := "", int64(0), false
	lines := strings.Split(routeText, "\n")
	for _, line := range lines[1:] {
		cols := splitJS(jsjson.Trim(line))
		if len(cols) < 8 || cols[1] != "00000000" {
			continue
		}
		flags, ok := parseIntHex(cols[3])
		if !ok || flags&0x1 == 0 {
			continue
		}
		metric, ok := parseIntJS10(cols[6])
		if !ok {
			metric = 0
		}
		if !found || metric < bestMetric {
			best, bestMetric, found = cols[0], metric, true
		}
	}
	return best
}

// parseIntJS10 is parseInt(s, 10).
func parseIntJS10(s string) (int64, bool) {
	s = strings.TrimLeftFunc(s, jsjson.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:end], 10, 64)
	if err != nil {
		n = math.MaxInt64
	}
	if neg {
		n = -n
	}
	return n, true
}

// counter is Number(col) as a uint64 counter: exact for plain decimal text, otherwise the JS
// number truncated as protobufjs would encode it; ok is false for a non-finite value.
func counter(col string) (uint64, bool) {
	if n, err := strconv.ParseUint(col, 10, 64); err == nil {
		return n, true
	}
	f := config.JSNumber(col)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return uint64(jsjson.ToInt64(f)), true
}

// InterfaceCounters is interfaceCounters (hostNetwork.ts:30-40): the first /proc/net/dev line
// whose name (text before the first ':', trimmed) is iface; rx is column 0, tx column 8 of the
// rest; both must be finite numbers.
func InterfaceCounters(devText, iface string) (rx, tx uint64, ok bool) {
	for _, line := range strings.Split(devText, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found || jsjson.Trim(name) != iface {
			continue
		}
		cols := splitJS(jsjson.Trim(rest))
		if len(cols) < 9 {
			return 0, 0, false
		}
		r, ok1 := counter(cols[0])
		t, ok2 := counter(cols[8])
		if !ok1 || !ok2 {
			return 0, 0, false
		}
		return r, t, true
	}
	return 0, 0, false
}

// ReadHostNetwork is getHostNetwork: nil when /proc is unreadable, there is no default route or
// its interface is not in /proc/net/dev (fields then "", 0, 0: "no data" at the orchestrator).
func ReadHostNetwork(fs fsx.FS) *HostNetwork {
	route, err := fs.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	dev, err := fs.ReadFile("/proc/net/dev")
	if err != nil {
		return nil
	}
	iface := DefaultRouteInterface(string(route))
	if iface == "" {
		return nil
	}
	rx, tx, ok := InterfaceCounters(string(dev), iface)
	if !ok {
		return nil
	}
	return &HostNetwork{Iface: iface, RxBytes: rx, TxBytes: tx}
}
