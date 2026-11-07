package rear

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
)

const sbConfig = `{"log":{"level":"warn"},"experimental":{"clash_api":{"external_controller":"127.0.0.1:29090","secret":"s"}},"n":1.50,"z":"/é"}`

func (h *harness) configure(enabled bool, core, cfg string) *agentpb.RearSingboxConfigResponse {
	h.t.Helper()
	resp, err := h.svc.ConfigureRearSingbox(mdCtx(secret), &agentpb.RearSingboxConfigPayload{Enabled: enabled, Core: core, ConfigJson: cfg})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func serialized(t *testing.T, raw string) string {
	t.Helper()
	v, err := jsjson.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Serialize(v)
}

// 1. Bad secret: no FS changes; payload-only correct secret is accepted.
func TestConfigureAuth(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	resp, _ := h.svc.ConfigureRearSingbox(mdCtx("wrong"), &agentpb.RearSingboxConfigPayload{OrchestratorSecret: "bad", Enabled: true, ConfigJson: sbConfig})
	if resp.Success || resp.Message != "Invalid orchestrator secret token." || resp.Running {
		t.Fatalf("resp %+v", resp)
	}
	if h.exists("/etc/route-agent/rear.json") || len(h.displays()) != 0 {
		t.Fatal("unauthenticated call touched the node")
	}
	resp, _ = h.svc.ConfigureRearSingbox(context.Background(), &agentpb.RearSingboxConfigPayload{OrchestratorSecret: secret, Enabled: true, ConfigJson: sbConfig})
	if !resp.Success {
		t.Fatalf("payload secret rejected: %+v", resp)
	}
}

// 2. Core resolution.
func TestConfigureCoreResolution(t *testing.T) {
	for _, core := range []string{"", "singbox", "SINGBOX", "garbage"} {
		h := newHarness(t, nil)
		h.installBinaries()
		if r := h.configure(true, core, sbConfig); !r.Success {
			t.Fatalf("core %q: %+v", core, r)
		}
		if !h.exists("/etc/route-agent/rear.json") || h.exists("/etc/route-agent/rear.yaml") || h.exists(DefaultMihomoUnitPath) {
			t.Fatalf("core %q did not resolve to sing-box", core)
		}
	}
	h := newHarness(t, nil)
	h.installBinaries()
	if r := h.configure(true, "mihomo", `{"external-controller":"127.0.0.1:29090"}`); !r.Success || r.Message != "Rear mihomo configuration validated, applied and (re)started." {
		t.Fatalf("mihomo: %+v", r)
	}
	if !h.exists("/etc/route-agent/rear.yaml") || !h.exists(DefaultMihomoUnitPath) || h.exists("/etc/route-agent/rear.json") || h.exists(DefaultSingboxUnitPath) {
		t.Fatal("mihomo must only touch its own paths")
	}
}

// 3. Disable: both cores stopped and removed, empty config accepted.
func TestConfigureDisableRemovesBothCores(t *testing.T) {
	for _, core := range []string{"singbox", "mihomo"} {
		h := newHarness(t, nil)
		for _, p := range []string{"/etc/route-agent/rear.json", "/etc/route-agent/rear.yaml", DefaultSingboxUnitPath, DefaultMihomoUnitPath} {
			h.write(p, "x")
		}
		r := h.configure(false, core, "")
		if !r.Success || r.Running || r.Message != "Rear instance stopped and removed." {
			t.Fatalf("resp %+v", r)
		}
		for _, p := range []string{"/etc/route-agent/rear.json", "/etc/route-agent/rear.yaml", DefaultSingboxUnitPath, DefaultMihomoUnitPath} {
			if h.exists(p) {
				t.Fatalf("%s left behind", p)
			}
		}
		if !h.ran("systemctl disable --now route-rear-singbox") || !h.ran("systemctl disable --now route-rear-mihomo") {
			t.Fatalf("calls %v", h.displays())
		}
		if h.ranPrefix("systemctl daemon-reload") {
			t.Fatal("TS ran no daemon-reload on disable")
		}
	}
}

// 4. Missing binary: skipped reason, exact message, nothing touched.
func TestConfigureMissingBinary(t *testing.T) {
	for core, want := range map[string]string{"singbox": "sing-box", "mihomo": "mihomo"} {
		h := newHarness(t, nil)
		h.write(DefaultSingboxUnitPath, "old")
		r := h.configure(true, core, sbConfig)
		id := core
		if r.Success || r.SkippedReason != id+"_not_installed" || r.Running ||
			r.Message != want+" is not installed on this node — nothing to run a rear instance from." {
			t.Fatalf("resp %+v", r)
		}
		if len(h.displays()) != 0 || !h.exists(DefaultSingboxUnitPath) {
			t.Fatal("nothing may be stopped or written")
		}
	}
}

// 5. Invalid JSON.
func TestConfigureInvalidJSON(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	r := h.configure(true, "", "{nope")
	if r.Success || r.Running || r.SkippedReason != "" || !strings.HasPrefix(r.Message, "Internal Agent Error: ") {
		t.Fatalf("resp %+v", r)
	}
	r = h.configure(true, "", "")
	if r.Message != "Internal Agent Error: Unexpected end of JSON input" {
		t.Fatalf("empty config: %q", r.Message)
	}
}

// 6. Core switch: validated first, previous removed and daemon-reload; a failed check leaves the
// previous core running (deviation from TS, spec F R1).
func TestConfigureSwitchRemovesPreviousAfterCheck(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	h.write(DefaultSingboxUnitPath, "unit")
	h.write("/etc/route-agent/rear.json", "{}")
	r := h.configure(true, "mihomo", `{"a":1}`)
	if !r.Success {
		t.Fatalf("resp %+v", r)
	}
	if h.exists(DefaultSingboxUnitPath) || h.exists("/etc/route-agent/rear.json") {
		t.Fatal("previous core not removed")
	}
	check := -1
	for i, c := range h.fake.Calls() {
		if len(c.Args) > 0 && c.Args[0] == "-t" {
			check = i
			break
		}
	}
	stop := h.index("systemctl disable --now route-rear-singbox")
	reload := h.index("systemctl daemon-reload")
	if check < 0 || stop < 0 || reload < 0 || !(check < stop && stop < reload) {
		t.Fatalf("order check=%d stop=%d reload=%d: %v", check, stop, reload, h.displays())
	}

	// Check failure: the previous core stays untouched.
	h2 := newHarness(t, nil)
	h2.installBinaries()
	h2.write(DefaultSingboxUnitPath, "unit")
	h2.write("/etc/route-agent/rear.json", "{}")
	h2.mihomoT = func(runner.Cmd) error { return errors.New("bad rule") }
	h2.inactive["route-rear-mihomo"] = true
	r = h2.configure(true, "mihomo", `{"a":1}`)
	if r.Success || r.SkippedReason != "check_failed" || r.Running {
		t.Fatalf("resp %+v", r)
	}
	if !h2.exists(DefaultSingboxUnitPath) || !h2.exists("/etc/route-agent/rear.json") || h2.ranPrefix("systemctl disable") {
		t.Fatal("a rejected switch must leave the previous rear running")
	}
}

// 7. Unit text byte-exact.
func TestUnitContent(t *testing.T) {
	h := newHarness(t, nil)
	sb := "[Unit]\nDescription=Rear sing-box (WARP egress, managed by route-agent)\nAfter=network-online.target\nWants=network-online.target\n\n" +
		"[Service]\nType=simple\nExecStart=/usr/local/bin/sing-box run -c /etc/route-agent/rear.json\nRestart=on-failure\nRestartSec=5\n" +
		"ExecReload=/bin/sh -c \"/usr/local/bin/sing-box check -c /etc/route-agent/rear.json && /bin/kill -HUP $MAINPID\"\n\n[Install]\nWantedBy=multi-user.target\n"
	if got := h.svc.SingboxCore().UnitContent(); got != sb {
		t.Fatalf("sing-box unit:\n%s", got)
	}
	mh := "[Unit]\nDescription=Rear mihomo (WARP egress with rule sets, managed by route-agent)\nAfter=network-online.target\nWants=network-online.target\n\n" +
		"[Service]\nType=simple\nExecStart=/usr/local/bin/mihomo -d /etc/route-agent -f /etc/route-agent/rear.yaml\nRestart=on-failure\nRestartSec=5\n" +
		"ExecReload=/bin/sh -c \"/usr/local/bin/mihomo -t -d /etc/route-agent -f /etc/route-agent/rear.yaml && /bin/kill -HUP $MAINPID\"\n\n[Install]\nWantedBy=multi-user.target\n"
	if got := h.svc.MihomoCore().UnitContent(); got != mh {
		t.Fatalf("mihomo unit:\n%s", got)
	}
	if h.svc.SingboxCore().UnitName() != "route-rear-singbox" || h.svc.MihomoCore().UnitName() != "route-rear-mihomo" {
		t.Fatal("unit names")
	}
	if strings.HasPrefix(DefaultSingboxConfigPath, "/etc/sing-box/") {
		t.Fatal("rear config must stay out of /etc/sing-box")
	}
	if rel, err := filepath.Rel(filepath.Dir(h.svc.MihomoCore().ConfigPath), h.svc.Cfg.RearRuleSetDir); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatal("rule-set dir must lie inside mihomo's home")
	}
}

