// Package config loads the agent environment: the Go port of src/config.ts (spec A §2).
//
// Semantics kept from TS:
//   - Sources: the process environment wins; a dotenv file (./.env relative to cwd, the same
//     /opt/route-agent/.env systemd already loads via EnvironmentFile=) fills only absent keys
//     (dotenv 16 no-override, see dotenv.go).
//   - The same 33 variables with the same defaults. A default applies only when the variable is
//     ABSENT; an empty value is kept (zod .default()).
//   - Numbers use JavaScript Number(x) coercion (z.coerce.number()): "" is 0 and valid, "abc" is
//     NaN and invalid, no integer or range check (number.go).
//   - EGRESS_CONTROL_SECRET is required; empty is accepted (every authenticated call then fails).
//   - Unknown variables are ignored.
//   - On failure main writes "❌ Invalid agent environment variables: <zod-format JSON>" to
//     stderr and exits 1 (ValidationError.Error() is that JSON).
//
// One addition: LOG_LEVEL (debug|info|warn|error, default info), read outside the 33-variable
// schema; see docs/go-rewrite/deviations.md.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"strings"
)

// Config is the validated environment. Field names mirror the variable names.
type Config struct {
	Port                       float64 // PORT, coerced number (listen code checks it is a port)
	Host                       string
	EgressControlSecret        string
	SingboxConfigPath          string
	SingboxBinaryPath          string
	SingboxUnitFilePath        string
	ReloadCommand              string // shell string, contract (spec A §6)
	SingboxRestartCommand      string // shell string, contract
	RearSingboxConfigPath      string
	RearSingboxUnitFilePath    string
	MihomoBinaryPath           string
	RearMihomoConfigPath       string
	RearMihomoUnitFilePath     string
	RearRuleSetDir             string
	RearWarpGuardIntervalMs    float64
	SysctlProfilePath          string
	NodeWarningLookbackHours   float64
	CaddyfilePath              string
	CaddyReloadCommand         string // shell string, contract; default keeps "|| systemctl restart caddy"
	CaddyBinaryPath            string // optional: "" when unset
	CaddyBinaryPathSet         bool
	OlcrtcAgentSrvBinaryPath   string
	OlcrtcAgentSrvUnitFilePath string
	OlcrtcAgentSrvConfigDir    string
	AwgConfigPath              string
	AwgReloadCommand           string // declared but unused in TS (spec A R-14)
	AwgToolsBinaryPath         string
	AwgQuickBinaryPath         string
	AwgGoBinaryPath            string
	AwgUnitFilePath            string
	MeshAwgConfigPath          string
	CACertPath                 string
	AgentCertPath              string
	AgentKeyPath               string

	// LogLevel is not one of the 33; see the package comment.
	LogLevel slog.Level
}

type kind int

const (
	kString kind = iota
	kNumber
	kRequired
	kOptional
)

type varDef struct {
	name string
	kind kind
	def  string
	str  func(*Config) *string
	num  func(*Config) *float64
}

