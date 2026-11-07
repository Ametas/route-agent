package mesh

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/firewall"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const secret = "0123456789abcdef0123456789abcdef"

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mdCtx(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// answers maps a command Display() (exact, else prefix ending in "*") to stdout or a failure.
type answers struct {
	mu sync.Mutex
	m  map[string]runner.FakeResult
}

func (a *answers) set(display, stdout string, failStderr ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]runner.FakeResult{}
	}
	r := runner.FakeResult{Result: runner.Result{Stdout: stdout}}
	if len(failStderr) > 0 {
		r.Err = &runner.Error{Cmd: display, Stderr: failStderr[0], Code: 1}
		r.Result.ExitCode = 1
	}
	a.m[display] = r
}

func (a *answers) handle(c runner.Cmd) (runner.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.m[c.Display()]; ok {
		return r.Result, r.Err
	}
	for k, r := range a.m {
		if strings.HasSuffix(k, "*") && strings.HasPrefix(c.Display(), strings.TrimSuffix(k, "*")) {
			return r.Result, r.Err
		}
	}
	return runner.Result{}, nil
}

type env struct {
	t     *testing.T
	fs    fsx.FS
	run   *runner.Fake
	ans   *answers
	svc   *Service
	cfg   *config.Config
	ufwOK bool
}

func newEnv(t *testing.T) *env {
	root := t.TempDir()
	a := &answers{}
	fake := &runner.Fake{Handler: a.handle}
	cfg := &config.Config{MeshAwgConfigPath: DefaultConfigPath, AwgToolsBinaryPath: "/usr/local/bin/awg"}
	e := &env{t: t, fs: fsx.FS{Root: root}, run: fake, ans: a, cfg: cfg, ufwOK: true}
	fw := &firewall.Service{Auth: auth.New(secret), Runner: fake, Log: quietLog(), LookPath: func(string) (string, error) {
		if e.ufwOK {
			return "/usr/sbin/ufw", nil
		}
		return "", os.ErrNotExist
	}}
	e.svc = &Service{Auth: auth.New(secret), Runner: fake, FS: e.fs, Systemctl: systemd.Systemctl{Runner: fake},
		Cfg: cfg, Log: quietLog(), Firewall: fw}
	return e
}

