package jsjson

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SyntaxError is a JSON.parse failure. Message is V8's text (Node 22), e.g.
// "Unexpected end of JSON input" or
// "Expected ',' or '}' after property value in JSON at position 6 (line 1 column 7)".
type SyntaxError struct {
	Message string
}

func (e *SyntaxError) Error() string { return e.Message }

// Parse is JSON.parse(text). Invalid UTF-8 bytes in text are read as U+FFFD, as Node's UTF-8
// decoding of a file would have produced.
func Parse(text string) (any, error) {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	p := &parser{src: text}
	p.skipWS()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos < len(p.src) {
		return nil, p.errPos("Unexpected non-whitespace character after JSON at position ", p.pos)
	}
	return v, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) skipWS() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

// errAt builds "<msg> in JSON at position N (line L column C)" with N in UTF-16 units.
func (p *parser) errAt(msg string, bytePos int) *SyntaxError {
	return p.errPos(msg+" in JSON at position ", bytePos)
}

func (p *parser) errPos(lead string, bytePos int) *SyntaxError {
	prefix := p.src[:bytePos]
	pos := len(utf16Units(prefix))
	line := 1 + strings.Count(prefix, "\n")
	col := pos + 1
	if i := strings.LastIndexByte(prefix, '\n'); i >= 0 {
		col = len(utf16Units(prefix[i+1:])) + 1
	}
	return &SyntaxError{Message: lead + strconv.Itoa(pos) +
		" (line " + strconv.Itoa(line) + " column " + strconv.Itoa(col) + ")"}
}

func (p *parser) endOfInput() *SyntaxError {
	return &SyntaxError{Message: "Unexpected end of JSON input"}
}

// unexpectedToken is V8's kJsonParseUnexpectedToken* family: the offending character plus up to
// 10 UTF-16 units of context on each side.
func (p *parser) unexpectedToken(bytePos int) *SyntaxError {
	if bytePos >= len(p.src) {
		return p.endOfInput()
	}
	units := utf16Units(p.src)
	pos := len(utf16Units(p.src[:bytePos]))
	// The token is one UTF-16 unit: for an astral character V8 prints its high surrogate.
	token := unitsToString(units[pos : pos+1])
	const ctx = 10
	var snippet string
	switch {
	case len(units) <= ctx*2:
		snippet = `"` + p.src + `"`
	case pos < ctx:
		snippet = `"` + unitsToString(units[:pos+ctx]) + `"...`
	case pos >= len(units)-ctx:
		snippet = `..."` + unitsToString(units[pos-ctx:]) + `"`
	default:
		snippet = `..."` + unitsToString(units[pos-ctx:pos+ctx]) + `"...`
	}
	return &SyntaxError{Message: "Unexpected token '" + token + "', " + snippet + " is not valid JSON"}
}

func (p *parser) value() (any, error) {
	if p.eof() {
		return nil, p.endOfInput()
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	}
	return nil, p.unexpectedToken(p.pos)
}

func (p *parser) literal(word string) error {
	for i := 0; i < len(word); i++ {
		if p.pos+i >= len(p.src) {
			return p.endOfInput()
		}
		if c := p.src[p.pos+i]; c != word[i] {
			// V8 classifies the offending character as a token (ScanLiteral).
			switch {
			case c == '"':
				return p.errAt("Unexpected string", p.pos+i)
			case c == '-' || isDigit(c):
				return p.errAt("Unexpected number", p.pos+i)
			}
			return p.unexpectedToken(p.pos + i)
		}
	}
	p.pos += len(word)
	return nil
}

func (p *parser) object() (any, error) {
	p.pos++ // {
	o := NewObject()
	p.skipWS()
	if !p.eof() && p.src[p.pos] == '}' {
		p.pos++
		return o, nil
	}
	if p.eof() || p.src[p.pos] != '"' {
		return nil, p.errAt("Expected property name or '}'", p.pos)
	}
	for {
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.eof() || p.src[p.pos] != ':' {
			return nil, p.errAt("Expected ':' after property name", p.pos)
		}
		p.pos++
		p.skipWS()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		o.Set(k, v)
		p.skipWS()
		if p.eof() {
			return nil, p.errAt("Expected ',' or '}' after property value", p.pos)
		}
		switch p.src[p.pos] {
		case '}':
			p.pos++
			return o, nil
		case ',':
			p.pos++
			p.skipWS()
			if p.eof() || p.src[p.pos] != '"' {
				return nil, p.errAt("Expected double-quoted property name", p.pos)
			}
		default:
			return nil, p.errAt("Expected ',' or '}' after property value", p.pos)
		}
	}
}

