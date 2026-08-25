package addon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// This file is the instance pool (M66.5): what a redirect-class invocation gets
// instead of a module built from nothing, and what makes reusing one safe.
//
// # Why there is a pool at all, having decided there would not be
//
// D319 declined pooling on M60's instantiation figure — ~1.6 ms, measured on an
// idle VM with nothing else running. M66 then put an add-on on the redirect path
// and the same fixture, doing nothing but parsing its decision and allowing it,
// cost 44.89 ms at the generator's p99 against a 20 ms target: 11.05 ms per
// invocation, of which the guest's own code is a JSON parse. The rest is
// allocating 3.4 MB of linear memory, running a Go runtime's package
// initialization inside it, and throwing all of it away. D333 reverses D319 on
// that measurement.
//
// # Reuse is a security boundary, and the reset is the host's
//
// A destroyed instance cannot leak one visitor's residue to the next. A pooled
// one can, by construction: the guest's linear memory *is* its state, and handing
// the same instance to the next redirect hands over whatever the last one left in
// it. M61's privacy argument does not cover this — it is about what a module is
// **given**, not about what it **keeps**.
//
// So the host resets it, and the reset is not the guest's to cooperate with. The
// whole of an instance's mutable state lives in one linear memory; this file
// copies that memory the moment package initialization finishes and writes the
// copy back over it before the instance is handed out again. A module that stored
// the last visitor's destination in a package-level variable reads an empty one,
// because the bytes that held it are the bytes `_initialize` left there.
// TestAPooledInstanceCannotSeeTheLastRedirect drives two redirects through one
// entry and asserts exactly that, and it fails when the restore is removed.
//
// Three things bound what that claim rests on, and each is enforced rather than
// argued:
//
//   - **An instance that did not return cleanly is never reused.** A kill closes
//     the module underneath the call (WithCloseOnContextDone), a trap leaves it in
//     a state nothing here can characterise, and both mean the image cannot be
//     said to describe it. [Host.releaseInstance] closes such an entry instead of
//     returning it, which is also what stops a module being killed repeatedly from
//     filling the pool with dead entries.
//   - **An instance whose memory grew is never reused.** WebAssembly memory cannot
//     shrink, so an image taken at 52 pages cannot restore an instance now holding
//     61 — the extra pages would keep the guest's own bytes and the guest's
//     allocator would grow again on the next use, without bound. A size that
//     changed is an eviction.
//   - **The image is a copy.** api.Memory.Read hands back a window onto the live
//     buffer rather than a copy of it, so an image taken that way would be the
//     memory it is supposed to restore, and the restore would be a no-op that
//     looked like a reset.
//
// What is *not* reset is the eight mutable WebAssembly globals a Go module
// carries — the stack pointer, the goroutine register and six scratch slots.
// Nothing here can write them: wazero exposes globals through the module's export
// section and this toolchain exports none. They are safe to leave because the
// only state they hold between calls is the resting stack pointer, which is the
// same value after a clean return from any exported function as it is after
// `_initialize` — and *after a clean return* is the only state an entry is ever
// pooled in, per the first bullet above.
//
// # What is pooled, and what is not
//
// Both redirect classes, inline and observing. They pay the same startup on the
// same slots — F326 was found in both call sites — so serving only the inline
// class would be a choice, and there is nothing to argue for it: an observing
// invocation is off the request path but it is not free, and it holds one of the
// same [maxConcurrentRoutes] slots while it starts.
//
// **Add-on pages are not pooled**, and that is this milestone's scope rather than
// a claim that they should not be. A page request has a 250 ms budget where a
// redirect has 20 ms, waits for its slot rather than skipping, and carries a
// request record where a redirect carries a link's own facts. http.go's own
// account of one instance per request is still what happens there.
//
// # The two bounds, and why neither is the slot budget
//
// [maxConcurrentRoutes] bounds how many invocations are **in flight**. It already
// bounds three things under a name that says one (F324) and this file does not
// make it four: the pool takes nothing from it, waits on nothing of it, and an
// invocation that could not get a slot never reaches this file at all.
//
// What is new is that an instance now exists while nothing is using it, so the
// memory ceiling gains a second term. LINKCTRL_ADDON_POOL_SIZE bounds how many
// idle instances this host keeps across every add-on, and LINKCTRL_ADDON_POOL_TTL
// bounds how long one is kept before it is closed for lack of traffic. Sixteen in
// flight plus eight kept warm, each held to 8 MiB, is the 192 MiB the documents
// state.
//
// **The image is a second copy and the documents say so**, because it would
// otherwise be memory an operator sizing a host does not know about. Every live
// instance carries one, host-side, for as long as it exists — bounded by the same
// [maxGuestMemoryPages] the guest is, since an instance whose memory grew past its
// image is evicted rather than re-imaged. So the *resident* worst case is that
// ceiling twice: what the guest holds and what this host holds to reset it with.
// It is not on the guest bound — the runtime limit is per instance and this is
// ordinary Go memory — which is exactly why it needs saying out loud rather than
// being left to be inferred from a number that does not cover it.

