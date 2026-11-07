package warnings

import (
	"context"
	"errors"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// Pattern is one warning class of the registry: Regex is used verbatim for journalctl --grep
// and, case-insensitively, for classification.
type Pattern struct {
	Kind  string
	Regex string
}

// Patterns is WARNING_PATTERNS (journalWarnings.ts:39-62). First match wins, so order matters;
// the kinds are a public contract (the orchestrator dedups and titles by them).
var Patterns = []Pattern{
	{"quic_recv_buffer", "failed to sufficiently increase receive buffer size"},
	{"quic_send_buffer", "failed to sufficiently increase send buffer size"},
	{"quic_buffer_not_increased", "failed to increase (?:receive|send) buffer size"},
	{"conntrack_table_full", "nf_conntrack: table full"},
	{"too_many_open_files", "too many open files"},
	{"out_of_socket_memory", "Out of socket memory"},
	{"neighbour_table_overflow", "neighbour table overflow"},
}

var classifiers = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(Patterns))
	for i, p := range Patterns {
		out[i] = regexp.MustCompile("(?i)" + p.Regex)
	}
	return out
}()

const (
	// SampleLimit caps a sample at 300 JavaScript characters (UTF-16 units).
	SampleLimit = 300
	// LineLimit is both --lines and the number of output lines parsed per pass.
	LineLimit = 2000
	// PassTimeout bounds each journalctl pass: two passes fit the orchestrator's 60 s deadline
	// with room to spare (journalWarnings.ts:78-93: a 65 s scan once outlived it). TS used 20 s
	// and SIGKILL; the runner sends SIGTERM and SIGKILL only runner.DefaultKillGrace (5 s) later,
	// so 18 s keeps the worst case of both passes (2 × 23 s) under 80 % of the deadline, the
	// margin tests/journalWarnings.test.ts holds.
	PassTimeout = 18 * time.Second
)

// GrepPattern is buildGrepPattern: every regex wrapped in (?:…), joined by |, in registry order.
func GrepPattern() string {
	parts := make([]string, len(Patterns))
	for i, p := range Patterns {
		parts[i] = "(?:" + p.Regex + ")"
	}
	return strings.Join(parts, "|")
}

// Classify is classifyLine: the kind of the first matching pattern, "" when none.
func Classify(message string) string {
	for i, re := range classifiers {
		if re.MatchString(message) {
			return Patterns[i].Kind
		}
	}
	return ""
}

// SinceArgument is sinceArgument: now − h hours as LOCAL "YYYY-MM-DD HH:MM:SS" (absolute,
// because the relative forms journalctl accepts vary by systemd version).
func SinceArgument(lookbackHours float64, now time.Time) string {
	ms := now.UnixMilli() - int64(math.Trunc(lookbackHours*3600_000))
	return time.UnixMilli(ms).In(time.Local).Format("2006-01-02 15:04:05")
}

// Entry is one classified journal line.
type Entry struct {
	Kind     string
	Source   string
	Sample   string
	AtUnixMs int64
}

// sliceUTF16 is s.slice(0, n) in UTF-16 code units; a pair cut in half becomes U+FFFD.
func sliceUTF16(s string, n int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return string(utf16.Decode(u[:n]))
}

// ParseJournalJSON is parseJournalJson (journalWarnings.ts:143-177): the first LineLimit lines;
// blank, non-JSON, non-object lines and entries whose MESSAGE is not a string (journald's byte
// arrays for invalid UTF-8) are skipped, as are unclassified ones. Source is _SYSTEMD_UNIT without
// ".service", else fallback; the time is __REALTIME_TIMESTAMP µs → ms, else now.
func ParseJournalJSON(stdout, fallback string, now time.Time) []Entry {
	var out []Entry
	lines := strings.Split(stdout, "\n")
	if len(lines) > LineLimit {
		lines = lines[:LineLimit]
	}
	for _, line := range lines {
		t := jsjson.Trim(line)
		if t == "" {
			continue
		}
		v, err := jsjson.Parse(t)
		if err != nil {
			continue
		}
		obj, ok := v.(*jsjson.Object)
		if !ok {
			continue
		}
		mv, _ := obj.Get("MESSAGE")
		msg, ok := mv.(string)
		if !ok {
			continue
		}
		kind := Classify(msg)
		if kind == "" {
			continue
		}
		source := fallback
		if uv, _ := obj.Get("_SYSTEMD_UNIT"); uv != nil {
			if unit, ok := uv.(string); ok && unit != "" {
				source = strings.TrimSuffix(unit, ".service")
			}
		}
		at := now.UnixMilli()
		if tv, ok := obj.Get("__REALTIME_TIMESTAMP"); ok {
			micros := jsjson.ToNumber(tv)
			if !math.IsNaN(micros) && !math.IsInf(micros, 0) && micros > 0 {
				at = int64(math.Floor(micros / 1000))
			}
		}
		out = append(out, Entry{Kind: kind, Source: source, Sample: sliceUTF16(msg, SampleLimit), AtUnixMs: at})
	}
	return out
}

