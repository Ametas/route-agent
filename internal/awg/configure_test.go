package awg

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/server/auth"
)

func fullPayload() *agentpb.AwgConfigPayload {
	return &agentpb.AwgConfigPayload{
		Enabled: true, Port: 51820,
		ServerPrivateKey: " PRIV\r\n", ServerPublicKey: "ignored",
		AddressV4: "100.64.0.1/16", AddressV6: "fd00:a001::1/64",
		Jc: 4, Jmin: 40, Jmax: 70,
		S1: "0", S2: "  ", S3: "15", S4: "1\n7",
		H1: "1", H2: "2", H3: "3", H4: "4",
		I1: "<b 0x01>", I2: "", I3: "\r\n", I4: " i4 ", I5: "i5",
		HeaderProtectionKey: "hpk",
		RandomTrailers:      "ON", DisableCookies: "0",
		Ipv6Mode: "dual-stack",
		Peers: []*agentpb.AwgPeer{
			{PublicKey: "pk1", PresharedKey: "psk1", AllowedIps: "100.64.0.15/32, fd00:a001::15/128"},
			nil,
			{PublicKey: " \n "},
			{PublicKey: "pk2", AllowedIps: "100.64.0.16/32"},
		},
	}
}

// Checklist items 3, 5, 6: exact directive order, sanitizing, S/H raw check vs I sanitized check,
// peers after a blank line without Endpoint/keepalive.
func TestRenderConfigGolden(t *testing.T) {
	want := "[Interface]\n" +
		"PrivateKey = PRIV\n" +
		"ListenPort = 51820\n" +
		"Address = 100.64.0.1/16, fd00:a001::1/64\n" +
		"Jc = 4\nJmin = 40\nJmax = 70\n" +
		"S1 = 0\nS2 = \nS3 = 15\nS4 = 17\n" +
		"H1 = 1\nH2 = 2\nH3 = 3\nH4 = 4\n" +
		"I1 = <b 0x01>\nI4 = i4\nI5 = i5\n" +
		"HeaderProtectionKey = hpk\n" +
		"RandomTrailers = on\n" +
		"DisableCookies = off\n" +
		"\n[Peer]\nPublicKey = pk1\nPresharedKey = psk1\nAllowedIPs = 100.64.0.15/32, fd00:a001::15/128\n" +
		"\n[Peer]\nPublicKey = pk2\nAllowedIPs = 100.64.0.16/32\n"
	if got := RenderConfig(fullPayload()); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// Item 2 + 7: omitted jc/jmin/jmax are written as 0; port 0 → no ListenPort.
func TestRenderConfigDefaults(t *testing.T) {
	got := RenderConfig(&agentpb.AwgConfigPayload{Enabled: true})
	if got != "[Interface]\nJc = 0\nJmin = 0\nJmax = 0\n" {
		t.Fatalf("%q", got)
	}
}

// Item 4.
func TestIpv6Mode(t *testing.T) {
	for mode, want := range map[string]string{
		"ipv4-only": "100.64.0.1/16", "": "100.64.0.1/16, fd00::1/64",
		"dual-stack": "100.64.0.1/16, fd00::1/64", "trap-ipv6": "100.64.0.1/16, fd00::1/64",
	} {
		got := AwgAddress(&agentpb.AwgConfigPayload{AddressV4: "100.64.0.1/16", AddressV6: "fd00::1/64", Ipv6Mode: mode})
		if got != want {
			t.Errorf("mode %q: %q", mode, got)
		}
	}
	if got := AwgAddress(&agentpb.AwgConfigPayload{AddressV6: "fd00::1/64", Ipv6Mode: "ipv4-only"}); got != "" {
		t.Errorf("v6 only + ipv4-only: %q", got)
	}
}

// Item 5 (tests/awgBoolDirective.test.ts).
func TestBoolDirective(t *testing.T) {
	cases := map[string]string{
		"on": "X = on\n", "1": "X = on\n", " ON ": "X = on\n",
		"off": "X = off\n", "0": "X = off\n", "OFF": "X = off\n",
		"": "", "yes": "", "on\nPostUp = curl evil": "", "true": "",
	}
	for in, want := range cases {
		if got := BoolDirective("X", in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize(" a\r\nb \n"); got != "ab" {
		t.Fatalf("%q", got)
	}
	if got := Sanitize(" x "); got != "x" {
		t.Fatalf("JS trim: %q", got)
	}
}

func TestInterfaceName(t *testing.T) {
	for in, want := range map[string]string{
		"": "awg0", "/etc/amnezia/amneziawg/awg0.conf": "awg0", "/x/wg7.conf": "wg7",
		"/x/wg7": "wg7", "/x/a.conf.conf": "a.conf",
	} {
		if got := InterfaceName(in, "awg0"); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// Item 1: metadata only, OK status, exact message, nothing touched.
func TestConfigureAwgUnauthorized(t *testing.T) {
	e := newEnv(t)
	for _, ctx := range []context.Context{context.Background(), metadataCtx("wrong")} {
		resp, err := e.svc.ConfigureAwg(ctx, fullPayload())
		if err != nil || resp.Success || resp.Message != "Invalid orchestrator secret token." {
			t.Fatalf("%v %v", resp, err)
		}
	}
	if len(e.calls()) != 0 {
		t.Fatalf("commands ran: %v", e.calls())
	}
	if _, err := os.Stat(e.fs.Path(DefaultConfigPath)); err == nil {
		t.Fatal("config written without auth")
	}
	_ = auth.InvalidSecretMessage
}

// Items 7, 9 (missing interface → start), file mode 0600, the full command sequence.
func TestConfigureAwgFreshStart(t *testing.T) {
	e := newEnv(t)
	e.sc.fail("ip -o addr show dev awg0", `Device "awg0" does not exist.`)
	e.sc.fail("iptables -C FORWARD -i awg0 -j ACCEPT", "Bad rule")
	e.sc.ok("ip route show default", "default via 203.0.113.1 dev ens3 proto dhcp src 203.0.113.10 metric 100\n")
	e.sc.fail("iptables -t nat -C POSTROUTING -o ens3 -j MASQUERADE", "Bad rule")
	req := fullPayload()
	req.Ipv6Mode = "trap-ipv6"
	resp, _ := e.svc.ConfigureAwg(authed(), req)
	if !resp.Success || resp.Message != "AmneziaWG (AWG3) interface awg0 configured and active on port 51820." {
		t.Fatalf("%+v", resp)
	}
	want := []string{
		"sysctl -w net.ipv4.ip_forward=1",
		"sysctl -w net.ipv6.conf.all.forwarding=1",
		"ip -o addr show dev awg0",
		"systemctl start route-awg@awg0",
		"systemctl enable route-awg@awg0",
		"iptables -C FORWARD -i awg0 -j ACCEPT",
		"iptables -A FORWARD -i awg0 -j ACCEPT",
		"ip route show default",
		"iptables -t nat -C POSTROUTING -o ens3 -j MASQUERADE",
		"iptables -t nat -A POSTROUTING -o ens3 -j MASQUERADE",
		"ip6tables -C FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited",
		"sudo ufw allow 51820/udp",
		"sudo ufw reload",
	}
	if got := e.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	if e.read(DefaultConfigPath) != RenderConfig(req) {
		t.Fatal("file content")
	}
	st, _ := os.Stat(e.fs.Path(DefaultConfigPath))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

// Item 7: port 0 → message still says 51820, no ufw; the message always says awg0.
func TestConfigureAwgPortZeroAndIfaceName(t *testing.T) {
	e := newEnv(t)
	e.cfg.AwgConfigPath = "/etc/amnezia/amneziawg/wg7.conf"
	e.sc.fail("ip -o addr show dev wg7", "no device")
	req := fullPayload()
	req.Port = 0
	req.Ipv6Mode = "ipv4-only"
	resp, _ := e.svc.ConfigureAwg(authed(), req)
	if !resp.Success || resp.Message != "AmneziaWG (AWG3) interface awg0 configured and active on port 51820." {
		t.Fatalf("%+v", resp)
	}
	calls := e.calls()
	if !has(calls, "systemctl start route-awg@wg7") || hasPrefix(calls, "sudo ufw") || hasPrefix(calls, "sysctl -w net.ipv6") {
		t.Fatalf("%v", calls)
	}
	if !has(calls, "iptables -C FORWARD -i awg0 -j ACCEPT") {
		t.Fatal("NAT interface must stay awg0 (spec D R5)")
	}
	if strings.Contains(e.read("/etc/amnezia/amneziawg/wg7.conf"), "ListenPort") {
		t.Fatal("ListenPort written for port 0")
	}
}

func TestConfigureAwgStartFailure(t *testing.T) {
	e := newEnv(t)
	e.sc.fail("ip -o addr show dev awg0", "no device")
	e.sc.fail("systemctl start route-awg@awg0", "Job for route-awg@awg0.service failed.\n")
	resp, _ := e.svc.ConfigureAwg(authed(), fullPayload())
	if resp.Success || resp.Message != "Failed to start AWG service (route-awg@awg0): Job for route-awg@awg0.service failed." {
		t.Fatalf("%+v", resp)
	}
	if hasPrefix(e.calls(), "systemctl enable") {
		t.Fatal("enable after a failed start")
	}
}

// liveUp scripts an existing, active interface with the given addresses.
func liveUp(e *env, iface string, addrs ...string) {
	var b strings.Builder
	for _, a := range addrs {
		fam := "inet"
		if strings.Contains(a, ":") {
			fam = "inet6"
		}
		b.WriteString("7: " + iface + "    " + fam + " " + a + " scope global " + iface + "\\       valid_lft forever preferred_lft forever\n")
	}
	b.WriteString("7: " + iface + "    inet6 fe80::1/64 scope link \\       valid_lft forever preferred_lft forever\n")
	e.sc.ok("ip -o addr show dev "+iface, b.String())
	e.sc.ok("systemctl is-active route-awg@"+iface, "active\n")
}

// The R1 decision: a pure peer change is applied live (strip + syncconf), no restart.
func TestConfigureAwgPeersOnlyIsSyncconf(t *testing.T) {
	e := newEnv(t)
	old := fullPayload()
	old.Peers = old.Peers[:1]
	e.write(DefaultConfigPath, RenderConfig(old))
	liveUp(e, "awg0", "100.64.0.1/16", "fd00:a001::1/64")
	e.sc.ok("/usr/local/bin/awg-quick strip "+DefaultConfigPath, "[Interface]\nPrivateKey = PRIV\n")
	resp, _ := e.svc.ConfigureAwg(authed(), fullPayload())
	if !resp.Success {
		t.Fatalf("%+v", resp)
	}
	calls := e.calls()
	if !has(calls, "/usr/local/bin/awg-quick strip "+DefaultConfigPath) || !hasPrefix(calls, "/usr/local/bin/awg syncconf awg0 "+e.fs.Path("/etc/amnezia/amneziawg/.awg0.syncconf-")) {
		t.Fatalf("no syncconf: %v", calls)
	}
	if hasPrefix(calls, "systemctl restart") || hasPrefix(calls, "systemctl reload") {
		t.Fatalf("peer-only change restarted/reloaded: %v", calls)
	}
	// The stripped temp file (private key) is gone.
	ents, _ := os.ReadDir(e.fs.Path("/etc/amnezia/amneziawg"))
	for _, en := range ents {
		if strings.Contains(en.Name(), "syncconf") {
			t.Fatalf("temp left behind: %s", en.Name())
		}
	}
}

// The R1 decision, the part the brief requires: an Address change STILL applies (restart),
// both when the file says so and when only the live interface disagrees.
func TestConfigureAwgAddressChangeRestarts(t *testing.T) {
	t.Run("file changed", func(t *testing.T) {
		e := newEnv(t)
		old := fullPayload()
		old.AddressV6 = "fd00:aaaa::1/64"
		e.write(DefaultConfigPath, RenderConfig(old))
		liveUp(e, "awg0", "100.64.0.1/16", "fd00:aaaa::1/64")
		resp, _ := e.svc.ConfigureAwg(authed(), fullPayload())
		calls := e.calls()
		if !resp.Success || !has(calls, "systemctl restart route-awg@awg0") || hasPrefix(calls, "/usr/local/bin/awg syncconf") {
			t.Fatalf("%+v %v", resp, calls)
		}
	})
	t.Run("live diverged", func(t *testing.T) {
		e := newEnv(t)
		e.write(DefaultConfigPath, RenderConfig(fullPayload()))
		liveUp(e, "awg0", "100.64.0.9/16", "fd00:a001::1/64")
		_, _ = e.svc.ConfigureAwg(authed(), fullPayload())
		if !has(e.calls(), "systemctl restart route-awg@awg0") {
			t.Fatalf("%v", e.calls())
		}
	})
	t.Run("no previous file", func(t *testing.T) {
		e := newEnv(t)
		liveUp(e, "awg0", "100.64.0.1/16", "fd00:a001::1/64")
		_, _ = e.svc.ConfigureAwg(authed(), fullPayload())
		if !has(e.calls(), "systemctl restart route-awg@awg0") {
			t.Fatalf("%v", e.calls())
		}
	})
	t.Run("mimicry changed", func(t *testing.T) {
		e := newEnv(t)
		old := fullPayload()
		old.H1 = "999"
		e.write(DefaultConfigPath, RenderConfig(old))
		liveUp(e, "awg0", "100.64.0.1/16", "fd00:a001::1/64")
		_, _ = e.svc.ConfigureAwg(authed(), fullPayload())
		if !has(e.calls(), "systemctl restart route-awg@awg0") {
			t.Fatalf("%v", e.calls())
		}
	})
	t.Run("unit inactive", func(t *testing.T) {
		e := newEnv(t)
		e.write(DefaultConfigPath, RenderConfig(fullPayload()))
		liveUp(e, "awg0", "100.64.0.1/16", "fd00:a001::1/64")
		e.sc.ok("systemctl is-active route-awg@awg0", "inactive\n")
		_, _ = e.svc.ConfigureAwg(authed(), fullPayload())
		if !has(e.calls(), "systemctl restart route-awg@awg0") {
			t.Fatalf("%v", e.calls())
		}
	})
}

// Item 9: syncconf (the old "reload") failure falls back to restart; restart failure message.
func TestConfigureAwgSyncconfFallback(t *testing.T) {
	e := newEnv(t)
	e.write(DefaultConfigPath, RenderConfig(fullPayload()))
	liveUp(e, "awg0", "100.64.0.1/16", "fd00:a001::1/64")
	e.sc.failPrefix("/usr/local/bin/awg syncconf", "Unable to modify interface")
	e.sc.fail("systemctl restart route-awg@awg0", "restart boom\n")
	resp, _ := e.svc.ConfigureAwg(authed(), fullPayload())
	if resp.Success || resp.Message != "Failed to reload or restart AWG service (route-awg@awg0): restart boom" {
		t.Fatalf("%+v", resp)
	}
	calls := e.calls()
	if index(calls, "systemctl restart route-awg@awg0") < 0 || hasPrefix(calls, "systemctl enable") {
		t.Fatalf("%v", calls)
	}
}

// Item 10.
func TestConfigureAwgWriteError(t *testing.T) {
	e := newEnv(t)
	e.write("/etc/amnezia/amneziawg", "a file where the directory should be")
	resp, _ := e.svc.ConfigureAwg(authed(), fullPayload())
	if resp.Success || !strings.HasPrefix(resp.Message, "AmneziaWG configuration error: ") {
		t.Fatalf("%+v", resp)
	}
}

// Item 8 + architecture §6 item 11: disable stops (chain), disables the unit, leaves the marker,
// closes the port, keeps the file; the health loop then reports inactive and never restarts.
func TestConfigureAwgDisableStaysDisabled(t *testing.T) {
	e := newEnv(t)
	e.write(DefaultConfigPath, "[Interface]\n")
	e.sc.fail("systemctl stop route-awg@awg0", "not loaded")
	e.sc.fail("systemctl stop awg-quick@awg0", "not loaded")
	resp, _ := e.svc.ConfigureAwg(authed(), &agentpb.AwgConfigPayload{Enabled: false, Port: 51820})
	if !resp.Success || resp.Message != "AmneziaWG (AWG3) service successfully disabled and stopped." {
		t.Fatalf("%+v", resp)
	}
	want := []string{
		"systemctl stop route-awg@awg0",
		"systemctl stop awg-quick@awg0",
		"awg-quick down awg0",
		"systemctl disable route-awg@awg0",
		"sudo ufw delete allow 51820/udp",
		"sudo ufw reload",
	}
	if got := e.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	if e.read(DefaultConfigPath) != "[Interface]\n" {
		t.Fatal("config file must be kept")
	}

	h := &Health{Runner: e.run, FS: e.fs, Systemctl: e.svc.Systemctl, Cfg: e.cfg, Log: quietLog()}
	e.sc.fail("ip link show awg0", "no device")
	n := len(e.calls())
	h.Check(context.Background())
	if h.Status() != "inactive" || hasPrefix(e.calls()[n:], "systemctl restart") {
		t.Fatalf("status %q calls %v", h.Status(), e.calls()[n:])
	}

	// Enabling again removes the marker.
	e.sc.fail("ip -o addr show dev awg0", "no device")
	resp, _ = e.svc.ConfigureAwg(authed(), fullPayload())
	if !resp.Success {
		t.Fatalf("%+v", resp)
	}
	if _, err := os.Stat(e.fs.Path(DisabledMarker("awg0"))); !os.IsNotExist(err) {
		t.Fatal("marker survived enable")
	}
}

// The first stop that succeeds ends the chain; no port → no ufw.
func TestConfigureAwgDisableShortChain(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.svc.ConfigureAwg(authed(), &agentpb.AwgConfigPayload{})
	if !resp.Success {
		t.Fatal(resp)
	}
	want := "systemctl stop route-awg@awg0\nsystemctl disable route-awg@awg0"
	if got := strings.Join(e.calls(), "\n"); got != want {
		t.Fatalf("%s", got)
	}
}
