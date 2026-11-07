package binaries

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// RearHooks is what the mihomo and rule-set uploads need from the rear domain (group F).
type RearHooks interface {
	// VerifyMihomoConfig checks the candidate mihomo (real path) against the live rear config.
	// rejected=false when there is nothing to check (no config yet) or the config is accepted;
	// otherwise configPath names the config and errText is the checker's output (≤1000 chars).
	VerifyMihomoConfig(ctx context.Context, candidate string) (configPath, errText string, rejected bool)
	// MihomoUnit returns the unit to restart after a mihomo swap and whether it is provisioned
	// (no unit → nothing to restart: the rear provisions it together with its config).
	MihomoUnit() (unit string, provisioned bool)
	// ReloadAfterRuleSet makes a running rear re-read its rule sets (SIGHUP); best effort.
	ReloadAfterRuleSet(ctx context.Context, name string)
}

// DefaultRear reproduces the TS rear behaviour of spec C §6 and §7.
type DefaultRear struct {
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
}

// VerifyMihomoConfig is verifyMihomoAcceptsLiveConfig (mihomo.service.ts:38-53):
// `<candidate> -t -d <dir of cfg> -f <cfg>`; a missing config is fine.
func (d *DefaultRear) VerifyMihomoConfig(ctx context.Context, candidate string) (string, string, bool) {
	cfg := d.Cfg.RearMihomoConfigPath
	real := d.FS.Path(cfg)
	if _, err := os.Stat(real); err != nil {
		return "", "", false
	}
	_, err := d.Runner.Run(ctx, runner.Cmd{Name: candidate, Args: []string{"-t", "-d", filepath.Dir(real), "-f", real}, Timeout: checkTimeout})
	if err != nil {
		return cfg, truncate(or(runner.StderrOrMessage(err), "unknown error"), 1000), true
	}
	return "", "", false
}

// MihomoUnit: the unit name is hard-coded in TS (`systemctl restart route-rear-mihomo`); it is
// restarted only when REAR_MIHOMO_UNIT_FILE_PATH exists.
func (d *DefaultRear) MihomoUnit() (string, bool) {
	_, err := os.Stat(d.FS.Path(d.Cfg.RearMihomoUnitFilePath))
	return "route-rear-mihomo", err == nil
}

// ReloadAfterRuleSet reloads (not restarts: all "stars" traffic goes through the rear, and
// mihomo re-reads its config on SIGHUP, verified 2026-09-06) only when the unit is active.
func (d *DefaultRear) ReloadAfterRuleSet(ctx context.Context, name string) {
	unit := strings.TrimSuffix(filepath.Base(d.Cfg.RearMihomoUnitFilePath), ".service")
	st, err := d.Systemctl.IsActive(ctx, unit)
	if err != nil || st != "active" {
		return
	}
	if err := d.Systemctl.Reload(ctx, unit); err != nil {
		d.Log.Warn("Failed to reload rear mihomo after storing a rule set", "err", err.Error(), "name", name)
	}
}

// UploadMihomoBinary implements the RPC (spec C §6). No setcap: the rear only listens on
// 127.0.0.1 and needs no capabilities.
func (s *Service) UploadMihomoBinary(stream UploadStream) error {
	target := or(s.cfg.MihomoBinaryPath, "/usr/local/bin/mihomo")
	return s.receive(stream, receiveSpec{
		rpc: "UploadMihomoBinary", dir: s.binaryTempDir(target), prefix: tempPrefix(target), suffix: ".tmp",
		maxBytes: s.MaxBinaryBytes, unauthEmpty: auth.InvalidSecretMessage, empty: msgNoData,
		failPrefix: prefixUploadFailed, area: locks.Rear,
	}, func(ctx context.Context, r received) (Result, error) { return s.applyMihomo(ctx, r, target) })
}