// Warning is one aggregated warning class.
type Warning struct {
	Kind           string
	Source         string
	Sample         string
	Occurrences    uint32
	LastSeenUnixMs int64
}

// Aggregate is aggregateWarnings: per kind|source, occurrences summed (across passes too), the
// sample and time of the latest entry (ties: the later one), sorted by lastSeen descending
// (stable, as Array.prototype.sort).
func Aggregate(entries []Entry) []Warning {
	var order []string
	by := map[string]*Warning{}
	for _, e := range entries {
		key := e.Kind + "|" + e.Source
		w, ok := by[key]
		if !ok {
			by[key] = &Warning{Kind: e.Kind, Source: e.Source, Sample: e.Sample, Occurrences: 1, LastSeenUnixMs: e.AtUnixMs}
			order = append(order, key)
			continue
		}
		w.Occurrences++
		if e.AtUnixMs >= w.LastSeenUnixMs {
			w.LastSeenUnixMs, w.Sample = e.AtUnixMs, e.Sample
		}
	}
	out := make([]Warning, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeenUnixMs > out[j].LastSeenUnixMs })
	return out
}

// ManagedUnits is managedUnits: the front sing-box unit, the sing-box rear unit and caddy (the
// mihomo rear unit is not scanned, as in TS).
func ManagedUnits(cfg *config.Config) []string {
	return []string{
		strings.TrimSuffix(path.Base(cfg.SingboxUnitFilePath), ".service"),
		strings.TrimSuffix(path.Base(cfg.RearSingboxUnitFilePath), ".service"),
		"caddy",
	}
}

// Pass is one journalctl invocation of the scan.
type Pass struct {
	Args     []string
	Fallback string
}

// Passes are the two scans (journalWarnings.ts:240-256): the managed units, then the kernel
// ring buffer (-k and -u are mutually exclusive).
func Passes(cfg *config.Config, lookbackHours float64, now time.Time) []Pass {
	common := []string{
		"--since", SinceArgument(lookbackHours, now),
		"--grep", GrepPattern(),
		"--case-sensitive=false", "--output=json", "--no-pager", "--lines=" + strconv.Itoa(LineLimit),
	}
	var units []string
	for _, u := range ManagedUnits(cfg) {
		units = append(units, "-u", u)
	}
	return []Pass{
		{Args: append(units, common...), Fallback: "unknown"},
		{Args: append([]string{"-k"}, common...), Fallback: "kernel"},
	}
}

// Logger is the slice of slog the scan needs.
type Logger interface {
	Warn(msg string, args ...any)
}

// CollectJournalWarnings is collectJournalWarnings: the two passes one after the other, each
// bounded by PassTimeout. A failed pass that still produced output (exit 1 with lines, a timeout
// kill after some output) is parsed anyway; exit 1 with no output is "no matches" and silent.
func CollectJournalWarnings(ctx context.Context, r runner.Runner, cfg *config.Config, log Logger, lookbackHours float64, now time.Time) []Warning {
	var all []Entry
	for _, p := range Passes(cfg, lookbackHours, now) {
		res, err := r.Run(ctx, runner.Cmd{Name: "journalctl", Args: p.Args, Timeout: PassTimeout})
		if err == nil {
			all = append(all, ParseJournalJSON(res.Stdout, p.Fallback, now)...)
			continue
		}
		if jsjson.Trim(res.Stdout) != "" {
			all = append(all, ParseJournalJSON(res.Stdout, p.Fallback, now)...)
			continue
		}
		var re *runner.Error
		isRunErr := errors.As(err, &re)
		switch {
		case isRunErr && re.Killed:
			log.Warn("Journal warning scan pass hit the time limit — reporting without it",
				"timeoutMs", PassTimeout.Milliseconds(), "source", p.Fallback)
		case isRunErr && re.Code == 1:
			// no matches
		default:
			log.Warn("Journal warning scan pass failed", "err", err.Error(), "args", strings.Join(p.Args, " "))
		}
	}
	return Aggregate(all)
}
