package jsjson

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Stringify is JSON.stringify(v, null, indent) for the value model of this package. indent ""
// gives the compact form; "  " gives JSON.stringify(v, null, 2). No trailing newline. NaN and
// ±Infinity print as null, -0 as 0, as in JS.
func Stringify(v any, indent string) string {
	var b strings.Builder
	writeValue(&b, v, indent, "")
	return b.String()
}

func writeValue(b *strings.Builder, v any, indent, cur string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(FormatNumber(t))
		}
	case int:
		b.WriteString(FormatNumber(float64(t)))
	case string:
		writeString(b, t)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		inner := cur + indent
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteByte('\n')
				b.WriteString(inner)
			}
			writeValue(b, e, indent, inner)
		}
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(cur)
		}
		b.WriteByte(']')
	case *Object:
		keys := t.Keys()
		if len(keys) == 0 {
			b.WriteString("{}")
			return
		}
		inner := cur + indent
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteByte('\n')
				b.WriteString(inner)
			}
			writeString(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			writeValue(b, t.m[k], indent, inner)
		}
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(cur)
		}
		b.WriteByte('}')
	default:
		// Not part of the value model; JSON.stringify would drop undefined/functions. Never
		// produced by Parse.
		b.WriteString("null")
	}
}

const hexDigits = "0123456789abcdef"

// writeString quotes s the way JSON.stringify does (ES2019 well-formed stringify): only ", \ and
// U+0000–U+001F are escaped (\b \f \n \r \t, the rest as lowercase \u00xx) plus lone surrogates
// as lowercase \udxxx. <, >, &, U+2028 and U+2029 stay raw.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					b.WriteString(`\u00`)
					b.WriteByte(hexDigits[c>>4])
					b.WriteByte(hexDigits[c&0xF])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		if u, ok := wtf8Surrogate(s[i:]); ok {
			b.WriteString(`\u`)
			b.WriteByte(hexDigits[u>>12])
			b.WriteByte(hexDigits[(u>>8)&0xF])
			b.WriteByte(hexDigits[(u>>4)&0xF])
			b.WriteByte(hexDigits[u&0xF])
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid byte: a JS string cannot hold it; Node would have decoded it as U+FFFD.
			b.WriteString("�")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// wtf8Surrogate recognises the 3-byte WTF-8 encoding of a lone surrogate (ED A0..BF 80..BF).
func wtf8Surrogate(s string) (uint16, bool) {
	if len(s) < 3 || s[0] != 0xED || s[1] < 0xA0 || s[1] > 0xBF || s[2] < 0x80 || s[2] > 0xBF {
		return 0, false
	}
	return uint16(0xD000) | uint16(s[1]&0x3F)<<6 | uint16(s[2]&0x3F), true
}

func appendWTF8Surrogate(b []byte, u uint16) []byte {
	return append(b, 0xED, byte(0x80|(u>>6)&0x3F), byte(0x80|u&0x3F))
}

// utf16Units returns the UTF-16 code units of s as JS sees the string.
func utf16Units(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		if u, ok := wtf8Surrogate(s[i:]); ok {
			out = append(out, u)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x10000 {
			a, c := utf16.EncodeRune(r)
			out = append(out, uint16(a), uint16(c))
		} else {
			out = append(out, uint16(r))
		}
		i += size
	}
	return out
}

// unitsToString is the inverse of utf16Units (pairs join, lone surrogates become WTF-8).
func unitsToString(units []uint16) string {
	b := make([]byte, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF:
			b = utf8.AppendRune(b, utf16.DecodeRune(rune(u), rune(units[i+1])))
			i++
		case u >= 0xD800 && u <= 0xDFFF:
			b = appendWTF8Surrogate(b, u)
		default:
			b = utf8.AppendRune(b, rune(u))
		}
	}
	return string(b)
}

// FormatNumber is JavaScript's Number.prototype.toString() for a finite double (ECMA-262
// Number::toString): the shortest round-trip digits, plain notation for exponents in
// [-7, 21), exponent notation ("1e+21", "5e-7", "1.5e-10") otherwise; -0 prints as "0".
func FormatNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// Shortest round-trip digits: "d.ddde±XX".
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digits

	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		ev := n - 1
		if ev < 0 {
			ev = -ev
		}
		if k == 1 {
			s = digits + "e" + sign + strconv.Itoa(ev)
		} else {
			s = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(ev)
		}
	}
	if neg {
		return "-" + s
	}
	return s
}

// String is JavaScript's String(v) for the value model (used only in log text).
func String(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return FormatNumber(t)
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e != nil {
				parts[i] = String(e)
			}
		}
		return strings.Join(parts, ",")
	case *Object:
		return "[object Object]"
	}
	return "undefined"
}
