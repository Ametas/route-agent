package singbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/caddy"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// ForkVersionSuffix marks our sing-box fork with the users-api service (hotUsers.ts:38).
const ForkVersionSuffix = "-hotusers"

// configMode is the sing-box config's mode: it holds reality private keys, inbound passwords
// and the clash secret (spec B R8: TS left it at the umask default, 0644). The owner of an
// existing file is kept, so a core running as another user still reads it.
const configMode = 0o600

// jsNonSpace is JavaScript's \S.
const jsNonSpace = `[^\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]`

var (
	versionRe      = regexp.MustCompile(`sing-box version (` + jsNonSpace + `+)`)
	versionLooseRe = regexp.MustCompile(`(?i)version[\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]+(` + jsNonSpace + `+)`)
)

// ParseVersionOutput is parseSingBoxVersionOutput (telemetry.ts:124-127): the WHOLE token after
// "sing-box version" (so "1.14.0-hotusers" keeps its suffix), else after "version", else the
// first line trimmed, else "not_installed".
func ParseVersionOutput(stdout string) string {
	if m := versionRe.FindStringSubmatch(stdout); m != nil {
		return m[1]
	}
	if m := versionLooseRe.FindStringSubmatch(stdout); m != nil {
		return m[1]
	}
	first, _, _ := strings.Cut(stdout, "\n")
	if t := jsjson.Trim(first); t != "" {
		return t
	}
	return "not_installed"
}

// Version is getSingBoxVersion (telemetry.ts:129-144), uncached: "not_installed" when the unit
// file is missing (a binary nothing can start is not an installed core) or `<bin> version`
// fails; otherwise ParseVersionOutput of its stdout.
func (s *Service) Version(ctx context.Context) string {
	if _, err := os.Stat(s.FS.Path(s.unitPath())); err != nil {
		return "not_installed"
	}
	res, err := s.Runner.Run(ctx, runner.Cmd{Name: s.binaryPath(), Args: []string{"version"}, Timeout: versionTimeout})
	if err != nil {
		return "not_installed"
	}
	return ParseVersionOutput(res.Stdout)
}

// IsFork reports whether the installed core is our -hotusers fork (isForkBinary).
func (s *Service) IsFork(ctx context.Context) bool {
	return strings.HasSuffix(s.Version(ctx), ForkVersionSuffix)
}

// UnitActive is isSingboxUnitActive: `systemctl is-active sing-box` prints "active". Any other
// state, and a failure to ask, is "not active".
func (s *Service) UnitActive(ctx context.Context) bool {
	state, err := s.Systemctl.IsActive(ctx, "sing-box")
	return err == nil && state == "active"
}

// ImageStale is isSingboxImageStale: the running sing-box executes a binary that has since been
// replaced on disk (/proc/<MainPID>/exe ends with " (deleted)"). A reload would SIGHUP the same
// old process, so only a restart heals it. Any doubt answers false: the other direction would
// restart the core — dropping every session — on every push.
func (s *Service) ImageStale(ctx context.Context) bool {
	res, err := s.Systemctl.Run(ctx, "show", "sing-box", "-p", "MainPID", "--value")
	if err != nil {
		return false
	}
	pid := strings.TrimSpace(res.Stdout)
	if pid == "" || pid == "0" {
		return false
	}
	target, err := os.Readlink(s.FS.Path("/proc/" + pid + "/exe"))
	if err != nil {
		return false
	}
	return strings.HasSuffix(target, " (deleted)")
}

// reloadCmd is resolveSingboxReloadCommand: an inactive unit needs `systemctl start sing-box`
// (reload would bring nothing up), a stale image needs SINGBOX_RESTART_COMMAND, otherwise
// RELOAD_COMMAND. The configurable ones are shell strings (contract, spec A §6).
func (s *Service) reloadCmd(ctx context.Context) runner.Cmd {
	if !s.UnitActive(ctx) {
		return runner.Cmd{Name: "systemctl", Args: []string{"start", "sing-box"}, Timeout: reloadTimeout}
	}
	if s.ImageStale(ctx) {
		s.Log.Warn("Running sing-box executes a replaced binary — restarting instead of reloading")
		return runner.Cmd{Name: s.Cfg.SingboxRestartCommand, Shell: true, Timeout: reloadTimeout}
	}
	return runner.Cmd{Name: s.Cfg.ReloadCommand, Shell: true, Timeout: reloadTimeout}
}