// 8. Unchanged + active → already current, nothing written, checked or reloaded.
func TestConfigureUnchangedSkips(t *testing.T) {
	for _, core := range []string{"singbox", "mihomo"} {
		h := newHarness(t, nil)
		h.installBinaries()
		c := h.svc.ResolveCore(core)
		h.write(c.UnitPath, c.UnitContent())
		h.write(c.ConfigPath, serialized(t, sbConfig))
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(h.path(c.ConfigPath), old, old)
		r := h.configure(true, core, sbConfig)
		if !r.Success || !r.Running || !strings.Contains(r.Message, "already current") ||
			r.Message != "Rear sing-box configuration already current — nothing to write or reload." {
			t.Fatalf("resp %+v", r)
		}
		st, _ := os.Stat(h.path(c.ConfigPath))
		if !st.ModTime().Equal(old) {
			t.Fatal("config rewritten")
		}
		if h.sbChecks != 0 || h.ranPrefix("systemctl reload") || h.ranPrefix("/usr/local/bin/mihomo") || h.ranPrefix("systemctl daemon-reload") {
			t.Fatalf("calls %v checks %d", h.displays(), h.sbChecks)
		}
	}
}

// 9. Same config but rear inactive → check, write, reload.
func TestConfigureUnchangedButDown(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	c := h.svc.SingboxCore()
	h.write(c.UnitPath, c.UnitContent())
	h.write(c.ConfigPath, serialized(t, sbConfig))
	h.inactive["route-rear-singbox"] = true
	r := h.configure(true, "", sbConfig)
	if !r.Success || strings.Contains(r.Message, "already current") || h.sbChecks != 1 || !h.ran("systemctl reload route-rear-singbox") {
		t.Fatalf("resp %+v calls %v", r, h.displays())
	}
}