// The two ways acquiring an instance fails for a reason that is not the module's
// code. Neither reaches an operator as itself: the redirect path logs through
// [Host.redirectFailed] like every other failure and the redirect completes
// without the add-on.
var (
	// errNoHostState is a host with no load-time state for this add-on, which is a
	// host being closed underneath a redirect.
	errNoHostState = errors.New("the add-on has no host state")
	// errNoGuestMemory is a module whose memory cannot be read, so no image can be
	// taken and reuse cannot be made safe. It is refused rather than pooled
	// unreset.
	errNoGuestMemory = errors.New("the add-on's module exports no readable memory")
)

// DefaultPoolSize is how many idle add-on instances this host keeps, across every
// add-on, unless an operator says otherwise with LINKCTRL_ADDON_POOL_SIZE.
//
// **It is not a concurrency bound and it is deliberately not sixteen.** What
// bounds invocations in flight is [maxConcurrentRoutes], which this file does not
// touch; this bounds only what is held at rest, and the two are added rather than
// merged — see the file comment.
//
// Eight is measured into. The redirect path's steady-state demand for instances is
// its arrival rate times how long one is held, and the k6 run in docs/slo.md holds
// one for a mean of 451 µs against the 11.05 ms M66 measured — so 2,000 redirects a
// second want about one instance at any moment. Eight is several times that and
// covers the bursts a Poisson arrival pattern produces around it: in that run nine
// redirects of 240,001 found no instance slot and none found the pool short, so the
// eviction path this bound guards did not run at all. Above eight the return is
// nothing and the cost is 8 MiB a slot, which is why the number is small rather
// than generous.
const DefaultPoolSize = 8

// DefaultPoolTTL is how long an idle instance is kept before it is closed, unless
// an operator says otherwise with LINKCTRL_ADDON_POOL_TTL.
//
// It is what keeps the idle cost proportional to traffic rather than to peak
// traffic that has since stopped. Without it a burst at midnight leaves eight
// instances holding guest memory until the process ends, which on an instance with
// one visitor an hour is the whole of the pool's cost and none of its benefit.
//
// A minute, and the choice is loose on purpose: the number does not have to be
// right, it has to be finite. Too short costs an instantiation on the next
// redirect, which is what every redirect cost before this milestone; too long
// costs memory that is already bounded by [DefaultPoolSize]. Under any traffic
// worth pooling for, an entry is taken again within milliseconds and the sweep
// never sees it idle.
const DefaultPoolTTL = time.Minute

// poolEntry is one instance kept between invocations.
type poolEntry struct {
	mod api.Module
	// addon and class are which pool this entry belongs to. An entry never crosses
	// classes: package initialization runs once per entry and the redirect-safe
	// subset applies to it, so an entry whose init ran as an observer — where
	// storage is allowed — must not later serve an inline invocation, where it is
	// not. Two idle sets per add-on rather than one is the whole of the cost.
	addon string
	class string
	// name is the module's instance name, and it is also the key its hostState is
	// registered under: dispatch resolves a host call by the *calling* module's
	// name, so this string is what scopes an invocation to the instance answering
	// it. It outlives one invocation now, which is why release puts a resting state
	// back rather than leaving the last redirect's there.
	name string
	// image is linear memory as `_initialize` left it, copied out of the live
	// buffer. Restoring it is what makes reuse safe; see the file comment.
	image []byte
	// idleAt is when this entry was last returned to the pool, read only by the
	// sweep.
	idleAt time.Time
	// gen is the pool generation this instance was made in, and it is what makes a
	// drain reach an instance that was **in flight** when the drain happened.
	//
	// Draining empties the idle set, which by construction does not contain the
	// entry an invocation is holding. Without this, [Host.releaseInstance] would
	// return that entry to the pool a moment later and the instance a drain existed
	// to destroy would go on serving — for up to [DefaultPoolTTL], carrying whatever
	// its package initialization read. That is the exact case
	// [Host.SaveSettings]'s drain is for, so the drain has to survive the gap
	// between take and put rather than only the moment it runs.
	gen uint64
}

