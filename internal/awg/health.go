package awg

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Health-loop constants (telemetry.ts:533, :603).
const (
	HealthInterval  = 30 * time.Second
	RestartCooldown = 5 * time.Minute
	// lockWait: a tick that finds ConfigureAwg (or an AWG binary upload) holding locks.AWG skips
	// itself instead of restarting the interface under an apply.
	lockWait = 100 * time.Millisecond
)

// HealthState is a snapshot of the self-heal state (getAwgHealthState).
type HealthState struct {
	Checked         bool
	Configured      bool
	Alive           bool
	LastCheck       time.Time
	LastAutoRestart time.Time // last SUCCESSFUL self-heal restart
	LastAttempt     time.Time // last restart attempt, successful or not (cooldown anchor)
	AutoRestarts    int
}

// Health is the AWG health check / self-heal loop (spec D §4, E §2.5).
//
// Differences from TS (deviations.md): it runs only from its own 30 s loop — never inside a
// telemetry tick (telemetry reads Status, which formats the stored state); ticks never overlap
// (lifecycle.Loop) and take locks.AWG, so a check never restarts the unit under ConfigureAwg; the
// 5-minute cooldown starts at every restart ATTEMPT, so a failing restart is retried every 5 minutes
// instead of every telemetry tick; and an interface the orchestrator disabled (DisabledMarker) is
// "inactive" and never brought back. The mesh interface is deliberately not watched (spec D §4.3).
type Health struct {
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
	Locks     *locks.Registry
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	ownLocks locks.Registry
	mu       sync.Mutex
	st       HealthState
}

func (h *Health) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Health) locks() *locks.Registry {
	if h.Locks != nil {
		return h.Locks
	}
	return &h.ownLocks
}

// Tick is one loop iteration: check, heal if needed, store.
func (h *Health) Tick(ctx context.Context) {
	wait, cancel := context.WithTimeout(ctx, lockWait)
	held, unlock, err := h.locks().Lock(wait, locks.AWG)
	cancel()
	if err != nil {
		h.Log.Debug("AWG health check skipped: the AWG area is busy", "err", err.Error())
		return
	}
	defer unlock()
	h.Check(context.WithoutCancel(held))
}

// Check is checkAndHealAwgInterface for getAwgInterfaceName(). It does not take the lock (Tick does).
func (h *Health) Check(ctx context.Context) HealthState {
	iface := Iface(h.Cfg)
	unit := "route-awg@" + iface
	now := h.now()

	h.mu.Lock()
	st := h.st
	h.mu.Unlock()
	st.Checked = true
	st.LastCheck = now

	st.Configured = h.configured(ctx, iface, unit)
	if !st.Configured {
		st.Alive = false
		h.store(st)
		return st
	}
	_, err := h.Runner.Run(ctx, runner.Cmd{Name: "ip", Args: []string{"link", "show", iface}, Timeout: IPTimeout})
	st.Alive = err == nil
	if !st.Alive {
		h.Log.Warn("AWG interface "+iface+" is down but unit is configured. Checking auto-restart cooldown...", "iface", iface)
		since := now.Sub(st.LastAttempt)
		if st.LastAttempt.IsZero() || since >= RestartCooldown {
			h.Log.Warn("Initiating self-healing restart for AWG service "+unit+"...", "iface", iface)
			st.LastAttempt = now
			if err := h.Systemctl.Restart(ctx, unit); err != nil {
				h.Log.Error("Self-healing restart failed for "+unit, "iface", iface, "err", err.Error())
			} else {
				st.LastAutoRestart = now
				st.AutoRestarts++
				h.Log.Info("Self-healing restart executed for "+unit, "iface", iface, "count", st.AutoRestarts)
			}
		} else {
			waitSec := int(math.Ceil((RestartCooldown - since).Seconds()))
			h.Log.Warn("Auto-restart cooldown active for "+iface+". Next attempt allowed in "+strconv.Itoa(waitSec)+"s.", "iface", iface, "waitSec", waitSec)
		}
	}
	h.store(st)
	return st
}

func (h *Health) store(st HealthState) {
	h.mu.Lock()
	h.st = st
	h.mu.Unlock()
}

// configured: not disabled by the orchestrator, and (config file exists OR unit enabled).
func (h *Health) configured(ctx context.Context, iface, unit string) bool {
	if _, err := os.Stat(h.FS.Path(DisabledMarker(iface))); err == nil {
		return false
	}
	if _, err := os.Stat(h.FS.Path(ConfigPath(h.Cfg))); err == nil {
		return true
	}
	st, err := h.Systemctl.IsEnabled(ctx, unit)
	return err == nil && st == "enabled"
}

// State returns a snapshot.
func (h *Health) State() HealthState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st
}

// Status is the awg_status telemetry string, formatted from the last check at the current time:
// "unknown" before the first check, then inactive | nominal | nominal (restarted Nm ago|Nh ago)
// (restart < 24 h ago) | crashed | crashed (restarted …) (telemetry.ts:560-585).
func (h *Health) Status() string {
	return FormatStatus(h.State(), h.now())
}

// FormatStatus is the status-string half of checkAndHealAwgInterface.
func FormatStatus(st HealthState, now time.Time) string {
	if !st.Checked {
		return "unknown"
	}
	if !st.Configured {
		return "inactive"
	}
	info := ""
	var elapsed time.Duration
	if !st.LastAutoRestart.IsZero() {
		elapsed = now.Sub(st.LastAutoRestart)
		mins := int64(math.Floor(float64(elapsed.Milliseconds()) / 60000))
		if mins < 60 {
			info = "restarted " + strconv.FormatInt(max(0, mins), 10) + "m ago"
		} else {
			info = "restarted " + strconv.FormatInt(mins/60, 10) + "h ago"
		}
	}
	if st.Alive {
		if info != "" && elapsed < 24*time.Hour {
			return "nominal (" + info + ")"
		}
		return "nominal"
	}
	if info != "" {
		return "crashed (" + info + ")"
	}
	return "crashed"
}
