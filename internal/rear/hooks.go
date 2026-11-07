package rear

import (
	"context"
	"path/filepath"

	"github.com/Ametas/route-agent/internal/runner"
)

// The methods below implement binaries.RearHooks (group C's mihomo and rule-set uploads). They
// take no lock: the uploads already hold locks.Rear while calling them.

// VerifyMihomoConfig is verifyMihomoAcceptsLiveConfig (mihomo.service.ts:38-53): the candidate
// binary (real path) must accept the config already live on the node —
// `<candidate> -t -d <dir of cfg> -f <cfg>`. No config yet is fine. The path is
// REAR_MIHOMO_CONFIG_PATH without fallback, as in TS.
func (s *Service) VerifyMihomoConfig(ctx context.Context, candidate string) (string, string, bool) {
	cfg := s.Cfg.RearMihomoConfigPath
	if cfg == "" || !s.exists(cfg) {
		return "", "", false
	}
	real := s.FS.Path(cfg)
	_, err := s.Runner.Run(ctx, runner.Cmd{Name: candidate, Args: []string{"-t", "-d", filepath.Dir(real), "-f", real}, Timeout: mihomoCheckTimeout})
	if err == nil {
		return "", "", false
	}
	text := runner.StderrOrMessage(err)
	if text == "" {
		text = "unknown error"
	}
	return cfg, jsSlice(text, 1000), true
}

// MihomoUnit names the rear mihomo unit and reports whether its file exists (no unit → nothing
// to restart after a swap). TS hard-coded `systemctl restart route-rear-mihomo` while checking
// REAR_MIHOMO_UNIT_FILE_PATH (spec F R8); the name is now derived from that same path, as the
// rule-set reload and ConfigureRearSingbox already did.
func (s *Service) MihomoUnit() (string, bool) {
	c := s.MihomoCore()
	return c.UnitName(), s.exists(c.UnitPath)
}

// ReloadAfterRuleSet reloads (SIGHUP, never restart) the rear mihomo after a rule set was stored,
// only when its unit is active; a failure is only logged (mihomo.service.ts:187-198).
func (s *Service) ReloadAfterRuleSet(ctx context.Context, name string) {
	unit := s.MihomoCore().UnitName()
	if !s.Systemctl.Active(ctx, unit) {
		return
	}
	if err := s.Systemctl.Reload(ctx, unit); err != nil {
		s.Log.Warn("Failed to reload rear mihomo after storing a rule set", "err", err.Error(), "name", name)
	}
}
