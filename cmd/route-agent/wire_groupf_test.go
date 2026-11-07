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

func TestGroupFWired(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x", "REAR_MIHOMO_UNIT_FILE_PATH": "/etc/systemd/system/custom-rear.service"})
	if err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{}
	d := deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: f, fs: fsx.FS{Root: t.TempDir()},
		systemctl: systemd.Systemctl{Runner: f}, auth: auth.New("x"), version: &version.Resolver{Runner: f}, exe: "/x",
	}
	h := buildHandlers(d)
	if h.ConfigureRearSingbox == nil || h.GetRearConnections == nil || h.SyncOlcrtcInstance == nil ||
		h.DeleteOlcrtcInstance == nil || h.StreamOlcrtcEvents == nil {
		t.Fatal("group F RPCs must be wired")
	}
	// The upload hooks are the group F ones: the mihomo unit name follows the configured path.
	if u, _ := newRear(d).MihomoUnit(); u != "custom-rear" {
		t.Fatalf("mihomo unit %q", u)
	}
	if newRear(d).ValidateSingbox == nil {
		t.Fatal("sing-box rear validator not wired")
	}
	if l := newRear(d).GuardLoop(); l.Name != "rear-warp-guard" || l.Interval.Milliseconds() != 30000 {
		t.Fatalf("guard loop %+v", l)
	}
}
