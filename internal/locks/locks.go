// Package locks is the agent-wide registry of area locks (architecture §2: "a lock on every
// area"). One Registry is built in cmd/route-agent and handed to every group's service, so two
// RPCs of DIFFERENT groups that touch the same area — an ApplyConfig and a sing-box binary upload,
// a ConfigureCaddy and a Caddy binary upload — serialize on the same mutex. A per-service mutex
// would not: each group would only serialize with itself.
//
// # Lock map
//
//	Area      RPCs that take it (ported ones; the rest are where the owning group must take it)
//	SingBox   ApplyConfig (group B); UploadSingboxBinary (C)
//	Rear      UploadMihomoBinary, UploadRearRuleSet (C); ConfigureRearSingbox (F)
//	Caddy     ConfigureCaddy (B); UploadCaddyBinary (C)
//	AWG       UploadAwgToolsBinary, UploadAwgGoBinary (C, tools/quick/go share one lock: they
//	          restart the same AWG units); ConfigureAwg (D)
//	Mesh      UploadMeshAwgKernelSource (C, DKMS build + modprobe); ConfigureMeshTunnel (D)
//	Olcrtc    UploadOlcrtcAgentSrvBinary (C); SyncOlcrtcInstance, DeleteOlcrtcInstance (F)
//	Firewall  the egress ufw sync inside ApplyConfig (B, nested: SingBox → Firewall);
//	          ManageFirewall (D)
//
// UpgradeSingbox takes no lock: it answers an honest failure and changes nothing on the node
// (deviations.md); waiting behind a long apply only to say "unsupported" would gain nothing.
//
// # Order
//
// An RPC normally holds ONE area lock. When it must hold two, it acquires them in the fixed rank
// order below and never the other way round, so no cycle (deadlock) is possible:
//
//	SingBox < Rear < Caddy < AWG < Mesh < Olcrtc < Firewall
//
// The order is enforced, not just documented: Lock returns a context that records the held rank,
// and a Lock through such a context for an area of equal or lower rank fails with ErrOrder instead
// of deadlocking (that includes taking the same area twice — the locks are not reentrant). Pass
// the returned context down to the code that may nest. LockAll takes several areas in rank order.
//
// # Waiting
//
// Lock waits until the area is free or ctx is done (then it returns ctx.Err() and holds nothing).
// The caller chooses what the wait honours:
//
//   - unary applies (ApplyConfig, ConfigureCaddy) wait with the CLIENT's context: an orchestrator
//     that gave up does not get its push applied later (it answers DEADLINE_EXCEEDED itself and
//     retries); once acquired, the apply runs under context.WithoutCancel(held) and is not cut in
//     half by the client's deadline;
//   - uploads wait detached (context.WithoutCancel): the file is completely received at the
//     half-close and spec C §12 item 34 requires it to be applied even if the client leaves.
package locks

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Name names an area.
type Name string

// Areas, declared in rank order (see the package doc).
const (
	SingBox  Name = "sing-box"
	Rear     Name = "rear"
	Caddy    Name = "caddy"
	AWG      Name = "awg"
	Mesh     Name = "mesh"
	Olcrtc   Name = "olcrtc"
	Firewall Name = "firewall"
)

// Order is the acquisition order: an area may only be taken while holding areas listed before it.
var Order = []Name{SingBox, Rear, Caddy, AWG, Mesh, Olcrtc, Firewall}

func rank(n Name) int {
	for i, o := range Order {
		if o == n {
			return i + 1
		}
	}
	return 0
}

var (
	// ErrOrder: the context already holds an area of equal or higher rank.
	ErrOrder = errors.New("locks: acquisition out of order")
	// ErrUnknown: the name is not one of the areas in Order.
	ErrUnknown = errors.New("locks: unknown area")
)

type heldKey struct{}

// heldRank is the highest rank held through ctx (0: none).
func heldRank(ctx context.Context) int {
	r, _ := ctx.Value(heldKey{}).(int)
	return r
}

// Registry holds one mutex per area. The zero value is ready to use; it must not be copied.
type Registry struct {
	mu    sync.Mutex
	slots map[Name]chan struct{}
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

func (r *Registry) slot(n Name) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slots == nil {
		r.slots = map[Name]chan struct{}{}
	}
	c, ok := r.slots[n]
	if !ok {
		c = make(chan struct{}, 1)
		r.slots[n] = c
	}
	return c
}

// Lock acquires area n, waiting until it is free or ctx is done. It returns a context derived
// from ctx that records the held area (pass it to code that may nest a later area) and the
// unlock function (safe to call more than once).
func (r *Registry) Lock(ctx context.Context, n Name) (context.Context, func(), error) {
	rk := rank(n)
	if rk == 0 {
		return ctx, func() {}, fmt.Errorf("%w %q", ErrUnknown, n)
	}
	if held := heldRank(ctx); held >= rk {
		return ctx, func() {}, fmt.Errorf("%w: %q requested while holding %q", ErrOrder, n, Order[held-1])
	}
	c := r.slot(n)
	// A free slot wins over an already-done context only when the select picks it; check the
	// context first so a cancelled caller deterministically gets nothing.
	if err := ctx.Err(); err != nil {
		return ctx, func() {}, err
	}
	select {
	case c <- struct{}{}:
	case <-ctx.Done():
		return ctx, func() {}, ctx.Err()
	}
	var once sync.Once
	return context.WithValue(ctx, heldKey{}, rk), func() { once.Do(func() { <-c }) }, nil
}

// LockAll acquires every named area in rank order (duplicates ignored). On failure it releases
// what it took and returns the error.
func (r *Registry) LockAll(ctx context.Context, names ...Name) (context.Context, func(), error) {
	want := map[Name]bool{}
	for _, n := range names {
		if rank(n) == 0 {
			return ctx, func() {}, fmt.Errorf("%w %q", ErrUnknown, n)
		}
		want[n] = true
	}
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	cur := ctx
	for _, n := range Order {
		if !want[n] {
			continue
		}
		next, unlock, err := r.Lock(cur, n)
		if err != nil {
			release()
			return ctx, func() {}, err
		}
		unlocks = append(unlocks, unlock)
		cur = next
	}
	var once sync.Once
	return cur, func() { once.Do(release) }, nil
}
