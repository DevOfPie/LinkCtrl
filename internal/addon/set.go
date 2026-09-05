package addon

import (
	"sync"
	"sync/atomic"
)

// This file is what M67 changed about the shape of a host: the installed set
// stopped being written once at boot.
//
// # Why a snapshot rather than a lock
//
// Until this milestone `loaded`, `inline` and `pools` were assigned inside
// [Open] and never again, so every reader could take them as constants. That is
// what let [Host.HasInline] be a field read on the hot path of every redirect,
// and it is what let the pool map be read without a lock from
// [Host.acquireInstance]. Runtime install and removal end that premise: the set
// changes while requests are being served.
//
// The obvious repair — an RWMutex over the three fields — would put a lock
// acquisition on every redirect served by an instance that has no add-ons at
// all, which is the cost m60.md promised nobody would pay. So the three fields
// become one immutable value behind an [atomic.Pointer]: a reader loads the
// pointer once and works from a set that cannot change underneath it, and a
// writer builds a whole new set and stores it. Reading costs one atomic load;
// writing costs a copy of a slice of a handful of elements, on an operation an
// operator performs by hand.
//
// **The three have to move together.** A redirect resolves an add-on out of
// `inline` and then looks its pool up in `pools`, and an add-on present in one
// and absent from the other is the defect a per-field lock would still allow.
// One value makes that unrepresentable rather than merely unlikely.
//
// # What it does not do
//
// It does not make an in-flight invocation safe on its own. Swapping the set
// stops the *next* caller from finding a removed add-on; the one already inside
// a guest call is holding a *Loaded that names a module removal is about to
// close, and that is [addonLive]'s job below.

// addonSet is everything this host is running, as one value that is never
// mutated after it is stored.
type addonSet struct {
	// loaded is every add-on that started, in discovery order.
	loaded []Loaded
	// inline is the subset holding `redirect.inline`, resolved once here rather
	// than filtered per redirect, for the reason grants are resolved once at load.
	inline []Loaded
	// observers is the subset holding `redirect.observe`. It was a slice captured
	// by the worker goroutine before this milestone; the worker now reads it from
	// the current set on every event, because an add-on installed after the worker
	// started must be shown redirects and one removed must not.
	observers []Loaded
	// discovered is every add-on **installed on disk**, by name, whether or not it
	// loaded — which is a different question from `loaded` and is the one an
	// orphan check has to ask. A degrade-class add-on whose module fails to
	// instantiate, or whose manifest stops validating while its schema survives
	// from an earlier good version, is still installed; subtracting `loaded`
	// called its schema an orphan and M68's manager then offered a
	// still-installed add-on's rows for purge, which is F281 and which was driven
	// end to end. D428 puts the distinction here, at the source, so the boot
	// warning, the manager's list and the purge confirmation inherit one answer
	// instead of three.
	//
	// **Every path that leaves an add-on on disk has to keep this true.** That is
	// the cost the decision names: a failure path added later which forgets to
	// record what it found brings the defect back through a door nobody is
	// watching. There is one recorder — [Open]'s loop — and two maintainers,
	// [Host.Install] and [Host.Remove].
	discovered []string
	// pools is one idle set per (add-on, class) on either redirect class. Entries
	// are carried across a set change for the add-ons that survive it: a pool is a
	// live object holding open wasm instances, and rebuilding the map must not
	// silently orphan them.
	pools map[string]*addonPool
}

// emptyAddonSet is what a nil host, and a host between construction and its
// first store, answers with. A value rather than a nil pointer so that every
// reader can dereference without asking.
var emptyAddonSet = &addonSet{}

// current is the set as it is right now. Nil-safe, because a nil *Host is the
// ordinary state of an instance that configured no add-ons directory.
func (h *Host) current() *addonSet {
	if h == nil {
		return emptyAddonSet
	}
	if s := h.set.Load(); s != nil {
		return s
	}
	return emptyAddonSet
}

