package locks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutualExclusion(t *testing.T) {
	r := New()
	var in atomic.Int32
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, unlock, err := r.Lock(context.Background(), SingBox)
			if err != nil {
				t.Error(err)
				return
			}
			if n := in.Add(1); n != 1 {
				t.Errorf("%d holders at once", n)
			}
			time.Sleep(time.Millisecond)
			in.Add(-1)
			unlock()
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func TestAreasIndependent(t *testing.T) {
	r := New()
	_, u1, _ := r.Lock(context.Background(), SingBox)
	defer u1()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, u2, err := r.Lock(ctx, Caddy); err != nil {
		t.Fatal("different area must not wait:", err)
	} else {
		u2()
	}
}

func TestWaitRespectsContext(t *testing.T) {
	r := New()
	_, unlock, _ := r.Lock(context.Background(), Caddy)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := r.Lock(ctx, Caddy); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	unlock()
	unlock() // idempotent
	// The cancelled waiter took nothing: the area is free now.
	if _, u, err := r.Lock(context.Background(), Caddy); err != nil {
		t.Fatal(err)
	} else {
		u()
	}
	// An already-done context never acquires, even a free area.
	done, c := context.WithCancel(context.Background())
	c()
	if _, _, err := r.Lock(done, AWG); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestOrderEnforced(t *testing.T) {
	r := New()
	held, unlock, err := r.Lock(context.Background(), SingBox)
	if err != nil {
		t.Fatal(err)
	}
	inner, u2, err := r.Lock(held, Firewall)
	if err != nil {
		t.Fatal("SingBox → Firewall is in order:", err)
	}
	if _, _, err := r.Lock(inner, Caddy); !errors.Is(err, ErrOrder) {
		t.Fatalf("Firewall → Caddy must be refused, got %v", err)
	}
	u2()
	unlock()
	held, unlock, _ = r.Lock(context.Background(), Caddy)
	if _, _, err := r.Lock(held, SingBox); !errors.Is(err, ErrOrder) {
		t.Fatalf("Caddy → SingBox must be refused, got %v", err)
	}
	if _, _, err := r.Lock(held, Caddy); !errors.Is(err, ErrOrder) {
		t.Fatalf("reentrant Caddy must be refused, got %v", err)
	}
	unlock()
	if _, _, err := r.Lock(context.Background(), Name("nope")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("got %v", err)
	}
}

func TestLockAll(t *testing.T) {
	r := New()
	ctx, unlock, err := r.LockAll(context.Background(), Firewall, SingBox, SingBox)
	if err != nil {
		t.Fatal(err)
	}
	if heldRank(ctx) != rank(Firewall) {
		t.Fatal("context must record the highest area")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := r.Lock(short, SingBox); err == nil {
		t.Fatal("SingBox must be held")
	}
	unlock()
	// A partial failure releases what was taken.
	_, uf, _ := r.Lock(context.Background(), Firewall)
	short2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, _, err := r.LockAll(short2, SingBox, Firewall); err == nil {
		t.Fatal("Firewall is held")
	}
	uf()
	if _, u, err := r.Lock(context.Background(), SingBox); err != nil {
		t.Fatal("SingBox leaked by the failed LockAll:", err)
	} else {
		u()
	}
}
