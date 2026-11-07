package version

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ametas/route-agent/internal/runner"
)

func TestValidShape(t *testing.T) {
	for _, v := range []string{"v1.4.0", "1.4.0", "v1.4.0-3-gabc1234", "v10.20.30-120-g0123456789abcdef"} {
		if !Valid(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"", "abc1234", "v1.4", "1.5.0+go", "go-1.5.0", "v1.5.0-dirty", "0.0.0-development", "v1.4.0-3-gABC"} {
		if Valid(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
}

func TestResolveOrder(t *testing.T) {
	ctx := context.Background()
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v2.0.0"
	fake := &runner.Fake{}
	if got := (&Resolver{Runner: fake}).Get(ctx); got != "v2.0.0" || len(fake.Calls()) != 0 {
		t.Fatalf("ldflags version must win without running git: %q %v", got, fake.Displays())
	}

	Version = ""
	fake = &runner.Fake{Results: []runner.FakeResult{{Result: runner.Result{Stdout: "v1.4.0-3-gabc1234\n"}}}}
	r := &Resolver{Runner: fake}
	if got := r.Get(ctx); got != "v1.4.0-3-gabc1234" {
		t.Fatalf("git describe: %q", got)
	}
	if fake.Displays()[0] != "git describe --tags --always" || fake.Calls()[0].Timeout <= 0 {
		t.Fatalf("call %+v", fake.Calls()[0])
	}
	r.Get(ctx)
	if len(fake.Calls()) != 1 {
		t.Fatal("version must be cached for the process lifetime")
	}

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"0.0.0-development"}`), 0o644)
	failing := &runner.Fake{Results: []runner.FakeResult{{Err: errors.New("not a git repository")}}}
	if got := (&Resolver{Runner: failing, Dir: dir}).Get(ctx); got != "0.0.0-development" {
		t.Fatalf("package.json fallback: %q", got)
	}
	failing = &runner.Fake{Results: []runner.FakeResult{{Err: errors.New("x")}}}
	if got := (&Resolver{Runner: failing, Dir: t.TempDir()}).Get(ctx); got != "1.0.0" {
		t.Fatalf("last fallback: %q", got)
	}
}
