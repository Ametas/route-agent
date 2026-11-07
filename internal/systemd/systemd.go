// Package systemd drives systemctl and writes unit files.
//
// It mirrors src/services/systemdUnit.service.ts and the unit helpers spread over the TS agent
// (spec A §9.3, §9.4; D §2; F §1.3, §6.2):
//
//   - Systemctl: systemctl verbs through the runner (argv, no shell, a timeout on each call).
//     D-Bus is a later step (architecture §2).
//   - EnsureUnit: the idempotent "write only if the bytes differ, then daemon-reload" pattern every
//     ensure*Unit function in TS uses. daemon-reload failure is only a warning; EnsureUnit never
//     fails the caller (TS returned false and logged).
//   - AwgUnit / OlcrtcAgentSrvUnit: the byte-exact unit contents. Any byte difference would make
//     every boot rewrite the unit and daemon-reload (spec A §9.3).
package systemd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
)

// DefaultTimeout bounds one systemctl call. A restart of a unit with a slow ExecStart (awg-quick,
// sing-box) fits well inside it; callers with known longer jobs pass their own.
const DefaultTimeout = 60 * time.Second

// Systemctl runs systemctl verbs.
type Systemctl struct {
	Runner runner.Runner
	// Binary is "systemctl" unless overridden (tests).
	Binary string
	// Timeout per call; 0 means DefaultTimeout.
	Timeout time.Duration
}

func (s Systemctl) cmd(args ...string) runner.Cmd {
	bin := s.Binary
	if bin == "" {
		bin = "systemctl"
	}
	t := s.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	return runner.Cmd{Name: bin, Args: args, Timeout: t}
}

// Run executes `systemctl <args...>` and returns its result.
func (s Systemctl) Run(ctx context.Context, args ...string) (runner.Result, error) {
	return s.Runner.Run(ctx, s.cmd(args...))
}

// DaemonReload runs `systemctl daemon-reload`.
func (s Systemctl) DaemonReload(ctx context.Context) error {
	_, err := s.Run(ctx, "daemon-reload")
	return err
}

// Start, Stop, Restart, Reload, Enable, Disable run the verb on the given units.
func (s Systemctl) Start(ctx context.Context, units ...string) error {
	return s.verb(ctx, "start", units)
}
func (s Systemctl) Stop(ctx context.Context, units ...string) error {
	return s.verb(ctx, "stop", units)
}
func (s Systemctl) Restart(ctx context.Context, units ...string) error {
	return s.verb(ctx, "restart", units)
}
func (s Systemctl) Reload(ctx context.Context, units ...string) error {
	return s.verb(ctx, "reload", units)
}
func (s Systemctl) Enable(ctx context.Context, units ...string) error {
	return s.verb(ctx, "enable", units)
}
func (s Systemctl) Disable(ctx context.Context, units ...string) error {
	return s.verb(ctx, "disable", units)
}

func (s Systemctl) verb(ctx context.Context, verb string, units []string) error {
	_, err := s.Run(ctx, append([]string{verb}, units...)...)
	return err
}

// IsActive runs `systemctl is-active <unit>` and returns the trimmed state word ("active",
// "inactive", "failed", …). A non-zero exit is not an error here: it is how systemctl reports
// "not active"; only a failure to run systemctl at all is returned.
func (s Systemctl) IsActive(ctx context.Context, unit string) (string, error) {
	return s.state(ctx, "is-active", unit)
}

// IsEnabled runs `systemctl is-enabled <unit>` with the same conventions as IsActive.
func (s Systemctl) IsEnabled(ctx context.Context, unit string) (string, error) {
	return s.state(ctx, "is-enabled", unit)
}

func (s Systemctl) state(ctx context.Context, verb, unit string) (string, error) {
	res, err := s.Run(ctx, verb, unit)
	if err != nil {
		if e, ok := err.(*runner.Error); ok && e.Spawn == nil && !e.Killed {
			return strings.TrimSpace(res.Stdout), nil
		}
		return strings.TrimSpace(res.Stdout), err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// EnsureUnit writes content to path only if the file's bytes differ (mode is not compared, as in
// TS), then runs daemon-reload. It returns true when it wrote. Errors are logged, never returned
// (the TS functions never threw; boot continues). label names the unit in log lines
// ("AWG", "olcrtc-agent-srv").
func EnsureUnit(ctx context.Context, f fsx.FS, sc Systemctl, log *slog.Logger, label, path, content string) bool {
	if f.UpToDate(path, []byte(content), 0) {
		log.Debug(label+" systemd unit file is already up to date; skipping write and daemon-reload", "path", path)
		return false
	}
	if err := f.WriteFile(path, []byte(content), fsx.WriteOptions{}); err != nil {
		log.Error("Failed to ensure "+label+" systemd unit file", "err", err.Error(), "path", path)
		return false
	}
	log.Info("Provisioned/updated "+label+" systemd unit file", "path", path)
	if err := sc.DaemonReload(ctx); err != nil {
		log.Warn("Failed to execute systemctl daemon-reload", "err", err.Error())
	} else {
		log.Info("Executed systemctl daemon-reload after updating " + label + " systemd unit file")
	}
	return true
}

// AwgUnit is the route-awg@.service template (systemdUnit.service.ts:19-33), shared by the client
// AWG (route-awg@awg0) and the mesh (route-awg@awgmesh0). awgBin is AWG_TOOLS_BINARY_PATH (empty
// falls back to /usr/local/bin/awg); awg-quick is derived as awgBin+"-quick", NOT
// AWG_QUICK_BINARY_PATH (spec A R-14).
//
// TODO(architecture §6 item 5): ExecReload uses bash process substitution `<(…)` under /bin/sh,
// which is dash on Debian/Ubuntu, so every reload fails and callers fall back to restart — and the
// restart is what actually applies Address/MTU changes today (spec D R1, line "A Go port that
// fixes the shell would silently stop applying AWG Address/MTU changes"). Reproduced byte-exact on
// purpose; the fix is a deliberate change together with the AWG/mesh callers (group D).
func AwgUnit(awgBin string) string {
	if awgBin == "" {
		awgBin = "/usr/local/bin/awg"
	}
	awgQuick := awgBin + "-quick"
	return "[Unit]\n" +
		"Description=AmneziaWG interface %i (managed by route-agent)\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=oneshot\n" +
		"RemainAfterExit=yes\n" +
		"ExecStart=" + awgQuick + " up %i\n" +
		"ExecStop=" + awgQuick + " down %i\n" +
		"ExecReload=/bin/sh -c '" + awgBin + " syncconf %i <(" + awgQuick + " strip %i)'\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

// AwgUnitPath applies the TS fallback: an empty AWG_UNIT_FILE_PATH means the default path.
func AwgUnitPath(configured string) string {
	if configured == "" {
		return "/etc/systemd/system/route-awg@.service"
	}
	return configured
}

// OlcrtcAgentSrvUnit is the olcrtc-agent-srv@.service template (olcrtc.service.ts:102-116).
func OlcrtcAgentSrvUnit(binPath, configDir string) string {
	return "[Unit]\n" +
		"Description=olcrtc-agent-srv instance %i (managed by route-agent)\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"User=root\n" +
		"ExecStart=" + binPath + " " + configDir + "/%i.yaml\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}
