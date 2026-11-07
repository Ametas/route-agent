package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopTicksNeverOverlap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Group{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var active, maxActive, ticks atomic.Int32
	g.Start(ctx, Loop{Name: "slow", Interval: 5 * time.Millisecond, Tick: func(context.Context) {
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		time.Sleep(20 * time.Millisecond) // longer than the interval
		ticks.Add(1)
		active.Add(-1)
	}})
	time.Sleep(150 * time.Millisecond)
	cancel()
	g.Wait()
	if maxActive.Load() != 1 || ticks.Load() < 2 {
		t.Fatalf("max concurrent ticks %d, ticks %d", maxActive.Load(), ticks.Load())
	}
}

func TestPanicsAreContained(t *testing.T) {
	g := &Group{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ran := false
	RunSteps(context.Background(), g.Log, []Step{
		{Name: "boom", Run: func(context.Context) { panic("x") }},
		{Name: "next", Run: func(context.Context) { ran = true }},
	})
	if !ran {
		t.Fatal("a panicking step stopped the boot")
	}
	g.Go(context.Background(), "task", func(context.Context) { panic("y") })
	g.Wait()
}
