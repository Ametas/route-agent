package clashapi

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// isoDate covers the ISO-8601 shapes V8's Date.parse accepts (and the lenient variants it also
// takes: lowercase t/z, a space separator, ±HHMM offsets). sing-box emits Go's RFC3339Nano.
var isoDate = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:[Tt ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?)?(Z|z|[+-]\d{2}:?\d{2})?$`)

// DateParse is Date.parse for ISO-8601 strings: milliseconds since the epoch, fractional seconds
// truncated to milliseconds (".123456789" → 123 ms), a date-only form in UTC and a date-time
// without offset in local time, as V8. Out-of-range fields (month 13, 24:00:01, second 60,
// offset hour 24) are invalid. V8's legacy fallback formats ("Sep 15 2026 …") are not parsed:
// the Clash API never emits them, and an unparsed start time is 0, as for junk in TS.
func DateParse(s string) (float64, bool) {
	m := isoDate.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	num := func(x string, def int) int {
		if x == "" {
			return def
		}
		n, _ := strconv.Atoi(x)
		return n
	}
	year := num(m[1], 0)
	month, day := num(m[2], 1), num(m[3], 1)
	hasTime := m[4] != ""
	hour, minute, sec := num(m[4], 0), num(m[5], 0), num(m[6], 0)
	ms := 0
	if m[7] != "" {
		frac := (m[7] + "00")[:3]
		ms = num(frac, 0)
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || minute > 59 || sec > 59 {
		return 0, false
	}
	if hour > 24 || (hour == 24 && (minute != 0 || sec != 0 || ms != 0)) {
		return 0, false
	}
	loc := time.UTC
	switch off := m[8]; {
	case off == "Z" || off == "z":
	case off != "":
		sign := 1
		if off[0] == '-' {
			sign = -1
		}
		digits := strings.ReplaceAll(off[1:], ":", "")
		oh, om := num(digits[:2], 0), num(digits[2:], 0)
		if oh > 23 || om > 59 {
			return 0, false
		}
		loc = time.FixedZone("", sign*(oh*3600+om*60))
	case hasTime:
		loc = time.Local
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, loc)
	return float64(t.UnixMilli() + int64(ms)), true
}
