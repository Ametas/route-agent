package binaries

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/binaries/dkms"
	"github.com/Ametas/route-agent/internal/runner"
)

const kernel = "6.8.0-139-generic"

func meshHarness(t *testing.T) *harness {
	h := newHarness(t, nil)
	h.svc.mesh = &dkms.Installer{Runner: h.fake, FS: h.fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		KernelRelease: func() string { return kernel }, Invalidate: h.svc.telemetry.MeshKernelChanged}
	h.on(named("tar"), runReal)
	h.on(display("lsmod"), stdout("Module                  Size  Used by\namneziawg             118784  0\nudp_tunnel 32768 1 amneziawg\n"))
	return h
}

func fixture(t *testing.T) []*agentpb.BinaryChunkPayload {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "amneziawg-kernel-src-fixture.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*agentpb.BinaryChunkPayload
	for i := 0; i < len(b); i += 100 {
		end := min(i+100, len(b))
		out = append(out, &agentpb.BinaryChunkPayload{Chunk: b[i:end], Version: "ignored", TargetBinary: "amneziawg-kernel-src"})
	}
	return out
}

// Checklist 29, 30, 31: fixture extracted with --strip-components=1, parsed name/version, full
// build sequence with the TS timeouts, modprobe + lsmod, modules-load.d, cleanup.
func TestMeshKernelSourceBuild(t *testing.T) {
	h := meshHarness(t)
	r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...)
	expect(t, r, true, "AmneziaWG kernel module (amneziawg v9.9.9-test) installed and loaded")

	type step struct {
		prefix  string
		timeout time.Duration
	}
	want := []step{
		{"tar -xzf " + h.tmp + "/amneziawg_kernel_src_", 60 * time.Second},
		{"dkms status -m amneziawg -v 9.9.9-test", 30 * time.Second},
		{"dkms status -m amneziawg", 30 * time.Second},
		{"dpkg --configure -a", 300 * time.Second},
		{"apt-get update -y", 120 * time.Second},
		{"apt-get install -y dkms build-essential linux-headers-" + kernel, 300 * time.Second},
		{"make -C " + h.tmp + "/amneziawg_kernel_build_", 60 * time.Second},
		{"dkms add -m amneziawg -v 9.9.9-test", 30 * time.Second},
		{"dkms build -m amneziawg -v 9.9.9-test", 180 * time.Second},
		{"dkms install -m amneziawg -v 9.9.9-test", 60 * time.Second},
		{"modprobe amneziawg", 15 * time.Second},
		{"lsmod", 5 * time.Second},
	}
	calls := h.fake.Calls()
	if len(calls) != len(want) {
		t.Fatalf("calls: %q", h.displays())
	}
	for i, w := range want {
		if !strings.HasPrefix(calls[i].Display(), w.prefix) || calls[i].Timeout != w.timeout {
			t.Errorf("step %d: %q (%v), want prefix %q (%v)", i, calls[i].Display(), calls[i].Timeout, w.prefix, w.timeout)
		}
	}
	if c := calls[0]; !strings.Contains(c.Display(), " --strip-components=1 -C ") {
		t.Fatal(c.Display())
	}
	if c := calls[6]; !strings.HasSuffix(c.Display(), "/src dkms-install") {
		t.Fatalf("make must run in the dkms.conf directory: %q", c.Display())
	}
	if h.read(dkms.ModulesLoadPath) != "amneziawg\n" || h.meshCI != 1 {
		t.Fatal("modules-load.d / cache invalidation")
	}
	if entries, _ := os.ReadDir(h.tmp); len(entries) != 0 {
		t.Fatalf("tar or build dir left: %v", entries)
	}
}

func TestMeshKernelAlreadyInstalledStillLoads(t *testing.T) {
	h := meshHarness(t)
	h.on(display("dkms status -m amneziawg -v 9.9.9-test"), stdout("amneziawg/9.9.9-test, "+kernel+", x86_64: installed\n"))
	expect(t, h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...), true, "AmneziaWG kernel module (amneziawg v9.9.9-test) installed and loaded")
	if h.calls("apt-get") != 0 || h.calls("dkms build") != 0 || h.calls("modprobe amneziawg") != 1 || h.calls("lsmod") != 1 {
		t.Fatalf("%q", h.displays())
	}
}

