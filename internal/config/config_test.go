package config

import (
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, env map[string]string) *Config {
	t.Helper()
	c, err := Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestThirtyThreeVariables(t *testing.T) {
	if n := len(VariableNames()); n != 33 {
		t.Fatalf("schema has %d variables, want 33 (spec A §2.2)", n)
	}
}

func TestDefaults(t *testing.T) {
	c := mustParse(t, map[string]string{"EGRESS_CONTROL_SECRET": "s"})
	want := map[string]string{
		"HOST": c.Host, "SINGBOX_CONFIG_PATH": c.SingboxConfigPath, "SINGBOX_BINARY_PATH": c.SingboxBinaryPath,
		"SINGBOX_UNIT_FILE_PATH": c.SingboxUnitFilePath, "RELOAD_COMMAND": c.ReloadCommand,
		"SINGBOX_RESTART_COMMAND": c.SingboxRestartCommand, "REAR_SINGBOX_CONFIG_PATH": c.RearSingboxConfigPath,
		"REAR_SINGBOX_UNIT_FILE_PATH": c.RearSingboxUnitFilePath, "MIHOMO_BINARY_PATH": c.MihomoBinaryPath,
		"REAR_MIHOMO_CONFIG_PATH": c.RearMihomoConfigPath, "REAR_MIHOMO_UNIT_FILE_PATH": c.RearMihomoUnitFilePath,
		"REAR_RULE_SET_DIR": c.RearRuleSetDir, "SYSCTL_PROFILE_PATH": c.SysctlProfilePath,
		"CADDYFILE_PATH": c.CaddyfilePath, "CADDY_RELOAD_COMMAND": c.CaddyReloadCommand,
		"OLCRTC_AGENT_SRV_BINARY_PATH": c.OlcrtcAgentSrvBinaryPath, "OLCRTC_AGENT_SRV_UNIT_FILE_PATH": c.OlcrtcAgentSrvUnitFilePath,
		"OLCRTC_AGENT_SRV_CONFIG_DIR": c.OlcrtcAgentSrvConfigDir, "AWG_CONFIG_PATH": c.AwgConfigPath,
		"AWG_RELOAD_COMMAND": c.AwgReloadCommand, "AWG_TOOLS_BINARY_PATH": c.AwgToolsBinaryPath,
		"AWG_QUICK_BINARY_PATH": c.AwgQuickBinaryPath, "AWG_GO_BINARY_PATH": c.AwgGoBinaryPath,
		"AWG_UNIT_FILE_PATH": c.AwgUnitFilePath, "MESH_AWG_CONFIG_PATH": c.MeshAwgConfigPath,
		"CA_CERT_PATH": c.CACertPath, "AGENT_CERT_PATH": c.AgentCertPath, "AGENT_KEY_PATH": c.AgentKeyPath,
	}
	expected := map[string]string{
		"HOST": "0.0.0.0", "SINGBOX_CONFIG_PATH": "/etc/sing-box/config.json", "SINGBOX_BINARY_PATH": "/usr/local/bin/sing-box",
		"SINGBOX_UNIT_FILE_PATH": "/etc/systemd/system/sing-box.service", "RELOAD_COMMAND": "systemctl reload sing-box",
		"SINGBOX_RESTART_COMMAND": "systemctl restart sing-box", "REAR_SINGBOX_CONFIG_PATH": "/etc/route-agent/rear.json",
		"REAR_SINGBOX_UNIT_FILE_PATH": "/etc/systemd/system/route-rear-singbox.service", "MIHOMO_BINARY_PATH": "/usr/local/bin/mihomo",
		"REAR_MIHOMO_CONFIG_PATH": "/etc/route-agent/rear.yaml", "REAR_MIHOMO_UNIT_FILE_PATH": "/etc/systemd/system/route-rear-mihomo.service",
		"REAR_RULE_SET_DIR": "/etc/route-agent/rules", "SYSCTL_PROFILE_PATH": "/etc/sysctl.d/99-route-agent.conf",
		"CADDYFILE_PATH": "/etc/caddy/Caddyfile", "CADDY_RELOAD_COMMAND": "systemctl reload caddy || systemctl restart caddy",
		"OLCRTC_AGENT_SRV_BINARY_PATH": "/usr/local/bin/olcrtc-agent-srv", "OLCRTC_AGENT_SRV_UNIT_FILE_PATH": "/etc/systemd/system/olcrtc-agent-srv@.service",
		"OLCRTC_AGENT_SRV_CONFIG_DIR": "/etc/olcrtc-agent-srv", "AWG_CONFIG_PATH": "/etc/amnezia/amneziawg/awg0.conf",
		"AWG_RELOAD_COMMAND": "systemctl restart route-awg@awg0", "AWG_TOOLS_BINARY_PATH": "/usr/local/bin/awg",
		"AWG_QUICK_BINARY_PATH": "/usr/local/bin/awg-quick", "AWG_GO_BINARY_PATH": "/usr/local/bin/amneziawg-go",
		"AWG_UNIT_FILE_PATH": "/etc/systemd/system/route-awg@.service", "MESH_AWG_CONFIG_PATH": "/etc/amnezia/amneziawg/awgmesh0.conf",
		"CA_CERT_PATH": "/etc/route-agent/certs/ca.crt", "AGENT_CERT_PATH": "/etc/route-agent/certs/agent.crt", "AGENT_KEY_PATH": "/etc/route-agent/certs/agent.key",
	}
	for k, v := range expected {
		if want[k] != v {
			t.Errorf("%s = %q, want %q", k, want[k], v)
		}
	}
	if c.Port != 8081 || c.RearWarpGuardIntervalMs != 30000 || c.NodeWarningLookbackHours != 24 {
		t.Errorf("numeric defaults: %v %v %v", c.Port, c.RearWarpGuardIntervalMs, c.NodeWarningLookbackHours)
	}
	if c.CaddyBinaryPath != "" || c.CaddyBinaryPathSet {
		t.Error("CADDY_BINARY_PATH has no default")
	}
	if c.LogLevel != slog.LevelInfo {
		t.Error("default log level must be info")
	}
}

func TestDefaultOnlyWhenAbsent(t *testing.T) {
	c := mustParse(t, map[string]string{"EGRESS_CONTROL_SECRET": "", "CADDY_RELOAD_COMMAND": "", "PORT": "", "CADDY_BINARY_PATH": ""})
	if c.CaddyReloadCommand != "" {
		t.Error("an empty value must not take the default (zod .default())")
	}
	if c.Port != 0 {
		t.Errorf("PORT= coerces to 0, got %v", c.Port)
	}
	if c.EgressControlSecret != "" {
		t.Error("empty secret is accepted")
	}
	if !c.CaddyBinaryPathSet {
		t.Error("CADDY_BINARY_PATH= is present")
	}
}

func TestInvalid(t *testing.T) {
	_, err := Parse(map[string]string{"PORT": "abc"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	want := "{\n  \"_errors\": [],\n  \"PORT\": {\n    \"_errors\": [\n      \"Expected number, received nan\"\n    ]\n  },\n" +
		"  \"EGRESS_CONTROL_SECRET\": {\n    \"_errors\": [\n      \"Required\"\n    ]\n  }\n}"
	if err.Error() != want {
		t.Fatalf("got\n%s\nwant\n%s", err.Error(), want)
	}
	if !strings.HasPrefix(StderrMessage(err), "❌ Invalid agent environment variables: {") {
		t.Fatal(StderrMessage(err))
	}
}

func TestJSNumber(t *testing.T) {
	cases := map[string]float64{
		"": 0, "  ": 0, "8081": 8081, " 42 ": 42, "1e3": 1000, "0x10": 16, "0b101": 5, "0o17": 15,
		"1.5": 1.5, ".5": 0.5, "5.": 5, "-3": -3, "+3": 3, "Infinity": math.Inf(1), "-Infinity": math.Inf(-1),
	}
	for in, want := range cases {
		if got := JSNumber(in); got != want {
			t.Errorf("JSNumber(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"abc", "1_000", "0x", "-0x10", "inf", "NaN", "1e", "12px", "0xZ"} {
		if got := JSNumber(in); !math.IsNaN(got) {
			t.Errorf("JSNumber(%q) = %v, want NaN", in, got)
		}
	}
}

// The exact file install.sh writes (spec A §2.2), including the unquoted value with spaces.
const installEnv = `PORT=8081
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
CA_CERT_PATH=/etc/route-agent/certs/ca.crt
AGENT_CERT_PATH=/etc/route-agent/certs/agent.crt
AGENT_KEY_PATH=/etc/route-agent/certs/agent.key
`

func TestLoadInstallEnvAndPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(installEnv+"OLCRTC_BINARY_PATH=/unknown/is/ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load([]string{"HOST=127.0.0.1", "UNRELATED=1"}, path)
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Host != "127.0.0.1" {
		t.Errorf("environment must win over .env, HOST=%q", c.Host)
	}
	if c.ReloadCommand != "systemctl reload sing-box" {
		t.Errorf("RELOAD_COMMAND=%q", c.ReloadCommand)
	}
	if len(c.EgressControlSecret) != 64 || c.Port != 8081 {
		t.Errorf("secret/port: %q %v", c.EgressControlSecret, c.Port)
	}
	if _, ok := l.DotenvAdded["HOST"]; ok {
		t.Error("HOST was in the environment; dotenv must not override it")
	}
	if l.DotenvAdded["OLCRTC_BINARY_PATH"] != "/unknown/is/ignored" {
		t.Error("dotenv still exports unknown keys to the process environment")
	}
	// Missing .env is fine.
	if _, err := Load([]string{"EGRESS_CONTROL_SECRET=x"}, filepath.Join(t.TempDir(), "none")); err != nil {
		t.Fatal(err)
	}
	// Missing secret everywhere fails.
	if _, err := Load(nil, filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("missing EGRESS_CONTROL_SECRET must fail")
	}
}

func TestParseDotenv(t *testing.T) {
	src := "# comment\n" +
		"export A=1\n" +
		"B = spaced value  # trailing comment\n" +
		"C='single # kept'\n" +
		"D=\"line\\nbreak\"\n" +
		"E=`back`\n" +
		"F=\n" +
		"G=first\n" +
		"G=second\r\n" +
		"H=\"multi\nline\"\n" +
		"I='no\\nexpand'\n"
	got := ParseDotenv([]byte(src))
	want := map[string]string{
		"A": "1", "B": "spaced value", "C": "single # kept", "D": "line\nbreak", "E": "back", "F": "",
		"G": "second", "H": "multi\nline", "I": `no\nexpand`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extra keys: %v", got)
	}
}

func TestLogLevel(t *testing.T) {
	c := mustParse(t, map[string]string{"EGRESS_CONTROL_SECRET": "s", "LOG_LEVEL": "debug"})
	if c.LogLevel != slog.LevelDebug {
		t.Fatal(c.LogLevel)
	}
}