// 10. Stale unit → rewrite, daemon-reload, enable --now + restart.
func TestConfigureUnitChangedRestarts(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	c := h.svc.SingboxCore()
	h.write(c.UnitPath, "stale")
	h.write(c.ConfigPath, serialized(t, sbConfig))
	r := h.configure(true, "", sbConfig)
	if !r.Success || strings.Contains(r.Message, "already current") {
		t.Fatalf("resp %+v", r)
	}
	if got, _ := h.read(c.UnitPath); got != c.UnitContent() {
		t.Fatal("unit not rewritten")
	}
	d, e, rs := h.index("systemctl daemon-reload"), h.index("systemctl enable --now route-rear-singbox"), h.index("systemctl restart route-rear-singbox")
	if d < 0 || e < 0 || rs < 0 || !(d < e && e < rs) || h.ranPrefix("systemctl reload") {
		t.Fatalf("calls %v", h.displays())
	}
}

// 11, 14, 16. Normal change → write (2-space JSON, insertion order, 0600), enable --now then reload.
func TestConfigureNormalChange(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	c := h.svc.SingboxCore()
	h.write(c.UnitPath, c.UnitContent())
	h.write(c.ConfigPath, "{}")
	r := h.configure(true, "", sbConfig)
	if !r.Success || !r.Running || r.Message != "Rear sing-box configuration validated, applied and (re)started." {
		t.Fatalf("resp %+v", r)
	}
	want := "{\n  \"log\": {\n    \"level\": \"warn\"\n  },\n  \"experimental\": {\n    \"clash_api\": {\n      \"external_controller\": \"127.0.0.1:29090\",\n      \"secret\": \"s\"\n    }\n  },\n  \"n\": 1.5,\n  \"z\": \"/é\"\n}"
	if got, _ := h.read(c.ConfigPath); got != want {
		t.Fatalf("written:\n%s", got)
	}
	if st, _ := os.Stat(h.path(c.ConfigPath)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	e, rl := h.index("systemctl enable --now route-rear-singbox"), h.index("systemctl reload route-rear-singbox")
	if e < 0 || rl < 0 || e > rl || h.ranPrefix("systemctl restart") {
		t.Fatalf("calls %v", h.displays())
	}
	entries, _ := os.ReadDir(h.path("/etc/route-agent"))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			t.Fatalf("temp file left: %s", en.Name())
		}
	}
}

// 12. Validation failure: check_failed, config untouched, running from is-active.
func TestConfigureCheckFailed(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	c := h.svc.SingboxCore()
	h.write(c.UnitPath, c.UnitContent())
	h.write(c.ConfigPath, "{}")
	long := strings.Repeat("e", 1500)
	h.sbValid = func(any) (bool, string) { return false, long }
	r := h.configure(true, "", sbConfig)
	if r.Success || r.SkippedReason != "check_failed" || !r.Running ||
		r.Message != "Rejected by Node Agent: invalid rear sing-box syntax. Error: "+long {
		t.Fatalf("resp %+v", r)
	}
	if got, _ := h.read(c.ConfigPath); got != "{}" {
		t.Fatal("config touched")
	}
	h.inactive["route-rear-singbox"] = true
	if r = h.configure(true, "", sbConfig); r.Running {
		t.Fatal("running must reflect is-active")
	}
}

