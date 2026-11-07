package jsjson

import (
	"math"
	"strings"
	"unicode"

	"github.com/Ametas/route-agent/internal/config"
)

// ToNumber is JavaScript's Number(v) for the value model: numbers as is, strings through the JS
// string-to-number grammar (config.JSNumber: "" → 0, " 443 " → 443, "0x1BB" → 443, junk → NaN),
// booleans 0/1, null 0, arrays via their string form, objects NaN. Missing (undefined) is the
// caller's business: Number(undefined) is NaN.
func ToNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		return config.JSNumber(t)
	case []any:
		return config.JSNumber(String(t))
	}
	return math.NaN()
}

// IsInteger is Number.isInteger.
func IsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// ToUint32 is ECMAScript ToUint32 (v >>> 0): NaN and ±Infinity give 0, otherwise the value is
// truncated toward zero and reduced modulo 2^32. protobufjs writes uint32 fields this way.
func ToUint32(f float64) uint32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	t := math.Trunc(f)
	m := math.Mod(t, 4294967296)
	if m < 0 {
		m += 4294967296
	}
	return uint32(m)
}

// ToInt64 is how protobufjs encodes a JS number into an int64 field (LongBits.fromNumber):
// truncation toward zero, wrapping modulo 2^64; NaN gives 0. The TS agent sent byte counters and
// timestamps as plain numbers, so this is what reached the wire.
func ToInt64(f float64) int64 {
	if f == 0 || math.IsNaN(f) {
		return 0
	}
	neg := f < 0
	if neg {
		f = -f
	}
	lo := ToUint32(f)
	hi := ToUint32((f - float64(lo)) / 4294967296)
	if neg {
		lo = ^lo
		hi = ^hi
		lo++
		if lo == 0 {
			hi++
		}
	}
	return int64(uint64(hi)<<32 | uint64(lo))
}

// IsSpace is JavaScript's WhiteSpace ∪ LineTerminator: what String.prototype.trim strips and \s
// matches (Go's unicode.IsSpace adds U+0085 and lacks U+FEFF).
func IsSpace(r rune) bool { return r == 0xFEFF || (unicode.IsSpace(r) && r != 0x85) }

// Trim is String.prototype.trim.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }
