package telemetry

import (
	"context"
	"sync"
	"time"
)

// probeCap bounds a detached probe as a whole, on top of the per-command runner timeouts (the
// AWG tools probe may try up to four binaries one after another).
const probeCap = 60 * time.Second

// source is one external data source of a frame (architecture §2: a hung `awg`, `systemctl` or
// `ip` must not stall a frame; TS awaited every probe with no timeout, spec E R1).
//
//   - At most one probe of a source runs at a time; concurrent callers (several streams, a unary
//     GetTelemetry) share it instead of stacking processes (TS had no in-flight dedupe, R10).
//   - A caller waits at most its budget. A probe that outlives the budget keeps running, bounded
//     by its commands' own timeouts and probeCap, and its result serves the next frames.
//   - Meanwhile the caller gets the last completed value of the source, or the fallback before
//     any probe ever completed.
type source[T any] struct {
	mu       sync.Mutex
	inflight chan struct{}
	last     T
	has      bool
}

func (s *source[T]) get(ctx context.Context, budget time.Duration, fallback T, probe func(context.Context) T) T {
	s.mu.Lock()
	ch := s.inflight
	if ch == nil {
		ch = make(chan struct{})
		s.inflight = ch
		go s.run(ch, probe)
	}
	s.mu.Unlock()

	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.has {
		return s.last
	}
	return fallback
}

func (s *source[T]) run(ch chan struct{}, probe func(context.Context) T) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), probeCap)
	defer cancel()
	var v T
	ok := false
	func() {
		defer func() { _ = recover() }() // a broken probe keeps the previous value
		v = probe(ctx)
		ok = true
	}()
	s.mu.Lock()
	if ok {
		s.last, s.has = v, true
	}
	s.inflight = nil
	s.mu.Unlock()
}

// forget drops the last value (after a cache invalidation the old version must not be served as
// the fallback of a slow re-probe).
func (s *source[T]) forget() {
	s.mu.Lock()
	var zero T
	s.last, s.has = zero, false
	s.mu.Unlock()
}

// cached is a source behind a TTL cache (the 60 s version caches of telemetry.ts:146-157,
// 307-329). A fresh value answers without probing; invalidate forces the next call to probe.
type cached[T any] struct {
	ttl time.Duration
	src source[T]

	mu    sync.Mutex
	val   T
	at    time.Time
	valid bool
	gen   uint64
}

func (c *cached[T]) get(ctx context.Context, now time.Time, budget time.Duration, fallback T, probe func(context.Context) T) T {
	c.mu.Lock()
	if c.valid && now.Sub(c.at) < c.ttl {
		v := c.val
		c.mu.Unlock()
		return v
	}
	gen := c.gen
	c.mu.Unlock()
	return c.src.get(ctx, budget, fallback, func(pctx context.Context) T {
		v := probe(pctx)
		c.mu.Lock()
		if c.gen == gen { // an invalidation during the probe wins
			c.val, c.at, c.valid = v, now, true
		}
		c.mu.Unlock()
		return v
	})
}

func (c *cached[T]) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.gen++
	c.mu.Unlock()
	c.src.forget()
}
