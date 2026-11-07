package config

import (
	"regexp"
	"strings"
)

// lineRe is dotenv 16.6.1's LINE regex (lib/main.js:9), which is RE2-compatible as written.
var lineRe = regexp.MustCompile("(?m)(?:^|^)\\s*(?:export\\s+)?([\\w.-]+)(?:\\s*=\\s*?|:\\s+?)(\\s*'(?:\\\\'|[^'])*'|\\s*\"(?:\\\\\"|[^\"])*\"|\\s*`(?:\\\\`|[^`])*`|[^#\\r\\n]+)?\\s*(?:#.*)?(?:$|$)")

// ParseDotenv parses a .env file with dotenv 16 semantics: KEY=value, optional "export ",
// unquoted values trimmed and cut at "#", quoted values (', ", `) may span lines and keep "#",
// "\n"/"\r" escapes expanded only in double quotes, later duplicates win. Unquoted values with
// spaces ("RELOAD_COMMAND=systemctl reload sing-box") are kept whole.
func ParseDotenv(src []byte) map[string]string {
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	out := map[string]string{}
	for _, m := range lineRe.FindAllStringSubmatch(text, -1) {
		key := m[1]
		value := strings.TrimSpace(m[2])
		quote := byte(0)
		if len(value) > 0 {
			quote = value[0]
		}
		// dotenv strips surrounding matching quotes with /^(['"`])([\s\S]*)\1$/; RE2 has no
		// backreferences, so compare the first and last byte instead.
		if len(value) >= 2 && (quote == '\'' || quote == '"' || quote == '`') && value[len(value)-1] == quote {
			value = value[1 : len(value)-1]
		}
		if quote == '"' {
			value = strings.ReplaceAll(value, `\n`, "\n")
			value = strings.ReplaceAll(value, `\r`, "\r")
		}
		out[key] = value
	}
	return out
}