// addonPool is one add-on's idle instances.
//
// Per add-on because an instance is a module: an entry made from one add-on's
// wasm can serve only that add-on. The *budget* is not per add-on — see
// [Host.idleInstances] — because what an operator sizes a host by is the memory
// this process holds, not how it is divided.
type addonPool struct {
	mu   sync.Mutex
	idle []*poolEntry
	// gen counts drains. An entry made under an older one is not returned to the
	// idle set — see [poolEntry.gen].
	gen uint64
}

// generation is what a new entry is stamped with.
func (p *addonPool) generation() uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

// take removes the most recently returned entry, or nil.
//
// Most recent rather than oldest, so that a pool holding more entries than the
// traffic needs lets the surplus age out under [Host.sweepPools] instead of
// keeping every entry equally warm and equally alive.
func (p *addonPool) take() *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) == 0 {
		return nil
	}
	e := p.idle[len(p.idle)-1]
	p.idle = p.idle[:len(p.idle)-1]
	return e
}

// put returns an entry to the idle set, and reports whether it took it.
//
// It refuses an entry made before the last drain. The caller closes what is
// refused, which is the same thing it does with an entry the pool is full for.
func (p *addonPool) put(e *poolEntry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.gen != p.gen {
		return false
	}
	e.idleAt = time.Now()
	p.idle = append(p.idle, e)
	return true
}

// drain removes and returns every idle entry, which is what closing a host and
// removing an add-on both need.
func (p *addonPool) drain() []*poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.idle
	p.idle = nil
	// Every instance now in flight was made under the old number and will not be
	// taken back. Without this the drain reaches only what happens to be resting at
	// the moment it runs, which is the half of the set that matters least: an
	// add-on under traffic has its instances *out*.
	p.gen++
	return out
}

// expired removes and returns the entries idle for longer than ttl.
func (p *addonPool) expired(ttl time.Duration, now time.Time) []*poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*poolEntry
	kept := p.idle[:0]
	for _, e := range p.idle {
		if now.Sub(e.idleAt) >= ttl {
			out = append(out, e)
			continue
		}
		kept = append(kept, e)
	}
	// The tail is cleared rather than left, because a *poolEntry still reachable
	// through the backing array is 8 MiB of guest memory the garbage collector
	// cannot reason about and this is the one place entries leave the slice.
	for i := len(kept); i < len(p.idle); i++ {
		p.idle[i] = nil
	}
	p.idle = kept
	return out
}

