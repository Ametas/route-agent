package binaries

import (
	"context"
	"fmt"
)

// activation describes how a swapped binary is put into service and checked.
type activation struct {
	target string // agent path of the binary
	label  string // "sing-box binary", "Custom Caddy binary", …
	what   string // what must come up, for messages: "sing-box.service"
	// wasActive: the unit was active before the swap. Only then may "not active after the
	// restart" be blamed on the new binary (a unit that was already down proves nothing).
	wasActive bool
	hadPrev   bool
	// restart puts the current file into service; its error is direct evidence.
	restart func(ctx context.Context) error
	// state, if set, reads the unit state after the settle delay ("active" = good).
	state func(ctx context.Context) string
	// afterRollback runs after .prev is back in place and before the second restart (setcap,
	// cache invalidation).
	afterRollback func(ctx context.Context)
}

// activationOutcome is what activate found.
type activationOutcome struct {
	// restartErr is the error of the first restart (nil when it succeeded).
	restartErr error
	// failed: the new binary did not come up (restart error or not active after settling).
	failed bool
	// rolledBack: .prev was restored and restarted; message is the failure answer.
	rolledBack bool
	message    string
}

// activate restarts, post-checks and, if the new binary did not come up and a previous version
// exists, rolls back to it and restarts again (spec C risk 4: TS had no rollback anywhere).
func (s *Service) activate(ctx context.Context, a activation) activationOutcome {
	out := activationOutcome{restartErr: a.restart(ctx)}
	reason := ""
	if out.restartErr != nil {
		reason = out.restartErr.Error()
	} else if a.state != nil && a.wasActive {
		s.settle(ctx)
		if st := a.state(ctx); st != "active" {
			reason = fmt.Sprintf("%s is %q after the restart", a.what, st)
		}
	}
	if reason == "" {
		return out
	}
	out.failed = true
	if !a.hadPrev {
		s.log.Warn("New binary did not come up and there is no previous version to roll back to",
			"target", a.target, "reason", reason)
		return out
	}
	s.log.Error("New binary did not come up; rolling back to the previous version", "target", a.target, "reason", reason)
	if err := s.rollback(a.target); err != nil {
		s.log.Error("Rollback failed", "target", a.target, "err", err.Error())
		out.message = fmt.Sprintf("%s did not come up on the new %s (%s), and restoring the previous version failed: %s",
			a.what, a.label, reason, err.Error())
		return out
	}
	out.rolledBack = true
	if a.afterRollback != nil {
		a.afterRollback(ctx)
	}
	again := ""
	if err := a.restart(ctx); err != nil {
		again = "; restart on the previous version also failed: " + err.Error()
	}
	out.message = fmt.Sprintf("%s did not come up on the new %s, so the previous version (%s) was restored and restarted. Cause: %s%s",
		a.what, a.label, PrevPath(a.target), reason, again)
	return out
}