// newAddonSet derives the three subsets from one ordered list of add-ons.
//
// discovered is every add-on installed on disk, which is **not** derivable from
// loaded and is why it is a parameter: the add-ons this list is missing are
// exactly the ones the caller has to have remembered.
//
// prev is the pool map being replaced, or nil. A pool survives for an add-on
// still in the list, so an install does not throw away the warm instances of the
// add-ons it arrived beside; a pool for an add-on no longer in the list is left
// behind here and drained by the caller, which is the only place that knows the
// context to close instances with.
func newAddonSet(loaded []Loaded, discovered []string, prev map[string]*addonPool) *addonSet {
	s := &addonSet{loaded: loaded, discovered: discovered}
	for _, l := range loaded {
		if l.compiled == nil {
			continue
		}
		if l.RunsInline() {
			s.inline = append(s.inline, l)
		}
		if l.ObservesRedirects() {
			s.observers = append(s.observers, l)
		}
		for class, declared := range map[string]bool{
			ClassInline: l.RunsInline(), ClassObserve: l.ObservesRedirects(),
		} {
			if !declared {
				continue
			}
			if s.pools == nil {
				s.pools = make(map[string]*addonPool, 2*len(loaded))
			}
			key := poolKey(l.Manifest.Name, class)
			if p := prev[key]; p != nil {
				s.pools[key] = p
				continue
			}
			s.pools[key] = &addonPool{}
		}
	}
	return s
}

// store publishes a set and returns it.
func (h *Host) store(s *addonSet) *addonSet {
	h.set.Store(s)
	return s
}

// addonLive is how many invocations of one add-on are inside a guest call, and
// whether the add-on is being removed.
//
// **This is the half a snapshot cannot do.** Removing an add-on swaps the set so
// that nothing new can resolve it, and then has to answer a second question:
// what about the request that resolved it a microsecond earlier and is now
// inside `InstantiateModule`? Closing its module underneath it is a trap the
// visitor sees; waiting for it forever is a removal an operator watches hang.
//
// So an invocation announces itself with [addonLive.enter] and leaves with
// [addonLive.leave], and removal [addonLive.seal]s: after the seal no new
// invocation is admitted, and the channel `seal` returns closes when the last
// one already inside has left. m67.md's *complete or are interrupted* is that
// channel raced against a bounded grace — the choice, and why it is bounded, is
// in lifecycle.go.
//
// A [sync.WaitGroup] is not this. `Add` from a reader racing `Wait` from the
// remover is exactly the case its documentation rules out, and the failure mode
// is `Wait` returning early — which here means closing a module with a guest
// running in it. A flag and a counter under one mutex have no such rule.
type addonLive struct {
	mu     sync.Mutex
	n      int
	sealed bool
	quiet  chan struct{}
}

func newAddonLive() *addonLive { return &addonLive{quiet: make(chan struct{})} }

// enter admits one invocation, or reports that the add-on is going away.
//
// Nil-safe: a Loaded built by a test rather than by loadOne has no counter, and
// the honest answer for one is that nothing is removing it.
func (a *addonLive) enter() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealed {
		return false
	}
	a.n++
	return true
}

// leave releases one, and wakes a sealer waiting on the last of them.
func (a *addonLive) leave() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n--
	if a.sealed && a.n == 0 {
		a.closeQuiet()
	}
}

// seal refuses further invocations and answers with the channel that closes when
// the ones already running have finished. Already-sealed is not an error: a
// second removal of the same add-on is a caller that lost a race, and it waits
// on the same channel.
func (a *addonLive) seal() <-chan struct{} {
	if a == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sealed = true
	if a.n == 0 {
		a.closeQuiet()
	}
	return a.quiet
}

// closeQuiet closes the channel once. Called under a.mu.
func (a *addonLive) closeQuiet() {
	select {
	case <-a.quiet:
	default:
		close(a.quiet)
	}
}

// inFlight is how many invocations of this add-on are inside a guest call. Read
// by the tests that assert removal waited; nothing in the serving paths calls it.
func (a *addonLive) inFlight() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// setPointer is the field's type in [Host], named so that the struct reads as
// what it is rather than as a generic.
type setPointer = atomic.Pointer[addonSet]