func (e *env) write(p, s string) {
	full := e.fs.Path(p)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(p string) string {
	b, err := os.ReadFile(e.fs.Path(p))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func has(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func hasPrefix(calls []string, p string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// The tests/app.test.ts:1196-1244 payload.
func appPayload() *agentpb.MeshTunnelConfigPayload {
	return &agentpb.MeshTunnelConfigPayload{
		PrivateKey: "meshprivkey123", AddressV4: "100.100.0.4/16", AddressV6: "fd00:a002::4/64",
		ListenPort: 51821, Jc: 4, Jmin: 40, Jmax: 70,
		S1: "113", S2: "87", S3: "0", S4: "0",
		H1: "1234567890", H2: "2345678901", H3: "3456789012", H4: "4567890123",
		HeaderProtectionKey: "test-header-protection-key",
		I1:                  "i1-junk-payload", I2: "i2-junk-payload",
		Peers: []*agentpb.MeshTunnelPeer{
			{PublicKey: "frontpubkey1", AllowedIps: "100.100.0.1/32", Endpoint: "203.0.113.1:51821", PersistentKeepalive: 25},
			{PublicKey: "frontpubkey2", AllowedIps: "100.100.0.2/32", Endpoint: "203.0.113.2:51821", PersistentKeepalive: 25},
		},
	}
}

func TestRenderGolden(t *testing.T) {
	req := appPayload()
	req.Mtu = 1380
	req.Peers = append(req.Peers, nil, &agentpb.MeshTunnelPeer{PublicKey: "\r\n"},
		&agentpb.MeshTunnelPeer{PublicKey: "egress1", AllowedIps: "100.100.0.9/32, fd00:a002::9/128"})
	want := "[Interface]\nPrivateKey = meshprivkey123\nListenPort = 51821\nAddress = 100.100.0.4/16, fd00:a002::4/64\nMTU = 1380\n" +
		"Jc = 4\nJmin = 40\nJmax = 70\nS1 = 113\nS2 = 87\nS3 = 0\nS4 = 0\n" +
		"H1 = 1234567890\nH2 = 2345678901\nH3 = 3456789012\nH4 = 4567890123\n" +
		"I1 = i1-junk-payload\nI2 = i2-junk-payload\nHeaderProtectionKey = test-header-protection-key\n" +
		"\n[Peer]\nPublicKey = frontpubkey1\nAllowedIPs = 100.100.0.1/32\nEndpoint = 203.0.113.1:51821\nPersistentKeepalive = 25\n" +
		"\n[Peer]\nPublicKey = frontpubkey2\nAllowedIPs = 100.100.0.2/32\nEndpoint = 203.0.113.2:51821\nPersistentKeepalive = 25\n" +
		"\n[Peer]\nPublicKey = egress1\nAllowedIPs = 100.100.0.9/32, fd00:a002::9/128\n"
	got, n := RenderConfig(req)
	if got != want || n != 3 {
		t.Fatalf("n=%d\n%q\nwant\n%q", n, got, want)
	}
	// Item 27: no MTU line for 0; no RandomTrailers/DisableCookies on the mesh.
	got, _ = RenderConfig(&agentpb.MeshTunnelConfigPayload{})
	if got != "[Interface]\nJc = 0\nJmin = 0\nJmax = 0\n" {
		t.Fatalf("%q", got)
	}
}

func TestConfigureUnauthorized(t *testing.T) {
	e := newEnv(t)
	// The payload secret is NOT honoured (metadata only).
	req := appPayload()
	req.OrchestratorSecret = secret
	r, err := e.svc.ConfigureMeshTunnel(mdCtx("bad_secret"), req)
	if err != nil || r.Success || r.Message != "Invalid orchestrator secret token." || len(e.run.Calls()) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Items 29-31: fresh interface → port allowed first, restart, enable; 0600 file.
func TestConfigureFresh(t *testing.T) {
	e := newEnv(t)
	e.ans.set("ip -o addr show dev awgmesh0", "", `Device "awgmesh0" does not exist.`)
	r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), appPayload())
	if !r.Success || r.Message != "Mesh AWG tunnel interface awgmesh0 configured with 2 peer(s)" {
		t.Fatalf("%+v", r)
	}
	want := []string{"ufw allow 51821/udp", "ip -o addr show dev awgmesh0", "systemctl restart route-awg@awgmesh0", "systemctl enable route-awg@awgmesh0"}
	if got := e.run.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	st, _ := os.Stat(e.fs.Path(DefaultConfigPath))
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	got, _ := RenderConfig(appPayload())
	if e.read(DefaultConfigPath) != got {
		t.Fatal("content")
	}
}

func TestConfigureNoUfwAndBadPort(t *testing.T) {
	e := newEnv(t)
	e.ufwOK = false
	_, _ = e.svc.ConfigureMeshTunnel(mdCtx(secret), appPayload())
	if hasPrefix(e.run.Displays(), "ufw") {
		t.Fatal("ufw without ufw")
	}
	e2 := newEnv(t)
	req := appPayload()
	req.ListenPort = 70000
	_, _ = e2.svc.ConfigureMeshTunnel(mdCtx(secret), req)
	if hasPrefix(e2.run.Displays(), "ufw") {
		t.Fatal("ufw for an out-of-range port")
	}
}

func up(e *env, v4, v6 string) {
	e.ans.set("ip -o addr show dev awgmesh0",
		"9: awgmesh0    inet "+v4+" scope global awgmesh0\\       valid_lft forever\n"+
			"9: awgmesh0    inet6 "+v6+" scope global \\       valid_lft forever\n")
	e.ans.set("systemctl is-active route-awg@awgmesh0", "active\n")
	e.write("/sys/class/net/awgmesh0/mtu", "1380\n")
}

// Item 30: reload (now syncconf) only when the identity is unchanged.
func TestConfigureIdentity(t *testing.T) {
	base := func() *agentpb.MeshTunnelConfigPayload { r := appPayload(); r.Mtu = 1380; return r }
	t.Run("peers only → syncconf", func(t *testing.T) {
		e := newEnv(t)
		old := base()
		old.Peers = old.Peers[:1]
		txt, _ := RenderConfig(old)
		e.write(DefaultConfigPath, txt)
		up(e, "100.100.0.4/16", "fd00:a002::4/64")
		r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), base())
		c := e.run.Displays()
		if !r.Success || !has(c, "/usr/local/bin/awg-quick strip "+DefaultConfigPath) || !hasPrefix(c, "/usr/local/bin/awg syncconf awgmesh0 ") || hasPrefix(c, "systemctl restart") {
			t.Fatalf("%+v %v", r, c)
		}
	})
	cases := map[string]func(e *env, req *agentpb.MeshTunnelConfigPayload){
		"v4 changed":  func(e *env, req *agentpb.MeshTunnelConfigPayload) { up(e, "100.100.0.7/16", "fd00:a002::4/64") },
		"v6 changed":  func(e *env, req *agentpb.MeshTunnelConfigPayload) { up(e, "100.100.0.4/16", "fd00:a002::7/64") },
		"mtu changed": func(e *env, req *agentpb.MeshTunnelConfigPayload) { e.write("/sys/class/net/awgmesh0/mtu", "1420\n") },
		"mtu removed": func(e *env, req *agentpb.MeshTunnelConfigPayload) { req.Mtu = 0 },
		"no v4":       func(e *env, req *agentpb.MeshTunnelConfigPayload) { req.AddressV4 = "" },
		"unit inactive": func(e *env, req *agentpb.MeshTunnelConfigPayload) {
			e.ans.set("systemctl is-active route-awg@awgmesh0", "inactive\n")
		},
	}
	for name, mutate := range cases {
		t.Run(name+" → restart", func(t *testing.T) {
			e := newEnv(t)
			txt, _ := RenderConfig(base())
			e.write(DefaultConfigPath, txt)
			up(e, "100.100.0.4/16", "fd00:a002::4/64")
			req := base()
			mutate(e, req)
			r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), req)
			c := e.run.Displays()
			if !r.Success || !has(c, "systemctl restart route-awg@awgmesh0") || hasPrefix(c, "/usr/local/bin/awg syncconf") {
				t.Fatalf("%+v %v", r, c)
			}
		})
	}
	t.Run("syncconf fails → restart; restart fails → message", func(t *testing.T) {
		e := newEnv(t)
		txt, _ := RenderConfig(base())
		e.write(DefaultConfigPath, txt)
		up(e, "100.100.0.4/16", "fd00:a002::4/64")
		e.ans.set("/usr/local/bin/awg syncconf*", "", "syncconf boom")
		e.ans.set("systemctl restart route-awg@awgmesh0", "", "restart boom\n")
		r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), base())
		if r.Success || r.Message != "Failed to reload or restart mesh tunnel service (route-awg@awgmesh0): restart boom" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("identity restart fails → message", func(t *testing.T) {
		e := newEnv(t)
		e.ans.set("ip -o addr show dev awgmesh0", "", "no device")
		e.ans.set("systemctl restart route-awg@awgmesh0", "", "Job failed.\n")
		r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), base())
		if r.Success || r.Message != "Failed to apply new tunnel identity to mesh service (route-awg@awgmesh0): Job failed." {
			t.Fatalf("%+v", r)
		}
		if hasPrefix(e.run.Displays(), "systemctl enable") {
			t.Fatal("enable after failure")
		}
	})
}