func TestMeshKernelFailures(t *testing.T) {
	t.Run("build failure carries the make.log tail", func(t *testing.T) {
		h := meshHarness(t)
		h.write("/var/lib/dkms/amneziawg/9.9.9-test/build/make.log", strings.Repeat("a", 1000)+strings.Repeat("b", 2000), 0o644)
		h.on(display("dkms build -m amneziawg -v 9.9.9-test"), failing(10, "Error! Bad return status for module build on kernel"))
		r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...)
		expect(t, r, false, "Failed to install kernel module: Command failed: dkms build -m amneziawg -v 9.9.9-test\n"+
			"Error! Bad return status for module build on kernel\n\n--- dkms build log (tail) ---\n"+strings.Repeat("b", 2000))
		if h.calls("dkms install") != 0 || h.calls("modprobe") != 0 {
			t.Fatal(h.displays())
		}
		if entries, _ := os.ReadDir(h.tmp); len(entries) != 0 {
			t.Fatalf("left: %v", entries)
		}
	})
	t.Run("dkms add already added is tolerated", func(t *testing.T) {
		h := meshHarness(t)
		h.on(display("dkms add -m amneziawg -v 9.9.9-test"), failing(3, "Error! DKMS tree already contains: amneziawg-9.9.9-test\nYou cannot add the same module/version combo more than once."))
		expect(t, h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...), true, "AmneziaWG kernel module (amneziawg v9.9.9-test) installed and loaded")
	})
	t.Run("dkms add other error", func(t *testing.T) {
		h := meshHarness(t)
		h.on(display("dkms add -m amneziawg -v 9.9.9-test"), failing(1, "Error! permission denied"))
		r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...)
		expect(t, r, false, "Failed to install kernel module: Command failed: dkms add -m amneziawg -v 9.9.9-test\nError! permission denied")
	})
	t.Run("not loaded", func(t *testing.T) {
		h := meshHarness(t)
		h.on(display("lsmod"), stdout("Module Size Used by\namneziawgx 1 0\n"))
		r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...)
		expect(t, r, false, "Failed to install kernel module: Command failed: lsmod | grep -w amneziawg\n")
	})
	t.Run("not a tarball", func(t *testing.T) {
		h := meshHarness(t)
		r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), &agentpb.BinaryChunkPayload{Chunk: []byte("garbage")})
		if r.Success || !strings.HasPrefix(r.Message, "Failed to install kernel module: Command failed: tar -xzf ") {
			t.Fatalf("%+v", r)
		}
		if entries, _ := os.ReadDir(h.tmp); len(entries) != 0 {
			t.Fatalf("left: %v", entries)
		}
	})
	t.Run("stale trees removed before apt", func(t *testing.T) {
		h := meshHarness(t)
		h.on(display("dkms status -m amneziawg"), stdout("amneziawg/1.0.0, 6.8.0-100-generic, x86_64: installed\namneziawg/1.0.0: added\n"))
		h.on(display("dkms remove -m amneziawg -v 1.0.0 --all"), failing(1, "x")) // logged only
		expect(t, h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...), true, "AmneziaWG kernel module (amneziawg v9.9.9-test) installed and loaded")
		if i, j := h.index("dkms remove -m amneziawg -v 1.0.0 --all"), h.index("dpkg --configure -a"); i < 0 || j < i || h.calls("dkms remove") != 1 {
			t.Fatal(h.displays())
		}
	})
	t.Run("apt failure stops", func(t *testing.T) {
		h := meshHarness(t)
		h.on(func(c runner.Cmd) bool { return strings.HasPrefix(c.Display(), "apt-get install") }, failing(100, "E: Unable to locate package linux-headers-x"))
		r := h.upload(h.svc.UploadMeshAwgKernelSource, mdCtx(secret), fixture(t)...)
		if r.Success || !strings.HasSuffix(r.Message, "\nE: Unable to locate package linux-headers-x") || h.calls("make") != 0 {
			t.Fatalf("%+v %q", r, h.displays())
		}
	})
}
