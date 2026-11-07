package telemetry

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
)

type frameResult struct {
	f    *agentpb.TelemetryResponse
	next CPUStats
}

// tests/app.test.ts:1995-2002 (checklist 13).
func TestParseAwgVersionString(t *testing.T) {
	for in, want := range map[string]string{
		"awg v1.0.20260618-2\n":      "1.0.20260618-2",
		"v1.0.0":                     "1.0.0",
		"v3.0.2":                     "3.0.2",
		"0.0.20250522":               "0.0.20250522",
		"1.0.20260618-2":             "1.0.20260618-2",
		"amneziawg-go v3.0.2":        "3.0.2",
		"amneziawg-go v0.0.20230223": "0.0.20230223",
		"no_version_here":            "",
		"":                           "",
		"amneziawg-tools v1.0.2025.": "1.0.2025",
		"wireguard-tools v1.0.20210914 - https://git.zx2c4.com/wireguard-tools/": "1.0.20210914",
	} {
		if got := ParseAwgVersionString(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestAwgToolsVersionCandidates(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Configured path first, then the two fixed ones, then `awg` from PATH; failures and
	// unparsable outputs fall through.
	r.ans.ok("/usr/local/bin/awg --version", "no_version_here")
	r.ans.ok("awg --version", "amneziawg-tools v1.0.20250901")
	if v := AwgToolsVersion(ctx, r.fake, "/opt/awg"); v != "1.0.20250901" {
		t.Fatal(v)
	}
	want := []string{"/opt/awg --version", "/usr/local/bin/awg --version", "/usr/bin/awg --version", "awg --version"}
	if got := r.fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for _, c := range r.fake.Calls() {
		if c.Shell || c.Timeout != versionTimeout {
			t.Fatalf("%+v", c)
		}
	}
	// Default path is not tried twice; stderr is read when stdout is empty.
	r = newRig(t)
	r.ans.mu.Lock()
	r.ans.out["/usr/local/bin/awg --version"] = runner.FakeResult{Result: runner.Result{Stderr: "awg v1.0.20260618-2\n"}}
	r.ans.mu.Unlock()
	if v := AwgToolsVersion(ctx, r.fake, ""); v != "1.0.20260618-2" || len(r.fake.Calls()) != 1 {
		t.Fatal(v, r.fake.Displays())
	}
	// A legacy awg → amneziawg-go symlink answers not_installed at once (case-insensitive).
	r = newRig(t)
	r.ans.ok("/usr/local/bin/awg --version", "AmneziaWG-Go v0.2.12")
	if v := AwgToolsVersion(ctx, r.fake, ""); v != NotInstalled || len(r.fake.Calls()) != 1 {
		t.Fatal(v, r.fake.Displays())
	}
	// Nothing works.
	r = newRig(t)
	if v := AwgToolsVersion(ctx, r.fake, ""); v != NotInstalled {
		t.Fatal(v)
	}
}

func TestAwgGoVersion(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.ans.ok("/opt/go --version", "amneziawg-go v0.2.12.\n")
	if v := AwgGoVersion(ctx, r.fake, "/opt/go"); v != "0.2.12." { // no trailing strip on this branch
		t.Fatal(v)
	}
	r = newRig(t)
	r.ans.ok("/usr/local/bin/amneziawg-go --version", "version 0.0.20250522\n")
	if v := AwgGoVersion(ctx, r.fake, "/opt/go"); v != "0.0.20250522" {
		t.Fatal(v)
	}
	if got := r.fake.Displays(); !reflect.DeepEqual(got, []string{"/opt/go --version", "/usr/local/bin/amneziawg-go --version"}) {
		t.Fatal(got)
	}
	r = newRig(t)
	if v := AwgGoVersion(ctx, r.fake, ""); v != NotInstalled || len(r.fake.Calls()) != 1 {
		t.Fatal(v, r.fake.Displays())
	}
}

// Checklist 10.
func TestWebRTCStatus(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	const list = "systemctl list-units --type=service --state=running --no-legend olcrtc-agent-srv@*"
	if s := r.col.WebRTCStatus(ctx); s != WebRTCNotInstalled || len(r.fake.Calls()) != 0 {
		t.Fatal(s)
	}
	writeFile(t, r.fs, "/usr/local/bin/olcrtc-agent-srv", "bin")
	r.ans.ok(list, "")
	if s := r.col.WebRTCStatus(ctx); s != WebRTCNoActiveTunnels {
		t.Fatal(s)
	}
	r.ans.ok(list, "  olcrtc-agent-srv@a.service loaded active running Olcrtc a\n\n   \nolcrtc-agent-srv@b.service loaded active running b\n")
	if s := r.col.WebRTCStatus(ctx); s != WebRTCNominal {
		t.Fatal(s)
	}
	// systemctl failing behaved like an empty pipeline in TS (awk's exit status): no tunnels.
	r.ans.fail(list)
	if s := r.col.WebRTCStatus(ctx); s != WebRTCNoActiveTunnels {
		t.Fatal(s)
	}
	// Killed by its timeout: unavailable.
	r.fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		return runner.Result{}, &runner.Error{Cmd: c.Display(), Killed: true, Code: -1, Signal: "SIGTERM"}
	}
	if s := r.col.WebRTCStatus(ctx); s != WebRTCUnavailable {
		t.Fatal(s)
	}
	if c := r.fake.Calls()[0]; c.Shell || c.Timeout <= 0 {
		t.Fatalf("%+v", c)
	}
}

// A whole frame: every field, the AWG alias, mesh peers == len(keys), the caches.
func TestFrameFields(t *testing.T) {
	r := newRig(t)
	now := r.clk.now().Unix()
	writeFile(t, r.fs, "/proc/stat", "cpu  100 0 50 800 40 0 10 0 0 0\n")
	writeFile(t, r.fs, "/proc/meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
	writeFile(t, r.fs, "/proc/net/tcp", "h\na\nb\n")
	writeFile(t, r.fs, "/proc/net/udp", "h\nc\n")
	writeFile(t, r.fs, "/proc/net/route", routeText)
	writeFile(t, r.fs, "/proc/net/dev", devText)
	writeFile(t, r.fs, "/etc/systemd/system/sing-box.service", "[Unit]\n")
	writeFile(t, r.fs, "/sys/class/net/awgmesh0/mtu", "1420\n")
	r.ans.ok("/usr/local/bin/sing-box version", "sing-box version 1.14.0-hotusers\n\nEnvironment: go1.24\n")
	r.ans.ok("/usr/local/bin/awg --version", "awg v1.0.20260618-2\n")
	r.ans.ok("/usr/local/bin/amneziawg-go --version", "amneziawg-go v0.2.12\n")
	r.ans.ok("awg show awg0 dump", "PRIV\tPUB\t51820\toff\n"+
		"p1\t(none)\t1.1.1.1:1\t10.0.0.2/32\t"+strconv.FormatInt(now-180, 10)+"\t1\t2\toff\n"+
		"p2\t(none)\t1.1.1.1:2\t10.0.0.3/32\t"+strconv.FormatInt(now-181, 10)+"\t1\t2\toff\n"+
		"p3\t(none)\t(none)\t10.0.0.4/32\t0\t0\t0\toff\n")
	r.ans.ok("awg show awgmesh0 dump", "PRIV\tPUB\t51821\toff\n"+
		"k1=\t(none)\t2.2.2.2:1\t100.64.0.2/32\t"+strconv.FormatInt(now-10, 10)+"\t1\t2\t25\n"+
		"k2=\t(none)\t2.2.2.3:1\t100.64.0.3/32\t"+strconv.FormatInt(now-500, 10)+"\t1\t2\t25\n"+
		"k3=\t(none)\t2.2.2.4:1\t100.64.0.4/32\t"+strconv.FormatInt(now-1, 10)+"\t1\t2\t25\n")
	r.ans.ok("modinfo amneziawg", "filename: x\nversion:        1.0.20250901\n")
	r.ans.ok("lsmod", "Module Size Used by\namneziawg 1 0\n")

	f := r.frame(t).f
	want := &agentpb.TelemetryResponse{
		CpuUsage: (1000.0 - 800) / 1000 * 100, MemUsage: 50, ActiveConnections: 3,
		WebrtcStatus: WebRTCNotInstalled, SingboxVersion: "1.14.0-hotusers", AwgActivePeers: 1,
		AwgVersion: "1.0.20260618-2", AwgStatus: "nominal", AwgToolsVersion: "1.0.20260618-2", AwgGoVersion: "0.2.12",
		MeshAwgKernelStatus: "loaded", MeshAwgKernelVersion: "1.0.20250901",
		MeshTunnelInterfaceStatus: "up", MeshTunnelActivePeers: 2, MeshTunnelLivePeerKeys: []string{"k1=", "k3="},
		HostNetIface: "eth0", HostNetRxBytes: 98765432100, HostNetTxBytes: 12345678900,
	}
	if !proto.Equal(f, want) {
		t.Fatalf("\n got %v\nwant %v", f, want)
	}
	// No `ip link show` per frame: the mesh interface is looked up in sysfs.
	for _, d := range r.fake.Displays() {
		if d == "ip link show awgmesh0" {
			t.Fatal("ip link spawned")
		}
	}

	// Version caches: 60 s; sing-box and AWG (tools+go share one timestamp) refresh together.
	_ = r.frame(t)
	if r.ans.count("/usr/local/bin/sing-box version") != 1 || r.ans.count("/usr/local/bin/awg --version") != 1 ||
		r.ans.count("/usr/local/bin/amneziawg-go --version") != 1 || r.ans.count("modinfo amneziawg") != 1 {
		t.Fatal("cache ignored")
	}
	r.clk.add(61 * time.Second)
	_ = r.frame(t)
	if r.ans.count("/usr/local/bin/sing-box version") != 2 || r.ans.count("/usr/local/bin/awg --version") != 2 ||
		r.ans.count("/usr/local/bin/amneziawg-go --version") != 2 || r.ans.count("modinfo amneziawg") != 2 {
		t.Fatal("TTL not applied")
	}
	// Invalidation (binary swaps): the next frame re-reads.
	r.ans.ok("/usr/local/bin/sing-box version", "sing-box version 1.15.0-beta.1\n")
	r.ans.ok("/usr/local/bin/awg --version", "awg v1.0.20270101\n")
	r.col.InvalidateSingboxVersion()
	r.col.InvalidateAwgVersions()
	r.col.Mesh.InvalidateKernel()
	f = r.frame(t).f
	if f.SingboxVersion != "1.15.0-beta.1" || f.AwgToolsVersion != "1.0.20270101" || f.AwgVersion != f.AwgToolsVersion ||
		r.ans.count("modinfo amneziawg") != 3 {
		t.Fatalf("%v", f)
	}

	// Unit file gone: not_installed even with a working binary (after the cache expires).
	_ = os.Remove(r.fs.Path("/etc/systemd/system/sing-box.service"))
	r.clk.add(61 * time.Second)
	if f = r.frame(t).f; f.SingboxVersion != NotInstalled {
		t.Fatal(f.SingboxVersion)
	}
	// Mesh interface gone; kernel module missing.
	_ = os.RemoveAll(r.fs.Path("/sys/class/net/awgmesh0"))
	r.ans.fail("modinfo amneziawg")
	r.col.Mesh.InvalidateKernel()
	f = r.frame(t).f
	if f.MeshTunnelInterfaceStatus != "absent" || f.MeshTunnelActivePeers != 0 || len(f.MeshTunnelLivePeerKeys) != 0 ||
		f.MeshAwgKernelStatus != "not_installed" || f.MeshAwgKernelVersion != "" {
		t.Fatalf("%v", f)
	}
}

// A source that hangs: the caller gets the fallback within its budget, a concurrent caller joins
// the same probe, the late result serves the next frames.
func TestSourceBudgetAndDedupe(t *testing.T) {
	var s source[string]
	release := make(chan struct{})
	var probes sync.WaitGroup
	starts := 0
	var mu sync.Mutex
	probe := func(context.Context) string {
		mu.Lock()
		starts++
		mu.Unlock()
		probes.Done()
		<-release
		return "late"
	}
	probes.Add(1)
	t0 := time.Now()
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = s.get(context.Background(), 50*time.Millisecond, "fb", probe) }()
	}
	wg.Wait()
	if el := time.Since(t0); el > time.Second {
		t.Fatal(el)
	}
	mu.Lock()
	n := starts
	mu.Unlock()
	if !reflect.DeepEqual(results, []string{"fb", "fb", "fb"}) || n != 1 {
		t.Fatal(results, n)
	}
	close(release)
	probes.Wait()
	// Wait for the probe to land.
	for i := 0; i < 100 && s.get(context.Background(), time.Millisecond, "fb", func(context.Context) string { return "late" }) != "late"; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	// A later hang answers the last value, not the fallback.
	block := make(chan struct{})
	defer close(block)
	if v := s.get(context.Background(), 20*time.Millisecond, "fb", func(context.Context) string { <-block; return "x" }); v != "late" {
		t.Fatal(v)
	}
	// forget drops it.
	s.forget()
	if v := s.get(context.Background(), 5*time.Millisecond, "fb", func(context.Context) string { <-block; return "x" }); v != "fb" {
		t.Fatal(v)
	}
}

// A cold-cache frame with a hung version probe still returns within the budget, with "unknown"
// for the value no probe has produced yet (never not_installed, which would trigger a repair).
func TestFrameBudgetColdCache(t *testing.T) {
	r := newRig(t)
	writeFile(t, r.fs, "/etc/systemd/system/sing-box.service", "x")
	r.ans.ok("/usr/local/bin/sing-box version", "sing-box version 1.13.0\n")
	r.ans.slow("/usr/local/bin/sing-box version", 400*time.Millisecond)
	r.ans.slow("modinfo amneziawg", 400*time.Millisecond)
	r.ans.slow("awg show awg0 dump", 400*time.Millisecond)
	t0 := time.Now()
	f, _ := r.col.Frame(context.Background(), CPUStats{}, 50*time.Millisecond)
	if el := time.Since(t0); el > 300*time.Millisecond {
		t.Fatal(el)
	}
	if f.SingboxVersion != "unknown" || f.MeshAwgKernelStatus != "unknown" || f.AwgActivePeers != 0 {
		t.Fatalf("%v", f)
	}
	time.Sleep(500 * time.Millisecond)
	f, _ = r.col.Frame(context.Background(), CPUStats{}, 50*time.Millisecond)
	if f.SingboxVersion != "1.13.0" || f.MeshAwgKernelStatus != "not_installed" {
		t.Fatalf("%v", f)
	}
}

func TestSysfsLinkExists(t *testing.T) {
	r := newRig(t)
	exists := SysfsLinkExists(r.fs)
	writeFile(t, r.fs, "/sys/class/net/eth0/mtu", "1500")
	if !exists("eth0") || exists("eth1") || exists("") || exists("../net") || exists("..") {
		t.Fatal("sysfs lookup")
	}
}