// acquireInstance returns an instance of one add-on's module, from the pool if
// there is one and freshly instantiated if there is not.
//
// The caller holds a slot and is inside its own instantiation budget; ctx is that
// budget, so an instance that will not start in time fails here exactly as it did
// before there was a pool. A pool hit does not touch ctx at all, which is the
// whole point: there is nothing to wait for.
//
// The returned entry carries a **resting** state — no decision, no event — and
// the caller attaches the invocation's own with [Host.attachState]. That split is
// what stops a module's package initialization, which runs during instantiation
// and only once per entry now, from seeing a redirect that was merely the first
// one to need an instance.
func (h *Host) acquireInstance(ctx context.Context, l *Loaded, class string) (*poolEntry, error) {
	name := l.Manifest.Name
	if p := h.current().pools[poolKey(name, class)]; p != nil {
		if e := p.take(); e != nil {
			h.idleInstances.Add(-1)
			return e, nil
		}
	}
	rest := h.hostState(name)
	if rest == nil {
		return nil, errNoHostState
	}
	inst := name + "#" + strconv.FormatUint(h.instances.Add(1), 10)
	// Registered before instantiation, because package initialization runs *during*
	// it and a guest calling a host function from init must find a state. What it
	// finds is the resting one, which for the inline class still carries the
	// redirect-safe subset: an instance made for the redirect path is held to that
	// subset from its first instruction, whether or not a redirect is attached yet.
	h.setState(inst, rest.forPool(class))
	mod, err := h.runtime.InstantiateModule(ctx, l.compiled, guestModuleConfig(inst))
	if err != nil {
		if mod != nil {
			_ = mod.Close(context.WithoutCancel(ctx))
		}
		h.clearState(inst)
		return nil, err
	}
	mem := mod.Memory()
	if mem == nil {
		// A module with no memory cannot be reset, so it is not poolable and the
		// honest answer is to refuse it rather than to reuse it. Nothing this
		// toolchain produces reaches here — every fixture exports one — and the
		// branch exists because "it always has memory" is an assumption about
		// somebody else's compiler.
		_ = mod.Close(context.WithoutCancel(ctx))
		h.clearState(inst)
		return nil, errNoGuestMemory
	}
	live, ok := mem.Read(0, mem.Size())
	if !ok {
		_ = mod.Close(context.WithoutCancel(ctx))
		h.clearState(inst)
		return nil, errNoGuestMemory
	}
	// **Copied, not aliased.** Read returns a window onto the buffer this image is
	// meant to restore; keeping it would make the restore a write of memory onto
	// itself, which resets nothing and looks exactly like a reset that works.
	image := make([]byte, len(live))
	copy(image, live)
	// Stamped from the pool as it is now. A drain between here and the release that
	// follows makes this entry stale, which is what closes it instead of pooling it.
	return &poolEntry{
		mod: mod, addon: name, class: class, name: inst, image: image,
		gen: h.current().pools[poolKey(name, class)].generation(),
	}, nil
}

// poolKey names one add-on's idle set for one class.
func poolKey(addon, class string) string { return addon + "\x00" + class }

// releaseInstance gives an instance back, and decides whether it may be reused.
//
// clean is the caller's answer to *did this invocation return normally* — not
// *did the add-on like the answer*. A veto is clean; a kill, a trap and a module
// that could not be called are not, and each of those closes the entry. That is
// the third bullet of m66.5: a killed instance is evicted rather than returned, so
// a module overrunning on every invocation degrades to what M66 did and cannot
// poison the pool with entries that are already closed.
func (h *Host) releaseInstance(ctx context.Context, e *poolEntry, clean bool) {
	if !clean {
		h.closeInstance(ctx, e)
		return
	}
	mem := e.mod.Memory()
	if mem == nil || int(mem.Size()) != len(e.image) {
		// Memory grew during the invocation, so the image no longer describes the
		// instance and WebAssembly gives no way to make it: memory does not shrink.
		// Closing is the only reset left.
		h.closeInstance(ctx, e)
		return
	}
	if !mem.Write(0, e.image) {
		h.closeInstance(ctx, e)
		return
	}
	// The resting state goes back before the entry does, so that an idle instance
	// is not sitting in the pool with the last visitor's redirect still resolvable
	// under its name.
	rest := h.hostState(e.addon)
	if rest == nil {
		h.closeInstance(ctx, e)
		return
	}
	h.setState(e.name, rest.forPool(e.class))
	if h.idleInstances.Add(1) > int64(h.poolSize) {
		h.idleInstances.Add(-1)
		h.closeInstance(ctx, e)
		return
	}
	// From the set as it is *now*, not from the one the invocation started under.
	// An add-on removed while this instance was in flight has no pool here any
	// more, and the nil branch below is what makes the entry close instead of
	// being returned to a set nothing will ever drain again (M67).
	p := h.current().pools[poolKey(e.addon, e.class)]
	if p == nil {
		h.idleInstances.Add(-1)
		h.closeInstance(ctx, e)
		return
	}
	if !p.put(e) {
		// Drained while this invocation was in flight. The entry is destroyed for the
		// reason the drain destroyed the resting ones — see [poolEntry.gen].
		h.idleInstances.Add(-1)
		h.closeInstance(ctx, e)
	}
}