// schema lists the 33 variables in src/config.ts order (the order matters only for error output).
var schema = []varDef{
	{name: "PORT", kind: kNumber, def: "8081", num: func(c *Config) *float64 { return &c.Port }},
	{name: "HOST", def: "0.0.0.0", str: func(c *Config) *string { return &c.Host }},
	{name: "EGRESS_CONTROL_SECRET", kind: kRequired, str: func(c *Config) *string { return &c.EgressControlSecret }},
	{name: "SINGBOX_CONFIG_PATH", def: "/etc/sing-box/config.json", str: func(c *Config) *string { return &c.SingboxConfigPath }},
	{name: "SINGBOX_BINARY_PATH", def: "/usr/local/bin/sing-box", str: func(c *Config) *string { return &c.SingboxBinaryPath }},
	{name: "SINGBOX_UNIT_FILE_PATH", def: "/etc/systemd/system/sing-box.service", str: func(c *Config) *string { return &c.SingboxUnitFilePath }},
	{name: "RELOAD_COMMAND", def: "systemctl reload sing-box", str: func(c *Config) *string { return &c.ReloadCommand }},
	{name: "SINGBOX_RESTART_COMMAND", def: "systemctl restart sing-box", str: func(c *Config) *string { return &c.SingboxRestartCommand }},
	{name: "REAR_SINGBOX_CONFIG_PATH", def: "/etc/route-agent/rear.json", str: func(c *Config) *string { return &c.RearSingboxConfigPath }},
	{name: "REAR_SINGBOX_UNIT_FILE_PATH", def: "/etc/systemd/system/route-rear-singbox.service", str: func(c *Config) *string { return &c.RearSingboxUnitFilePath }},
	{name: "MIHOMO_BINARY_PATH", def: "/usr/local/bin/mihomo", str: func(c *Config) *string { return &c.MihomoBinaryPath }},
	{name: "REAR_MIHOMO_CONFIG_PATH", def: "/etc/route-agent/rear.yaml", str: func(c *Config) *string { return &c.RearMihomoConfigPath }},
	{name: "REAR_MIHOMO_UNIT_FILE_PATH", def: "/etc/systemd/system/route-rear-mihomo.service", str: func(c *Config) *string { return &c.RearMihomoUnitFilePath }},
	{name: "REAR_RULE_SET_DIR", def: "/etc/route-agent/rules", str: func(c *Config) *string { return &c.RearRuleSetDir }},
	{name: "REAR_WARP_GUARD_INTERVAL_MS", kind: kNumber, def: "30000", num: func(c *Config) *float64 { return &c.RearWarpGuardIntervalMs }},
	{name: "SYSCTL_PROFILE_PATH", def: "/etc/sysctl.d/99-route-agent.conf", str: func(c *Config) *string { return &c.SysctlProfilePath }},
	{name: "NODE_WARNING_LOOKBACK_HOURS", kind: kNumber, def: "24", num: func(c *Config) *float64 { return &c.NodeWarningLookbackHours }},
	{name: "CADDYFILE_PATH", def: "/etc/caddy/Caddyfile", str: func(c *Config) *string { return &c.CaddyfilePath }},
	{name: "CADDY_RELOAD_COMMAND", def: "systemctl reload caddy || systemctl restart caddy", str: func(c *Config) *string { return &c.CaddyReloadCommand }},
	{name: "CADDY_BINARY_PATH", kind: kOptional, str: func(c *Config) *string { return &c.CaddyBinaryPath }},
	{name: "OLCRTC_AGENT_SRV_BINARY_PATH", def: "/usr/local/bin/olcrtc-agent-srv", str: func(c *Config) *string { return &c.OlcrtcAgentSrvBinaryPath }},
	{name: "OLCRTC_AGENT_SRV_UNIT_FILE_PATH", def: "/etc/systemd/system/olcrtc-agent-srv@.service", str: func(c *Config) *string { return &c.OlcrtcAgentSrvUnitFilePath }},
	{name: "OLCRTC_AGENT_SRV_CONFIG_DIR", def: "/etc/olcrtc-agent-srv", str: func(c *Config) *string { return &c.OlcrtcAgentSrvConfigDir }},
	{name: "AWG_CONFIG_PATH", def: "/etc/amnezia/amneziawg/awg0.conf", str: func(c *Config) *string { return &c.AwgConfigPath }},
	{name: "AWG_RELOAD_COMMAND", def: "systemctl restart route-awg@awg0", str: func(c *Config) *string { return &c.AwgReloadCommand }},
	{name: "AWG_TOOLS_BINARY_PATH", def: "/usr/local/bin/awg", str: func(c *Config) *string { return &c.AwgToolsBinaryPath }},
	{name: "AWG_QUICK_BINARY_PATH", def: "/usr/local/bin/awg-quick", str: func(c *Config) *string { return &c.AwgQuickBinaryPath }},
	{name: "AWG_GO_BINARY_PATH", def: "/usr/local/bin/amneziawg-go", str: func(c *Config) *string { return &c.AwgGoBinaryPath }},
	{name: "AWG_UNIT_FILE_PATH", def: "/etc/systemd/system/route-awg@.service", str: func(c *Config) *string { return &c.AwgUnitFilePath }},
	{name: "MESH_AWG_CONFIG_PATH", def: "/etc/amnezia/amneziawg/awgmesh0.conf", str: func(c *Config) *string { return &c.MeshAwgConfigPath }},
	{name: "CA_CERT_PATH", def: "/etc/route-agent/certs/ca.crt", str: func(c *Config) *string { return &c.CACertPath }},
	{name: "AGENT_CERT_PATH", def: "/etc/route-agent/certs/agent.crt", str: func(c *Config) *string { return &c.AgentCertPath }},
	{name: "AGENT_KEY_PATH", def: "/etc/route-agent/certs/agent.key", str: func(c *Config) *string { return &c.AgentKeyPath }},
}

