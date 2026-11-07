package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
)

// Item 42: golden file — 1356 bytes, SHA-256 from spec D §10.2.
func TestRenderGolden(t *testing.T) {
	got := RenderSysctlFile(Profile)
	sum := sha256.Sum256([]byte(got))
	if len(got) != 1356 || hex.EncodeToString(sum[:]) != "0a308f06ba9ed1e728fed4a578caee3371ea2ef184b8a72bd18040a33be3cc08" {
		t.Fatalf("len %d sha %x\n%s", len(got), sum, got)
	}
	for _, absent := range []string{"udp_mem", "tcp_rmem", "tcp_wmem", "netdev_max_backlog", "ip_local_port_range"} {
		if strings.Contains(got, absent) {
			t.Errorf("%s must stay out of the profile", absent)
		}
	}
}

func newTuning(t *testing.T, fake *runner.Fake) (*Tuning, string) {
	root := t.TempDir()
	return &Tuning{Runner: fake, FS: fsx.FS{Root: root}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, root
}

func TestApply(t *testing.T) {
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		switch c.Display() {
		case "sysctl -w net.core.rmem_max=7500000":
			return runner.FailWith(c, 255, "", "sysctl: permission denied on key \"net.core.rmem_max\"")
		case "sysctl -n net.core.wmem_max":
			return runner.Result{Stdout: "7499776\n"}, nil
		case "sysctl -n net.core.default_qdisc":
			return runner.FailWith(c, 1, "", "read failed")
		}
		if strings.HasPrefix(c.Display(), "sysctl -n ") {
			return runner.Result{Stdout: " a \t b\n"}, nil
		}
		if c.Timeout <= 0 {
			t.Error("no timeout")
		}
		return runner.Result{}, nil
	}}
	tu, root := newTuning(t, fake)
	res := tu.Apply(context.Background())
	if !res.FileChanged {
		t.Fatal("file not written")
	}
	b, _ := os.ReadFile(filepath.Join(root, DefaultProfilePath))
	if string(b) != RenderSysctlFile(Profile) {
		t.Fatal("content")
	}
	// -w then -n per key; a failed -w skips its -n and does not stop the others.
	want := []string{}
	for _, e := range Profile {
		want = append(want, "sysctl -w "+e.Key+"="+e.Value)
		if e.Key != "net.core.rmem_max" {
			want = append(want, "sysctl -n "+e.Key)
		}
	}
	if got := fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	byKey := map[string]KeyResult{}
	for _, k := range res.Keys {
		byKey[k.Key] = k
	}
	if k := byKey["net.core.rmem_max"]; k.Applied || k.Error == "" {
		t.Fatalf("%+v", k)
	}
	if k := byKey["net.core.wmem_max"]; !k.Applied || k.Effective != "7499776" {
		t.Fatalf("effective from the kernel: %+v", k)
	}
	if k := byKey["net.core.rmem_default"]; k.Effective != "a b" {
		t.Fatalf("whitespace collapsed: %+v", k)
	}
	if k := byKey["net.core.default_qdisc"]; !k.Applied || k.EffectiveKnown {
		t.Fatalf("failed read-back is not a failed apply: %+v", k)
	}

	// Identical file: not rewritten.
	st1, _ := os.Stat(filepath.Join(root, DefaultProfilePath))
	time.Sleep(10 * time.Millisecond)
	if res := tu.Apply(context.Background()); res.FileChanged {
		t.Fatal("rewritten although identical")
	}
	st2, _ := os.Stat(filepath.Join(root, DefaultProfilePath))
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("mtime changed")
	}
}

// R14: an unwritable profile is logged; the keys are still applied.
func TestApplyFileErrorNotFatal(t *testing.T) {
	fake := &runner.Fake{}
	tu, root := newTuning(t, fake)
	if err := os.MkdirAll(filepath.Join(root, "/etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "/etc/sysctl.d"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tu.Apply(context.Background())
	if res.FileError == "" || res.FileChanged || len(fake.Calls()) != 2*len(Profile) {
		t.Fatalf("%+v %d", res, len(fake.Calls()))
	}
}