func (s *Service) applyMihomo(ctx context.Context, r received, target string) (Result, error) {
	// Smoke test: catches a broken upload and a micro-architecture mismatch that would otherwise
	// SIGILL later under load (mihomo.service.ts:71-72).
	if err := s.smokeELF(ctx, r.path, "-v"); err != nil {
		return fail("Полученный бинарь mihomo не запускается: " + truncate(err.Error(), 300)), nil
	}
	if cfgPath, errText, rejected := s.rear.VerifyMihomoConfig(ctx, r.path); rejected {
		s.log.Warn("Rejected mihomo binary: it does not accept the config already live on this node", "configPath", cfgPath, "version", r.version)
		return fail("Бинарь mihomo " + r.version + " отвергнут: он не принимает уже применённый конфиг " +
			cfgPath + ". Прежний бинарь оставлен на месте. " + truncate(errText, 400)), nil
	}
	unit, provisioned := s.rear.MihomoUnit()
	wasActive := provisioned && s.systemctl.Active(ctx, unit)
	hadPrev, err := s.install(r.path, target)
	if err != nil {
		return Result{}, err
	}
	s.log.Info("Atomically updated mihomo binary", "path", target, "version", r.version, "prev", hadPrev)
	if provisioned {
		out := s.activate(ctx, activation{
			target: target, label: "mihomo binary " + r.version, what: unit + ".service",
			wasActive: wasActive, hadPrev: hadPrev,
			restart: func(ctx context.Context) error {
				err := s.systemctl.Restart(ctx, unit)
				if err != nil {
					s.log.Warn("Failed to restart rear mihomo after binary upgrade", "err", err.Error())
				}
				return err
			},
			state: func(ctx context.Context) string { st, _ := s.systemctl.IsActive(ctx, unit); return st },
		})
		if out.message != "" {
			return fail(prefixUploadFailed + out.message), nil
		}
	}
	return ok("mihomo binary version " + r.version + " successfully updated"), nil
}

// SafeRuleSetName is the allowlist for rule-set names (mihomo.service.ts:130). The name comes
// over the network and becomes part of a file name; `!` is allowed for MetaCubeX `…-!cn` sets.
var SafeRuleSetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$`)

const msgBadRuleSetName = "Недопустимое имя набора правил."

// UploadRearRuleSet implements the RPC (spec C §7). The name travels in target_binary. The body
// is received in TempDir: the rule-set directory must not even be created for a rejected name.
func (s *Service) UploadRearRuleSet(stream UploadStream) error {
	return s.receive(stream, receiveSpec{
		rpc: "UploadRearRuleSet", dir: s.tempDir(), prefix: "ruleset_", suffix: ".tmp",
		maxBytes: s.MaxRuleSetBytes, unauthEmpty: auth.InvalidSecretMessage, empty: msgNoData,
		failPrefix: prefixUploadFailed, area: locks.Rear,
	}, s.applyRuleSet)
}

func (s *Service) applyRuleSet(ctx context.Context, r received) (Result, error) {
	name := r.targetBinary
	if !SafeRuleSetName.MatchString(name) {
		s.log.Warn("Rejected rule set with a disallowed name", "name", truncate(name, 120))
		return fail(msgBadRuleSetName), nil
	}
	dir := s.cfg.RearRuleSetDir
	if err := s.fs.MkdirAll(dir); err != nil {
		return Result{}, err
	}
	dest := filepath.Join(dir, name+".mrs")
	// Second line of defence, should the pattern ever be loosened.
	if filepath.Dir(filepath.Clean(dest)) != filepath.Clean(dir) {
		s.log.Error("Rule set path escaped its directory — refusing to write", "name", name, "destPath", dest)
		return fail(msgBadRuleSetName), nil
	}
	// Atomic: mihomo may be reading the set right now (temp in the same dir + fsync + rename).
	data, err := os.ReadFile(r.path)
	if err != nil {
		return Result{}, err
	}
	if err := s.fs.WriteFile(dest, data, fsx.WriteOptions{Mode: 0o644}); err != nil {
		return Result{}, err
	}
	s.log.Info("Rear rule set stored", "name", name, "bytes", r.bytes, "destPath", dest)
	s.rear.ReloadAfterRuleSet(ctx, name)
	return ok("Набор правил " + name + " записан (" + strconv.FormatInt(r.bytes, 10) + " байт)"), nil
}

func (s *Service) tempDir() string {
	if s.TempDir != "" {
		return s.TempDir
	}
	return os.TempDir()
}