// 13. mihomo check command, temp removal and 1000-char truncation.
func TestMihomoValidate(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	var seen runner.Cmd
	var tempContent string
	var tempMode os.FileMode
	h.mihomoT = func(c runner.Cmd) error {
		seen = c
		b, _ := os.ReadFile(h.path(c.Args[4]))
		st, _ := os.Stat(h.path(c.Args[4]))
		tempContent, tempMode = string(b), st.Mode().Perm()
		return errors.New(strings.Repeat("x", 1200))
	}
	ok, msg := h.svc.Validate(context.Background(), h.svc.MihomoCore(), mustParse(t, `{"a":[1]}`))
	if ok || len(msg) != 1000 {
		t.Fatalf("ok=%v len=%d", ok, len(msg))
	}
	if seen.Name != "/usr/local/bin/mihomo" || seen.Args[0] != "-t" || seen.Args[1] != "-d" || seen.Args[2] != "/etc/route-agent" || seen.Args[3] != "-f" ||
		!strings.HasPrefix(seen.Args[4], "/etc/route-agent/.rear.check_") || !strings.HasSuffix(seen.Args[4], ".yaml") || seen.Timeout <= 0 {
		t.Fatalf("cmd %+v", seen)
	}
	if tempContent != "{\n  \"a\": [\n    1\n  ]\n}" || tempMode != 0o600 {
		t.Fatalf("temp %q %v", tempContent, tempMode)
	}
	if h.exists(seen.Args[4]) {
		t.Fatal("temp check file left")
	}
	h.mihomoT = nil
	if ok, _ := h.svc.Validate(context.Background(), h.svc.MihomoCore(), mustParse(t, `{}`)); !ok {
		t.Fatal("valid config rejected")
	}
}

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsjson.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 15. Legacy /etc/sing-box/rear.json removed after a successful write only.
func TestConfigureRemovesLegacyConfig(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	h.write(LegacyConfigPath, "{}")
	h.sbValid = func(any) (bool, string) { return false, "nope" }
	h.configure(true, "", sbConfig)
	if !h.exists(LegacyConfigPath) {
		t.Fatal("legacy removed before a successful write")
	}
	h.sbValid = nil
	if r := h.configure(true, "", sbConfig); !r.Success || h.exists(LegacyConfigPath) {
		t.Fatalf("legacy kept: %+v", r)
	}
	// Configured to live at the legacy path: never removed.
	h2 := newHarness(t, map[string]string{"REAR_SINGBOX_CONFIG_PATH": LegacyConfigPath})
	h2.installBinaries()
	if r := h2.configure(true, "", sbConfig); !r.Success || !h2.exists(LegacyConfigPath) {
		t.Fatalf("own config removed: %+v", r)
	}
}

// 17. systemctl failure during start/reload → Internal Agent Error, config stays written.
func TestConfigureSystemctlFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	c := h.svc.SingboxCore()
	h.write(c.UnitPath, c.UnitContent())
	h.fail["systemctl reload route-rear-singbox"] = "Job failed"
	r := h.configure(true, "", sbConfig)
	if r.Success || r.Running || r.Message != "Internal Agent Error: Command failed: systemctl reload route-rear-singbox\nJob failed" {
		t.Fatalf("resp %+v", r)
	}
	if got, _ := h.read(c.ConfigPath); got != serialized(t, sbConfig) {
		t.Fatal("config must stay written")
	}
}

// ConfigureRearSingbox serializes on locks.Rear (shared with the mihomo/rule-set uploads) and a
// caller whose deadline passes while waiting gets its context status, not an apply.
func TestConfigureWaitsForRearLock(t *testing.T) {
	h := newHarness(t, nil)
	h.installBinaries()
	reg := locks.New()
	h.svc.Locks = reg
	_, unlock, err := reg.Lock(context.Background(), locks.Rear)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(mdCtx(secret), 50*time.Millisecond)
	defer cancel()
	_, err = h.svc.ConfigureRearSingbox(ctx, &agentpb.RearSingboxConfigPayload{Enabled: true, ConfigJson: sbConfig})
	if err == nil || !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Fatalf("err %v", err)
	}
	if h.exists("/etc/route-agent/rear.json") {
		t.Fatal("applied after the client gave up")
	}
	unlock()
	if r := h.configure(true, "", sbConfig); !r.Success {
		t.Fatalf("resp %+v", r)
	}
}