// VariableNames returns the 33 schema variable names in src/config.ts order.
func VariableNames() []string {
	out := make([]string, len(schema))
	for i, v := range schema {
		out[i] = v.name
	}
	return out
}

// ValidationError lists the variables that failed validation. Error() is the zod
// error.format() object as JSON.stringify(…, null, 2) prints it.
type ValidationError struct {
	Issues []Issue
}

// Issue is one invalid variable.
type Issue struct {
	Name    string
	Message string
}

func (e *ValidationError) Error() string {
	var b bytes.Buffer
	b.WriteString("{\n  \"_errors\": []")
	for _, is := range e.Issues {
		name, _ := json.Marshal(is.Name)
		msg, _ := json.Marshal(is.Message)
		b.WriteString(",\n  ")
		b.Write(name)
		b.WriteString(": {\n    \"_errors\": [\n      ")
		b.Write(msg)
		b.WriteString("\n    ]\n  }")
	}
	b.WriteString("\n}")
	return b.String()
}

// StderrMessage is the exact line TS wrote to stderr before exit(1) (src/config.ts:100).
func StderrMessage(err error) string {
	return "❌ Invalid agent environment variables: " + err.Error() + "\n"
}

// EnvMap turns os.Environ()-style entries into a map. The first "=" splits; later duplicates win.
func EnvMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// Merge returns env with every dotenv key that env lacks added (dotenv no-override).
func Merge(env, dotenv map[string]string) map[string]string {
	out := make(map[string]string, len(env)+len(dotenv))
	for k, v := range dotenv {
		out[k] = v
	}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// Parse validates an already-merged environment.
func Parse(env map[string]string) (*Config, error) {
	c := &Config{}
	var issues []Issue
	for _, v := range schema {
		raw, present := env[v.name]
		switch v.kind {
		case kRequired:
			if !present {
				issues = append(issues, Issue{v.name, "Required"})
				continue
			}
			*v.str(c) = raw
		case kOptional:
			*v.str(c) = raw
			if v.name == "CADDY_BINARY_PATH" {
				c.CaddyBinaryPathSet = present
			}
		case kNumber:
			if !present {
				raw = v.def
			}
			n := JSNumber(raw)
			if math.IsNaN(n) {
				issues = append(issues, Issue{v.name, "Expected number, received nan"})
				continue
			}
			*v.num(c) = n
		default:
			if !present {
				raw = v.def
			}
			*v.str(c) = raw
		}
	}
	c.LogLevel = parseLogLevel(env["LOG_LEVEL"])
	if len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	return c, nil
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Loaded is the result of Load: the config plus the dotenv keys that were absent from the
// process environment (main exports them, as dotenv populated process.env for children).
type Loaded struct {
	Config      *Config
	DotenvAdded map[string]string
}

// Load reads environ and the dotenv file at dotenvPath (a missing file is fine, as in dotenv).
func Load(environ []string, dotenvPath string) (*Loaded, error) {
	env := EnvMap(environ)
	added := map[string]string{}
	if data, err := os.ReadFile(dotenvPath); err == nil {
		for k, v := range ParseDotenv(data) {
			if _, ok := env[k]; !ok {
				added[k] = v
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// dotenv logs and carries on when the file is unreadable; so do we (systemd already
		// loaded it via EnvironmentFile= in production).
		_ = err
	}
	c, err := Parse(Merge(env, added))
	if err != nil {
		return nil, err
	}
	return &Loaded{Config: c, DotenvAdded: added}, nil
}