// Serialize is the only on-disk form of a config: JSON.stringify(cfg, null, 2) (spec B §2).
func Serialize(cfg any) string { return jsjson.Stringify(cfg, "  ") }

// configMatches: the file on disk is byte for byte serialized. A missing file never matches.
func (s *Service) configMatches(serialized string) bool {
	cur, err := s.FS.ReadFile(s.configPath())
	return err == nil && bytes.Equal(cur, []byte(serialized))
}

func uniqueID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + hex.EncodeToString(b[:])
}

// ValidateConfig is validateSingBoxConfig (singbox.ts:252-275), also used by the rear core
// (group F): the Caddy ownership fix-up first (as in TS), then `sing-box check -c` on a temp
// file `.config.check_<ms>_<8 hex>.json` in the config's directory, removed afterwards. On
// failure it returns false and `err.stderr || err.message`.
func (s *Service) ValidateConfig(ctx context.Context, cfg any) (bool, string) {
	return s.validate(ctx, Serialize(cfg))
}

func (s *Service) validate(ctx context.Context, serialized string) (bool, string) {
	caddy.FixPermissions(ctx, s.Runner, s.FS, s.Log, caddy.LibDir)
	dir := path.Dir(s.configPath())
	check := path.Join(dir, ".config.check_"+uniqueID()+".json")
	defer func() { _ = os.Remove(s.FS.Path(check)) }()
	if err := s.FS.WriteFile(check, []byte(serialized), fsx.WriteOptions{Mode: configMode}); err != nil {
		s.Log.Error("Sing-box configuration syntax check failed", "err", err.Error())
		return false, err.Error()
	}
	if _, err := s.Runner.Run(ctx, runner.Cmd{Name: s.binaryPath(), Args: []string{"check", "-c", check}, Timeout: checkTimeout}); err != nil {
		var e *runner.Error
		stderr := ""
		if errors.As(err, &e) {
			stderr = e.Stderr
		}
		s.Log.Error("Sing-box configuration syntax check failed", "stderr", stderr)
		return false, runner.StderrOrMessage(err)
	}
	return true, ""
}

// writeConfig is writeSingboxConfigAtomically: optionally copy the current file to
// config.json.bak (errors ignored), then replace the config atomically (fsx). It reports whether
// a config existed before.
func (s *Service) writeConfig(serialized string, backup bool) (bool, error) {
	p := s.configPath()
	_, err := os.Stat(s.FS.Path(p))
	had := err == nil
	if had && backup {
		if data, err := s.FS.ReadFile(p); err == nil {
			_ = s.FS.WriteFile(p+".bak", data, fsx.WriteOptions{Mode: configMode})
		}
	}
	if err := s.FS.WriteFile(p, []byte(serialized), fsx.WriteOptions{Mode: configMode}); err != nil {
		return had, err
	}
	return had, nil
}

// restoreBackup copies config.json.bak back over the config (errors ignored).
func (s *Service) restoreBackup() {
	p := s.configPath()
	if data, err := s.FS.ReadFile(p + ".bak"); err == nil {
		_ = s.FS.WriteFile(p, data, fsx.WriteOptions{Mode: configMode})
	}
}

// readConfigOnDisk is readSingboxConfigOnDisk: the parsed config, or nil when it is missing,
// unparsable or not an object (an array counts as an object, as typeof does).
func (s *Service) readConfigOnDisk() any {
	raw, err := s.FS.ReadFile(s.configPath())
	if err != nil {
		return nil
	}
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil
	}
	switch v.(type) {
	case *jsjson.Object, []any:
		return v
	}
	return nil
}

// reload is the reload half of atomicApplyAndReload: pick the command, run it, log its output;
// on failure restore config.json.bak (when a config existed before) and return the exec error.
// No second reload follows the restore (spec B R7, kept).
func (s *Service) reload(ctx context.Context, hadPrevious bool) error {
	res, err := s.Runner.Run(ctx, s.reloadCmd(ctx))
	if err != nil {
		if hadPrevious {
			s.restoreBackup()
		}
		return err
	}
	if res.Stdout != "" {
		s.Log.Info("Reload command stdout", "stdout", res.Stdout)
	}
	if res.Stderr != "" {
		s.Log.Warn("Reload command stderr", "stderr", res.Stderr)
	}
	return nil
}
