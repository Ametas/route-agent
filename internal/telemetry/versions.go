package telemetry

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// NotInstalled is the version sentinel the orchestrator compares against.
const NotInstalled = "not_installed"

// versionTimeout bounds one `<bin> --version` probe (TS: none).
const versionTimeout = 10 * time.Second

// Defaults behind the TS `config.X || '<default>'` fallbacks.
const (
	defaultAwgTools = "/usr/local/bin/awg"
	defaultAwgGo    = "/usr/local/bin/amneziawg-go"
)

var (
	reAwgGoWord  = regexp.MustCompile(`(?i)amneziawg-go` + jsSpace + `+v?([0-9\.]+[\w\-]*)`)
	reAwgGoPlain = regexp.MustCompile(`(?i)amneziawg-go` + jsSpace + `+v?([0-9\.]+)`)
	reAwgDashed  = regexp.MustCompile(`(?i)v?([0-9\.]+\-[0-9a-zA-Z]+)`)
	reAwgDotted  = regexp.MustCompile(`(?i)v?([0-9]+\.[0-9]+[0-9a-zA-Z.-]*)`)
	reAwgDigits  = regexp.MustCompile(`([0-9\.]+)`)
	reLeadingV   = regexp.MustCompile(`(?i)^v`)
	reTrailPunct = regexp.MustCompile(jsSpace[:len(jsSpace)-1] + `.-]+$`)
)

// ParseAwgVersionString is parseAwgVersionString (telemetry.ts:206-220): the first of five
// patterns that matches, then a leading "v" and trailing whitespace, dots and dashes stripped;
// "" (no match, or nothing left) is TS's null.
func ParseAwgVersionString(out string) string {
	if out == "" {
		return ""
	}
	for _, re := range []*regexp.Regexp{reAwgGoWord, reAwgGoPlain, reAwgDashed, reAwgDotted, reAwgDigits} {
		m := re.FindStringSubmatch(out)
		if m == nil || m[1] == "" {
			continue
		}
		cleaned := jsjson.Trim(reTrailPunct.ReplaceAllString(reLeadingV.ReplaceAllString(m[1], ""), ""))
		if cleaned != "" && cleaned != "." {
			return cleaned
		}
		return ""
	}
	return ""
}

// versionOutput is `stdout, else stderr, trimmed (`(stdout || stderr).trim()`)` of a successful run.
func versionOutput(res runner.Result) string {
	if res.Stdout != "" {
		return jsjson.Trim(res.Stdout)
	}
	return jsjson.Trim(res.Stderr)
}

// dedupe keeps the first occurrence of each path (Array.from(new Set(...))).
func dedupe(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// AwgToolsVersion is getAwgToolsVersion (telemetry.ts:225-266): `<bin> --version` for the
// configured tools path, /usr/local/bin/awg and /usr/bin/awg (deduplicated, in order), then `awg`
// from PATH. An output mentioning amneziawg-go (case-insensitive) is a legacy symlink of awg to
// the Go core and answers not_installed at once; a failed run or an unparsable output tries the
// next candidate. No shell (the TS fallback was `awg --version` through /bin/sh).
func AwgToolsVersion(ctx context.Context, r runner.Runner, toolsPath string) string {
	for _, bin := range append(dedupe(orDefault(toolsPath, defaultAwgTools), defaultAwgTools, "/usr/bin/awg"), "awg") {
		res, err := r.Run(ctx, runner.Cmd{Name: bin, Args: []string{"--version"}, Timeout: versionTimeout})
		if err != nil {
			continue
		}
		out := versionOutput(res)
		if strings.Contains(strings.ToLower(out), "amneziawg-go") {
			return NotInstalled
		}
		if v := ParseAwgVersionString(out); v != "" {
			return v
		}
	}
	return NotInstalled
}

// AwgGoVersion is getAmneziaWgGoVersion (telemetry.ts:271-298): `<bin> --version` for the
// configured Go-core path and /usr/local/bin/amneziawg-go; "amneziawg-go <v>" gives <v> without
// a leading "v" (no trailing-punctuation strip on this branch, as in TS), otherwise
// ParseAwgVersionString; nothing → not_installed.
func AwgGoVersion(ctx context.Context, r runner.Runner, goPath string) string {
	for _, bin := range dedupe(orDefault(goPath, defaultAwgGo), defaultAwgGo) {
		res, err := r.Run(ctx, runner.Cmd{Name: bin, Args: []string{"--version"}, Timeout: versionTimeout})
		if err != nil {
			continue
		}
		out := versionOutput(res)
		m := reAwgGoWord.FindStringSubmatch(out)
		if m == nil {
			m = reAwgGoPlain.FindStringSubmatch(out)
		}
		if m != nil && m[1] != "" {
			return jsjson.Trim(reLeadingV.ReplaceAllString(m[1], ""))
		}
		if v := ParseAwgVersionString(out); v != "" {
			return v
		}
	}
	return NotInstalled
}

// awgVersions are awg_tools_version (also sent as awg_version) and awg_go_version. They share
// one cache timestamp in TS (lastAwgCheckTime) and so refresh together; here they are one cache.
type awgVersions struct {
	Tools string
	Go    string
}
