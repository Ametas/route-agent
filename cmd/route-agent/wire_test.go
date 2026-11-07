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

func TestGroupAWired(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{}
	h := buildHandlers(deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: f, fs: fsx.FS{Root: t.TempDir()},
		systemctl: systemd.Systemctl{Runner: f}, auth: auth.New("x"), version: &version.Resolver{Runner: f}, exe: "/x",
	})
	if h.GetAgentInfo == nil || h.SelfUpdate == nil || h.GetManagedCertificate == nil {
		t.Fatal("group A RPCs must be wired")
	}
}