func TestConfigureWriteError(t *testing.T) {
	e := newEnv(t)
	e.write("/etc/amnezia/amneziawg", "file, not dir")
	r, _ := e.svc.ConfigureMeshTunnel(mdCtx(secret), appPayload())
	if r.Success || !strings.HasPrefix(r.Message, "Mesh tunnel configuration error: ") {
		t.Fatalf("%+v", r)
	}
}

// Telemetry readers.
func TestKernelStatus(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_790_000_000, 0)
	s := &Status{Runner: e.run, Cfg: e.cfg, Now: func() time.Time { return now }}

	e.ans.set("modinfo amneziawg", "", "modinfo: ERROR: Module amneziawg not found.")
	if k := s.Kernel(context.Background()); k != (KernelStatus{Status: "not_installed"}) {
		t.Fatalf("%+v", k)
	}
	// Cached for 60 s.
	e.ans.set("modinfo amneziawg", "filename: /lib/modules/x/updates/dkms/amneziawg.ko\nversion:        1.0.20250901\nlicense: GPL\n")
	e.ans.set("lsmod", "Module Size Used by\namneziawg_extra 1 0\n")
	if k := s.Kernel(context.Background()); k.Status != "not_installed" {
		t.Fatal("cache ignored")
	}
	s.InvalidateKernel()
	if k := s.Kernel(context.Background()); k != (KernelStatus{Status: "built_not_loaded", Version: "1.0.20250901"}) {
		t.Fatalf("%+v (grep -w: amneziawg_extra is not the word)", k)
	}
	e.ans.set("lsmod", "Module Size Used by\namneziawg 110592 0\nip6_udp_tunnel 16384 1 amneziawg\n")
	now = now.Add(61 * time.Second)
	if k := s.Kernel(context.Background()); k != (KernelStatus{Status: "loaded", Version: "1.0.20250901"}) {
		t.Fatalf("%+v", k)
	}
	e.ans.set("lsmod", "", "lsmod failed")
	s.InvalidateKernel()
	if k := s.Kernel(context.Background()); k.Status != "built_not_loaded" {
		t.Fatalf("%+v", k)
	}
}

