package telemetry

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/fsx"
)

func writeFile(t *testing.T, fs fsx.FS, p, content string) {
	t.Helper()
	full := fs.Path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Checklist 7: the exact TS formula — every column summed (guest/guest_nice twice), only idle is
// idle (iowait busy), delta against the stream's previous tick, zero baseline = since boot.
func TestCPUUsage(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	stat1 := "cpu  100 0 50 800 40 0 10 0 20 0\ncpu0 1 2 3 4\n"
	u, st := CPUUsage(stat1, nil, CPUStats{})
	if st.Total != 1020 || st.Idle != 800 {
		t.Fatalf("%+v", st)
	}
	if want := (1020.0 - 800) / 1020 * 100; u != want {
		t.Fatalf("since-boot %v want %v", u, want)
	}
	stat2 := "cpu  150 0 70 900 60 0 10 0 40 0\n"
	u, st2 := CPUUsage(stat2, nil, st)
	// Δtotal = 1230-1020 = 210, Δidle = 100 → busy 110 (iowait 20 and guest 20 included).
	if want := (210.0 - 100) / 210 * 100; math.Abs(u-want) > 1e-12 || st2.Total != 1230 {
		t.Fatalf("%v want %v", u, want)
	}
	// No change → 0.
	if u, _ := CPUUsage(stat2, nil, st2); u != 0 {
		t.Fatal(u)
	}
	// Read error: 0 and prev kept.
	if u, keep := CPUUsage("", errors.New("x"), st2); u != 0 || keep != st2 {
		t.Fatal(u, keep)
	}
}

// Checklist 8.
func TestMemUsage(t *testing.T) {
	if m := MemUsage("MemTotal:       1000 kB\nMemFree: 1 kB\nMemAvailable:    250 kB\n", nil); m != 75 {
		t.Fatal(m)
	}
	if m := MemUsage("MemTotal: 1000 kB\n", nil); m != 0 {
		t.Fatal(m)
	}
	if m := MemUsage("", errors.New("x")); m != 0 {
		t.Fatal(m)
	}
}

// Checklist 9: IPv4 only, headers excluded, every line counted, a missing file → 0.
func TestConnectionCount(t *testing.T) {
	fs := fsx.FS{Root: t.TempDir()}
	if n := ConnectionCount(fs); n != 0 {
		t.Fatal(n)
	}
	hdr := "  sl  local_address rem_address   st\n"
	writeFile(t, fs, "/proc/net/tcp", hdr+"   0: 00000000:0016 00000000:0000 0A\n   1: 0100007F:1F90 0100007F:D431 06\n")
	writeFile(t, fs, "/proc/net/udp", hdr+"   0: 00000000:0035 00000000:0000 07\n")
	writeFile(t, fs, "/proc/net/tcp6", hdr+"x\ny\nz\n") // ignored
	if n := ConnectionCount(fs); n != 3 {
		t.Fatal(n)
	}
	writeFile(t, fs, "/proc/net/udp", "")
	if n := ConnectionCount(fs); n != 2 {
		t.Fatal(n)
	}
}

// tests/hostNetwork.test.ts vectors (checklist 17).
var routeText = strings.Join([]string{
	"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT",
	"awg-mesh\t0000000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0",
	"eth1\t00000000\t0101A8C0\t0003\t0\t0\t200\t00000000\t0\t0\t0",
	"eth0\t00000000\t010010AC\t0003\t0\t0\t100\t00000000\t0\t0\t0",
	"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0",
}, "\n")

var devText = strings.Join([]string{
	"Inter-|   Receive                                                |  Transmit",
	" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
	"    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0",
	"  eth0: 98765432100 70000 0 0 0 0 0 0 12345678900 60000 0 0 0 0 0 0",
}, "\n")

func TestHostNetwork(t *testing.T) {
	if got := DefaultRouteInterface(routeText); got != "eth0" {
		t.Fatal(got)
	}
	var noDefault []string
	for _, l := range strings.Split(routeText, "\n") {
		if !strings.Contains(l, "\t00000000\t0") {
			noDefault = append(noDefault, l)
		}
	}
	if got := DefaultRouteInterface(strings.Join(noDefault, "\n")); got != "" {
		t.Fatal(got)
	}
	// A default route without RTF_UP is skipped; equal metrics keep the first.
	if got := DefaultRouteInterface("h\nppp0\t00000000\t0\t0002\t0\t0\t0\t0\nwg0\t00000000\t0\t0001\t0\t0\t5\t0\nwg1\t00000000\t0\t0001\t0\t0\t5\t0"); got != "wg0" {
		t.Fatal(got)
	}
	if rx, tx, ok := InterfaceCounters(devText, "eth0"); !ok || rx != 98765432100 || tx != 12345678900 {
		t.Fatal(rx, tx, ok)
	}
	if _, _, ok := InterfaceCounters(devText, "eth9"); ok {
		t.Fatal("eth9")
	}

	fs := fsx.FS{Root: t.TempDir()}
	if ReadHostNetwork(fs) != nil {
		t.Fatal("no /proc → nil")
	}
	writeFile(t, fs, "/proc/net/route", routeText)
	writeFile(t, fs, "/proc/net/dev", devText)
	if hn := ReadHostNetwork(fs); hn == nil || *hn != (HostNetwork{Iface: "eth0", RxBytes: 98765432100, TxBytes: 12345678900}) {
		t.Fatalf("%+v", hn)
	}
	writeFile(t, fs, "/proc/net/dev", "h\nh\n  lo: 1 2 3 4 5 6 7 8 9 10\n")
	if ReadHostNetwork(fs) != nil {
		t.Fatal("iface absent from dev → nil")
	}
}
