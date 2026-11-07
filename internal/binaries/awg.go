package binaries

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/runner"
)

const bashCheckTimeout = 15 * time.Second

// awgLockKey: tools, awg-quick and amneziawg-go all restart the same AWG units, so their apply
// steps are serialized together.
const awgLockKey = "awg"

func (s *Service) awgToolsPath() string { return or(s.cfg.AwgToolsBinaryPath, defaultAwgToolsPath) }
func (s *Service) awgQuickPath() string { return or(s.cfg.AwgQuickBinaryPath, defaultAwgQuickPath) }
func (s *Service) awgGoPath() string    { return or(s.cfg.AwgGoBinaryPath, defaultAwgGoPath) }

// UploadAwgToolsBinary implements the RPC (spec C §3): target_binary "awg-quick" (exact) selects
// the awg-quick script, anything else the awg tools binary.
func (s *Service) UploadAwgToolsBinary(stream UploadStream) error {
	tools := s.awgToolsPath()
	return s.receive(stream, receiveSpec{
		rpc: "UploadAwgToolsBinary", dir: s.binaryTempDir(tools), prefix: ".awg-tools.upload-", suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: msgNoValidBinary, empty: msgNoValidBinary,
		failPrefix: prefixUploadFailed, lockKey: awgLockKey,
	}, s.applyAwgTools)
}

func (s *Service) applyAwgTools(ctx context.Context, r received) (Result, error) {
	isQuick := r.targetBinary == "awg-quick"
	target, label := s.awgToolsPath(), "awg-tools (awg) binary"
	if isQuick {
		target, label = s.awgQuickPath(), "awg-quick script"
		data, err := os.ReadFile(r.path)
		if err != nil {
			data = nil // TS: a read error counts as an empty script
		}
		if !strings.HasPrefix(string(data), "#!") {
			s.log.Warn("Rejected awg-quick upload: missing shebang")
			return fail("Invalid awg-quick script: missing shebang (#!) header."), nil
		}
		if res, err := s.run(ctx, bashCheckTimeout, "bash", "-n", r.path); err != nil {
			msg := ""
			var e *runner.Error
			if errors.As(err, &e) {
				msg = or(e.Stderr, or(res.Stdout, err.Error()))
			} else {
				msg = err.Error()
			}
			return fail("Invalid awg-quick script syntax: " + strings.TrimSpace(msg)), nil
		}
	} else {
		if err := s.smokeELF(ctx, r.path, "--version"); err != nil {
			return Result{}, err
		}
	}
	return s.swapAwg(ctx, r, target, label)
}

// UploadAwgGoBinary implements the RPC (spec C §4).
func (s *Service) UploadAwgGoBinary(stream UploadStream) error {
	target := s.awgGoPath()
	return s.receive(stream, receiveSpec{
		rpc: "UploadAwgGoBinary", dir: s.binaryTempDir(target), prefix: tempPrefix(target), suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: msgNoValidBinary, empty: msgNoValidBinary,
		failPrefix: prefixUploadFailed, lockKey: awgLockKey,
	}, func(ctx context.Context, r received) (Result, error) {
		if err := s.smokeELF(ctx, r.path, "--version"); err != nil {
			return Result{}, err
		}
		return s.swapAwg(ctx, r, target, "amneziawg-go binary")
	})
}

// smokeELF checks the ELF header, makes the candidate executable and runs it with args (the
// same `--version` probe telemetry uses for awg and amneziawg-go). TS ran no check for them.
func (s *Service) smokeELF(ctx context.Context, path string, args ...string) error {
	if err := checkELF(path, s.Arch); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	_, err := s.run(ctx, smokeTimeout, path, args...)
	return err
}

// swapAwg installs the candidate, invalidates the AWG version cache and restarts the active AWG
// units; a unit that fails to restart on the new file triggers a rollback.
func (s *Service) swapAwg(ctx context.Context, r received, target, label string) (Result, error) {
	if err := os.Chmod(r.path, 0o755); err != nil {
		return Result{}, err
	}
	units := s.activeAwgUnits(ctx)
	hadPrev, err := s.install(r.path, target)
	if err != nil {
		return Result{}, err
	}
	s.log.Info("Atomically updated AWG file", "path", target, "version", r.version, "prev", hadPrev)
	call(s.telemetry.AwgSwapped)

	if len(units) == 0 {
		s.log.Info("AWG interface " + s.awgIface() + " not yet configured, skipping restart — will use new binary on first configure")
		return ok(label + " version " + r.version + " successfully updated (service restart skipped: not yet configured)"), nil
	}
	restarted := 0
	out := s.activate(ctx, activation{
		target: target, label: label + " " + r.version, what: strings.Join(units, ", "),
		hadPrev: hadPrev,
		restart: func(ctx context.Context) error {
			restarted = 0
			var errs []error
			for _, u := range units {
				if err := s.systemctl.Restart(ctx, u); err != nil {
					s.log.Warn("Failed to restart AWG service "+u, "unit", u, "err", err.Error())
					errs = append(errs, err)
					continue
				}
				s.log.Info("Successfully restarted active AWG service "+u, "unit", u)
				restarted++
			}
			return errors.Join(errs...)
		},
		afterRollback: func(context.Context) { call(s.telemetry.AwgSwapped) },
	})
	if out.message != "" {
		return fail(prefixUploadFailed + out.message), nil
	}
	suffix := " (service restart skipped: not yet configured)"
	if restarted > 0 {
		suffix = " and active AWG service(s) restarted"
	}
	return ok(label + " version " + r.version + " successfully updated" + suffix), nil
}

var awgUnitLine = regexp.MustCompile(`^(route-awg@\S+|awg-quick@\S+)`)

// awgIface is basename(AWG_CONFIG_PATH, ".conf"), default awg0 (src/utils/awg.ts:12-16).
func (s *Service) awgIface() string {
	name := strings.TrimSuffix(filepath.Base(or(s.cfg.AwgConfigPath, "/etc/amnezia/amneziawg/awg0.conf")), ".conf")
	return or(name, "awg0")
}

// activeAwgUnits is the discovery half of restartActiveAwgServices (src/utils/awg.ts:38-91):
// every active route-awg@*/awg-quick@* unit (this includes the mesh route-awg@awgmesh0), else
// route-awg@<iface> if it reports active.
func (s *Service) activeAwgUnits(ctx context.Context) []string {
	var units []string
	seen := map[string]bool{}
	if res, err := s.systemctl.Run(ctx, "list-units", "route-awg@*", "awg-quick@*", "--state=active", "--plain", "--no-legend"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			if m := awgUnitLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				units = append(units, m[1])
			}
		}
	}
	if len(units) == 0 {
		primary := "route-awg@" + s.awgIface()
		if st, err := s.systemctl.IsActive(ctx, primary); err == nil && st == "active" {
			units = append(units, primary)
		}
	}
	return units
}

// SanitizeAwgToolsSymlink is boot step 1 (spec A §10.1, C §3): a symlink at AWG_TOOLS_BINARY_PATH,
// left by the removed UploadAwgBinary RPC, is deleted. Regular files and missing paths are left
// alone; every error is swallowed.
func (s *Service) SanitizeAwgToolsSymlink(context.Context) {
	p := s.awgToolsPath()
	real := s.fs.Path(p)
	st, err := os.Lstat(real)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return
	}
	target, err := os.Readlink(real)
	if err != nil {
		target = "unknown"
	}
	s.log.Warn("Self-heal: removed legacy symlink at AWG_TOOLS_BINARY_PATH created by deprecated UploadAwgBinary RPC",
		"path", p, "target", target)
	_ = os.Remove(real)
}