func TestInterfaceStatus(t *testing.T) {
	e := newEnv(t)
	const now = 1_790_400_000
	s := &Status{Runner: e.run, Cfg: e.cfg, Now: func() time.Time { return time.Unix(now, 0) }}
	e.ans.set("ip link show awgmesh0", "", "does not exist")
	if st := s.Interface(context.Background()); st.Status != "absent" || st.ActivePeers != 0 || st.LivePeerKeys == nil || len(st.LivePeerKeys) != 0 {
		t.Fatalf("%+v", st)
	}
	e.ans.set("ip link show awgmesh0", "9: awgmesh0: <POINTOPOINT,UP>")
	e.ans.set("awg show awgmesh0 dump", "", "Unable to access interface")
	if st := s.Interface(context.Background()); st.Status != "up" || st.ActivePeers != 0 || len(st.LivePeerKeys) != 0 {
		t.Fatalf("%+v", st)
	}
	e.ans.set("awg show awgmesh0 dump", "PRIV\tPUB\t51821\toff\n"+
		"live=\t(none)\t1.2.3.4:51821\t100.100.0.5/32\t"+strconv.Itoa(now-10)+"\t1\t2\t25\n"+
		"stale=\t(none)\t1.2.3.5:51821\t100.100.0.6/32\t"+strconv.Itoa(now-181)+"\t1\t2\t25\n")
	st := s.Interface(context.Background())
	if st.Status != "up" || st.ActivePeers != 1 || !reflect.DeepEqual(st.LivePeerKeys, []string{"live="}) {
		t.Fatalf("%+v", st)
	}
	if int(st.ActivePeers) != len(st.LivePeerKeys) {
		t.Fatal("active_peers must equal len(live_peer_keys)")
	}
	// The interface name follows MESH_AWG_CONFIG_PATH.
	e.cfg.MeshAwgConfigPath = "/etc/amnezia/amneziawg/m1.conf"
	_ = s.Interface(context.Background())
	if !has(e.run.Displays(), "ip link show m1") {
		t.Fatal(e.run.Displays())
	}
}
