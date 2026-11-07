package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/version"
)

func TestGroupBWired(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{}
	d := deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: f, fs: fsx.FS{Root: t.TempDir()},
		systemctl: systemd.Systemctl{Runner: f}, auth: auth.New("x"), version: &version.Resolver{Runner: f}, exe: "/x",
	}
	h := buildHandlers(d)
	if h.ApplyConfig == nil || h.ConfigureCaddy == nil || h.GetSingBoxUserTraffic == nil || h.GetSingBoxConnections == nil {
		t.Fatal("group B RPCs must be wired")
	}
	found := false
	for _, s := range preBindSteps(d) {
		if s.Name == "fixXraySocketPermissions" {
			found = true
		}
	}
	if !found {
		t.Fatal("boot step 4 fixXraySocketPermissions missing")
	}
}