// closeInstance destroys one entry and forgets its state.
func (h *Host) closeInstance(ctx context.Context, e *poolEntry) {
	_ = e.mod.Close(context.WithoutCancel(ctx))
	h.clearState(e.name)
}

// drainPool closes every idle instance of one add-on.
//
// **M67 consumes this**, and the seam turned out to be exactly the right shape:
// removing an add-on has to end the instances made from its module, and a pool is
// a cache whose invalidation is where the defect would live — without this, an
// uninstalled add-on's wasm would keep running on the redirect path until the
// entries aged out. What removal adds is the map to drain *from*, which is the
// set being replaced rather than the current one: by the time this is called the
// new set no longer has the add-on's pools in it, so reading them off the host
// would find nothing and close nothing.
func (h *Host) drainPool(ctx context.Context, addon string, pools map[string]*addonPool) {
	for _, class := range []string{ClassInline, ClassObserve} {
		p := pools[poolKey(addon, class)]
		if p == nil {
			continue
		}
		for _, e := range p.drain() {
			h.idleInstances.Add(-1)
			h.closeInstance(ctx, e)
		}
	}
}

// startPoolSweep runs the idle sweep, and does nothing on a host with no pools.
//
// **Called again by Install** (M67), because a host that booted with no pooled
// add-on has no sweep and an add-on installed into it brings the first pool. It
// is idempotent on the sweep already running, which is what `h.poolStop != nil`
// answers: the alternative — starting one per install — would leave a goroutine
// per add-on ever installed, closing entries a sibling sweep already closed.
// Called under installMu, which is what makes reading poolStop safe.
func (h *Host) startPoolSweep(ctx context.Context) {
	if h.poolStop != nil || h.poolTTL <= 0 {
		return
	}
	if len(h.current().pools) == 0 {
		return
	}
	// Detached from Open's context and made cancellable again, for the two reasons
	// startObserving does it: a boot context is done before the first redirect, and
	// Close must be able to end the sweep rather than wait one out.
	sweep, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.poolStop = cancel
	// Half the TTL, so an entry is closed within 1.5x of it rather than within 2x,
	// and the sweep costs one wakeup a period on a host where nothing is idle.
	interval := h.poolTTL / 2
	if interval <= 0 {
		interval = time.Millisecond
	}
	h.poolWG.Add(1)
	go func() {
		defer h.poolWG.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				h.sweepPools(sweep)
			case <-sweep.Done():
				return
			}
		}
	}()
}

// sweepPools closes the entries nothing has wanted for a TTL.
func (h *Host) sweepPools(ctx context.Context) {
	now := time.Now()
	for _, p := range h.current().pools {
		for _, e := range p.expired(h.poolTTL, now) {
			h.idleInstances.Add(-1)
			h.closeInstance(ctx, e)
			h.log.Debug("closed an idle add-on instance",
				slog.String("addon", e.addon),
				slog.String("class", e.class),
				slog.String("ttl", h.poolTTL.String()))
		}
	}
}

// stopPoolSweep ends the sweep and closes every entry still held.
func (h *Host) stopPoolSweep(ctx context.Context) {
	if h.poolStop != nil {
		h.poolStop()
		h.poolStop = nil
	}
	h.poolWG.Wait()
	for _, p := range h.current().pools {
		for _, e := range p.drain() {
			h.idleInstances.Add(-1)
			h.closeInstance(ctx, e)
		}
	}
}

// idleInstanceCount is how many instances are held at rest, across every add-on.
// Read by the tests that assert what the pool holds; nothing in the redirect path
// calls it.
func (h *Host) idleInstanceCount() int64 { return h.idleInstances.Load() }

// setState registers one instance's hostState under its module name.
func (h *Host) setState(instance string, st *hostState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.states == nil {
		h.states = make(map[string]*hostState)
	}
	h.states[instance] = st
}

// clearState forgets one instance's hostState.
func (h *Host) clearState(instance string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, instance)
}

// poolSizeFrom and poolTTLFrom apply the defaults, so that a zero in Options is a
// caller that did not care rather than a caller asking for no pool.
func poolSizeFrom(n int) int {
	if n <= 0 {
		return DefaultPoolSize
	}
	return n
}

func poolTTLFrom(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultPoolTTL
	}
	return d
}
