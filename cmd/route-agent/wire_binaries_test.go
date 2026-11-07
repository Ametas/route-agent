package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/version"
)

func TestGroupCWired(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{}
	root := t.TempDir()
	d := deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: f, fs: fsx.FS{Root: root},
		systemctl: systemd.Systemctl{Runner: f}, auth: auth.New("x"), version: &version.Resolver{Runner: f}, exe: "/x",
	}
	h := buildHandlers(d)
	if h.UpgradeSingbox == nil || h.UploadSingboxBinary == nil || h.UploadAwgToolsBinary == nil || h.UploadAwgGoBinary == nil ||
		h.UploadCaddyBinary == nil || h.UploadMihomoBinary == nil || h.UploadRearRuleSet == nil ||
		h.UploadOlcrtcAgentSrvBinary == nil || h.UploadMeshAwgKernelSource == nil {
		t.Fatal("group C RPCs must be wired")
	}
	// Boot step 1 is sanitizeAwgToolsSymlink: it removes a legacy symlink at the tools path.
	steps := preBindSteps(d)
	if steps[0].Name != "sanitizeAwgToolsSymlink" {
		t.Fatalf("step 1 is %q", steps[0].Name)
	}
	link := d.fs.Path("/usr/local/bin/awg")
	_ = os.MkdirAll(d.fs.Path("/usr/local/bin"), 0o755)
	if err := os.Symlink("/nowhere", link); err != nil {
		t.Fatal(err)
	}
	steps[0].Run(context.Background())
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("symlink not removed at boot")
	}
}
