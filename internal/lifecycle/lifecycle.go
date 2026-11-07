// Package lifecycle holds the boot steps and background loops of the agent (spec A §1.3, §10).
//
// TS ran its loops with setInterval, which starts a new tick while the previous one still hangs
// (spec A R-18, E). Loop runs ticks strictly one after another: the next tick is scheduled only
// after the current one returns (architecture §2 "the telemetry tick does not overlap").
package lifecycle

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Step is a boot step. Steps never fail the boot (every TS boot helper swallowed its errors);
// they log instead.
type Step struct {
	Name string
	Run  func(ctx context.Context)
}

// RunSteps runs steps in order, recovering panics so one broken step cannot stop the agent.
func RunSteps(ctx context.Context, log *slog.Logger, steps []Step) {
	for _, s := range steps {
		safe(log, s.Name, func() { s.Run(ctx) })
	}
}

// Loop is a periodic background job.
type Loop struct {
	Name string
	// Interval between the end of one tick and the start of the next.
	Interval time.Duration
	// FirstDelay before the first tick (TS setInterval: one interval). Zero means Interval.
	FirstDelay time.Duration
	Tick       func(ctx context.Context)
}

// Group runs loops and one-shot tasks until its context is cancelled.
type Group struct {
	Log *slog.Logger
	wg  sync.WaitGroup
}

// Go runs a one-shot background task (e.g. kernel tuning after bind).
func (g *Group) Go(ctx context.Context, name string, fn func(ctx context.Context)) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		safe(g.Log, name, func() { fn(ctx) })
	}()
}

// Start runs l until ctx is cancelled. Ticks never overlap.
func (g *Group) Start(ctx context.Context, l Loop) {
	if l.Interval <= 0 {
		g.Log.Error("Background loop not started: non-positive interval", "loop", l.Name, "interval", l.Interval.String())
		return
	}
	first := l.FirstDelay
	if first <= 0 {
		first = l.Interval
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		t := time.NewTimer(first)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			safe(g.Log, l.Name, func() { l.Tick(ctx) })
			t.Reset(l.Interval)
		}
	}()
}

// Wait blocks until every loop and task has returned (after ctx cancellation).
func (g *Group) Wait() { g.wg.Wait() }

func safe(log *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Background job panicked", "job", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}
