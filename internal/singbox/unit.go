package singbox

import (
	"context"
	"log/slog"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/systemd"
)

// UnitContent is the sing-box.service unit (singbox.ts:93-105), byte-exact. There is
// deliberately no CapabilityBoundingSet/AmbientCapabilities: the bounding set also applies to
// ExecReload and took CAP_KILL away from it, so reload never worked where the process belonged
// to another user (mo-nl-node, 2026-09-04).
func UnitContent(cfg *config.Config) string {
	bin := orDefault(cfg.SingboxBinaryPath, DefaultBinaryPath)
	conf := orDefault(cfg.SingboxConfigPath, DefaultConfigPath)
	return "[Unit]\n" +
		"Description=sing-box service\n" +
		"After=network.target nss-lookup.target\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + bin + " run -c " + conf + "\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"ExecReload=/bin/sh -c \"" + bin + " check -c " + conf + " && /bin/kill -HUP $MAINPID\"\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

// EnsureUnit is ensureSingboxSystemdUnit (singbox.ts:61-134), called by UploadSingboxBinary
// (internal/binaries, group C): write the unit only when its bytes differ, then daemon-reload (a
// failure there is only logged). It reports whether it wrote and never fails the caller.
// unitPath "" means SINGBOX_UNIT_FILE_PATH (or its default).
func EnsureUnit(ctx context.Context, f fsx.FS, sc systemd.Systemctl, log *slog.Logger, cfg *config.Config, unitPath string) bool {
	if unitPath == "" {
		unitPath = orDefault(cfg.SingboxUnitFilePath, DefaultUnitPath)
	}
	return systemd.EnsureUnit(ctx, f, sc, log, "sing-box", unitPath, UnitContent(cfg))
}
