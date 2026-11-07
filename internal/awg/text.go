// Package awg implements the client-facing AmneziaWG interface of group D
// (docs/go-rewrite/spec/d-awg-mesh-firewall-shaping.md §1–§5): the RPCs ConfigureAwg and
// GetAwgObservation, the AWG health/self-heal loop and the telemetry readers for it (active peers,
// health status). The config text, live-interface probe and the "syncconf or restart" apply step
// are exported for the mesh tunnel (internal/mesh), which writes the same file format to the same
// route-awg@.service template.
package awg

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// Sanitize is sanitizeConfigInput (config.service.ts:27-30, meshTunnel.service.ts:17-20): every
// CR and LF removed, then trimmed the way String.prototype.trim does. A value cannot smuggle a
// second directive (`PostUp = …`) into the config this way.
func Sanitize(v string) string {
	v = strings.NewReplacer("\r", "", "\n", "").Replace(v)
	return jsjson.Trim(v)
}

// BoolDirective is awgBoolDirective (config.service.ts:38-43): on|1 → "<name> = on\n",
// off|0 → "<name> = off\n", anything else (empty, junk, an injection attempt) → no line.
func BoolDirective(name, raw string) string {
	switch strings.ToLower(jsjson.Trim(raw)) {
	case "on", "1":
		return name + " = on\n"
	case "off", "0":
		return name + " = off\n"
	}
	return ""
}

// InterfaceName is basename(path, ".conf") with a fallback (src/utils/awg.ts:12-26): an empty
// path means def+".conf" under /etc/amnezia/amneziawg, an empty basename means def.
func InterfaceName(path, def string) string {
	if path == "" {
		return def
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".conf") && base != ".conf" {
		base = strings.TrimSuffix(base, ".conf")
	}
	if base == "" || base == "." || base == "/" {
		return def
	}
	return base
}

// Obfuscation are the AWG3 [Interface] parameters both ConfigureAwg and ConfigureMeshTunnel
// write, in this order, right after Address (and the mesh's MTU).
type Obfuscation struct {
	Jc, Jmin, Jmax uint32
	S              [4]string // raw request values
	H              [4]string
	I              [5]string
	HPK            string
}

// Render appends the Jc…HeaderProtectionKey lines. Jc/Jmin/Jmax are always written: the TS
// handlers tested `!== undefined && !== null`, which proto3 defaults make always true (spec D §0.1).
// S/H are written when the RAW value is non-empty (so "  " writes an empty value) and carry the
// sanitized value; I1-I5 and the key are sanitized first and omitted when empty (spec D §1.2).
func (o Obfuscation) Render(b *strings.Builder) {
	b.WriteString("Jc = " + strconv.FormatUint(uint64(o.Jc), 10) + "\n")
	b.WriteString("Jmin = " + strconv.FormatUint(uint64(o.Jmin), 10) + "\n")
	b.WriteString("Jmax = " + strconv.FormatUint(uint64(o.Jmax), 10) + "\n")
	for i, s := range o.S {
		if s != "" {
			b.WriteString("S" + strconv.Itoa(i+1) + " = " + Sanitize(s) + "\n")
		}
	}
	for i, h := range o.H {
		if h != "" {
			b.WriteString("H" + strconv.Itoa(i+1) + " = " + Sanitize(h) + "\n")
		}
	}
	for i, v := range o.I {
		if c := Sanitize(v); c != "" {
			b.WriteString("I" + strconv.Itoa(i+1) + " = " + c + "\n")
		}
	}
	if c := Sanitize(o.HPK); c != "" {
		b.WriteString("HeaderProtectionKey = " + c + "\n")
	}
}

// InterfaceSection is the text before the first [Peer] block: everything the interface itself is
// built from. A change here (address, port, MTU, keys, mimicry) is applied by a full restart; a
// change only below it (the peer list) is applied live with `awg syncconf`.
func InterfaceSection(conf string) string {
	if i := strings.Index(conf, "\n[Peer]"); i >= 0 {
		return conf[:i]
	}
	return conf
}

// CmdText is the TS idiom `(err.stderr || err.stdout || err.message || fallback).trim()`.
func CmdText(err error, fallback string) string {
	var e *runner.Error
	if errors.As(err, &e) {
		switch {
		case e.Stderr != "":
			return jsjson.Trim(e.Stderr)
		case e.Stdout != "":
			return jsjson.Trim(e.Stdout)
		}
	}
	if err != nil && err.Error() != "" {
		return jsjson.Trim(err.Error())
	}
	return fallback
}

// ParseIntJS is Number.parseInt(s, 10): optional leading whitespace and sign, then the leading
// decimal digits; ok is false when there are none (NaN).
func ParseIntJS(s string) (int64, bool) {
	s = strings.TrimLeftFunc(s, jsjson.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
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
		// More digits than int64 holds: JS gives a huge double; saturate.
		n = 1<<63 - 1
	}
	if neg {
		n = -n
	}
	return n, true
}

// Fields is `line.trim().split(/\s+/)` for a non-empty trimmed line.
func Fields(line string) []string {
	return strings.FieldsFunc(line, jsjson.IsSpace)
}