func (p *parser) array() (any, error) {
	p.pos++ // [
	arr := []any{}
	p.skipWS()
	if !p.eof() && p.src[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipWS()
		if p.eof() {
			return nil, p.errAt("Expected ',' or ']' after array element", p.pos)
		}
		switch p.src[p.pos] {
		case ']':
			p.pos++
			return arr, nil
		case ',':
			p.pos++
			p.skipWS()
		default:
			return nil, p.errAt("Expected ',' or ']' after array element", p.pos)
		}
	}
}

func hexVal(c byte) (uint16, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint16(c - '0'), true
	case c >= 'a' && c <= 'f':
		return uint16(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return uint16(c-'A') + 10, true
	}
	return 0, false
}

// str parses a string starting at the opening quote. Escapes are collected as UTF-16 units so
// that a 😀 pair joins into one character and a lone surrogate survives (as WTF-8).
func (p *parser) str() (string, error) {
	p.pos++ // opening quote
	start := p.pos
	// Fast path: no escapes.
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			s := p.src[start:p.pos]
			p.pos++
			return s, nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", p.errAt("Bad control character in string literal", p.pos)
		}
		p.pos++
	}
	if p.eof() {
		return "", p.errAt("Unterminated string", p.pos)
	}
	units := utf16Units(p.src[start:p.pos])
	for {
		if p.eof() {
			return "", p.errAt("Unterminated string", p.pos)
		}
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return unitsToString(units), nil
		case c < 0x20:
			return "", p.errAt("Bad control character in string literal", p.pos)
		case c == '\\':
			p.pos++
			if p.eof() {
				return "", p.endOfInput()
			}
			e := p.src[p.pos]
			switch e {
			case '"', '\\', '/':
				units = append(units, uint16(e))
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			case 'u':
				var u uint16
				for i := 1; i <= 4; i++ {
					if p.pos+i >= len(p.src) {
						return "", p.endOfInput()
					}
					h, ok := hexVal(p.src[p.pos+i])
					if !ok {
						return "", p.errAt("Bad Unicode escape", p.pos+i)
					}
					u = u<<4 | h
				}
				units = append(units, u)
				p.pos += 4
			default:
				// V8's escape table covers Latin-1; a wider character is an "unexpected token".
				if r, _ := utf8.DecodeRuneInString(p.src[p.pos:]); r > 0xFF {
					return "", p.unexpectedToken(p.pos)
				}
				return "", p.errAt("Bad escaped character", p.pos)
			}
			p.pos++
		default:
			r, size := utf8.DecodeRuneInString(p.src[p.pos:])
			units = append(units, utf16Units(string(r))...)
			p.pos += size
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (p *parser) number() (any, error) {
	start := p.pos
	if p.src[p.pos] == '-' {
		p.pos++
		if p.eof() || !isDigit(p.src[p.pos]) {
			return nil, p.errAt("No number after minus sign", p.pos)
		}
	}
	if p.src[p.pos] == '0' {
		p.pos++
		if !p.eof() && isDigit(p.src[p.pos]) {
			return nil, p.errAt("Unexpected number", p.pos)
		}
	} else {
		for !p.eof() && isDigit(p.src[p.pos]) {
			p.pos++
		}
	}
	if !p.eof() && p.src[p.pos] == '.' {
		p.pos++
		if p.eof() || !isDigit(p.src[p.pos]) {
			return nil, p.errAt("Unterminated fractional number", p.pos)
		}
		for !p.eof() && isDigit(p.src[p.pos]) {
			p.pos++
		}
	}
	if !p.eof() && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if !p.eof() && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		if p.eof() || !isDigit(p.src[p.pos]) {
			return nil, p.errAt("Exponent part is missing a number", p.pos)
		}
		for !p.eof() && isDigit(p.src[p.pos]) {
			p.pos++
		}
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		// Only ErrRange is possible for a grammar-valid literal: ±Inf on overflow, as V8.
		if math.IsInf(f, 0) || f == 0 {
			return f, nil
		}
		return nil, p.unexpectedToken(start)
	}
	return f, nil
}
