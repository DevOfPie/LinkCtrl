package addon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
)

// This file is the redirect limb (M66): the two classes an add-on may declare
// against the redirect path, what each of them may do, and what bounds them.
//
// # The owner's boundary, and what it makes this file responsible for
//
// *We are only responsible for maintaining the core redirect promise; if an
// add-on ruins that, it is on the add-on.* So latency stops being this product's
// to defend the moment an operator puts a module on the path — and availability
// does not. The whole of the difference is [Host.Inline]: a module that is slow
// makes redirects slow and that is the operator's problem with their add-on, and
// a module that never returns is killed, the redirect completes without it, and
// the kill is counted against the add-on by name.
//
// The published measurement in docs/slo.md is therefore restated as *core, with
// no inline add-on on the path*, in the same diff that made an inline add-on
// possible. A claim about to stop being unconditionally true is edited before the
// capability ships.
//
// **And the series behind that claim is made core's rather than merely relabelled**:
// RedirectHandler.ServeHTTP times this call and subtracts it from what it hands
// linkctrl_redirect_duration_seconds, so core's curve and each add-on's are two
// readings that can disagree. Without that, the two histograms would nest — the
// redirect one containing every invocation this file made, including the whole
// deadline of one that was killed — and an operator running an inline add-on
// would have no core curve left to compare against. Per-module attribution is the
// owner's second requirement on this answer, and it needs the subtraction to mean
// anything.
//
// # Two classes, and only one of them is on the path
//
// **redirect.observe** runs after the visitor has been answered *and* after the
// click is durable, from the analytics pipeline's own goroutine — see
// [Host.Observe]. It can delay nothing because nothing waits for it: the queue
// is bounded and drops rather than blocks, which is the same contract the click
// ingester it is fed from already keeps with the hot path.
//
// **redirect.inline** runs inside RedirectHandler.ServeHTTP, at one named point:
// after the destination is decided and **before the gates** that spend a link's
// budget. Before the gates is not a detail — a veto after a one-time link's
// single click had been spent would refuse the visitor and consume the link, and
// m66.md names that exact hazard.
//
// # What an inline invocation costs, and why there is no pool
//
// A fresh instance per invocation, exactly as a route gets one (D260), closed
// when the invocation ends. Measured rather than assumed —
// TestAnInlineInvocationCostsAnInstantiation is the number — and the reasoning is
// in decisions.md: a pool would be an order of magnitude cheaper per call and it
// would put an add-on's guest memory across two visitors' redirects, on the one
// path in this product where the host hands a module something *while somebody
// waits*. It would also raise the guest-memory ceiling four documents state,
// because a pooled instance holds its memory whether or not anything is using it.
// The cost of not pooling is paid by the operator who installed an inline add-on
// and is visible per module in the histogram this file writes.
//
// The instance budget is the host's existing one — [maxConcurrentRoutes] slots,
// shared with add-on pages and with out-of-band observation — and it is taken
// **without waiting**. A redirect that
// cannot get a slot is served without the add-on and counted as throttled. That
// is the backpressure that makes "core availability survives a slow add-on" true
// rather than hoped for: sixteen slow invocations do not become sixteen thousand.

// The three grants this file branches on, named for the reason
// [PermissionRoutes] is: a second spelling of a permission is the drift a closed
// vocabulary exists to stop, and a test holds each against [abi.Permissions].
const (
	// PermissionRedirectObserve is out-of-band observation.
	PermissionRedirectObserve = "redirect.observe"
	// PermissionRedirectInline is running on the path itself.
	PermissionRedirectInline = "redirect.inline"
	// PermissionRewriteQuery is altering the destination's query, and it is a
	// token of its own on top of the one above — D317.
	PermissionRewriteQuery = "redirect.rewrite_query"
)

// The two class labels, which are metric label values and therefore a closed
// vocabulary rather than free text.
const (
	ClassInline  = "inline"
	ClassObserve = "observe"
)

// DefaultInlineDeadline is how long an inline add-on may hold a redirect open,
// unless an operator says otherwise with LINKCTRL_ADDON_INLINE_DEADLINE.
//
// **Measured into, not chosen.** The upcoming-decisions entry M66 was planned
// with fixed the shape of this answer a phase in advance: one instance-wide knob,
// no per-add-on override until a real case argues for one, and a value taken from
// this milestone's own runs rather than from a guess. The runs are in
// docs/slo.md and the arithmetic is D318.
//
// What the budget has to cover is an instantiation *and* the guest's own work,
// because package initialization runs during instantiation under
// -buildmode=c-shared — the `spinning` fixture is a module that never finishes
// loading, and a deadline that started after the instance existed would not bound
// it at all. Measured on this machine, 2026-08-22: a fixture that reads its
// decision, probes six host functions and writes a query rewrite costs a mean of
// **3.27 ms** and a worst-of-twenty of **4.34 ms** end to end, against M60's
// separately measured ~1.6 ms to instantiate the same class of module. So 25 ms is
// roughly six times a module doing real work.
//
// **That figure is taken without the race detector**, which costs this measurement
// an order of magnitude — the same effect D225 records for the load path. The test
// reports the number either way and compares it against this constant only on a
// plain build; racecost_test.go says why.
//
// It is deliberately **larger than the 20 ms cached-redirect target** and that is
// not a contradiction. The target is core's, measured with nothing on the path;
// the deadline is the point at which the host stops waiting for somebody else's
// code. Setting it under the target would make the host kill add-ons that were
// working, which trades an operator's feature for a number that no longer
// describes their instance anyway.
const DefaultInlineDeadline = 25 * time.Millisecond

// RedirectDecision is where a visitor is about to be sent, as an inline add-on
// sees it. The ABI record is abi.Records' RedirectDecision and the field names
// here are that record's.
//
// Nothing on it is derived from the visitor, which is a bound rather than an
// omission: an inline module holds somebody's request open, and what it is
// entitled to know is the decision it is being asked about. Watching visitors is
// the observe class's, off the path, under a grant an operator declares
// separately.
type RedirectDecision struct {
	LinkID      uuid.UUID `json:"link_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Alias       string    `json:"alias"`
	Destination string    `json:"destination"`
}

// RedirectAnswer is what an inline module wrote back.
type RedirectAnswer struct {
	Verdict string `json:"verdict,omitempty"`
	Rewrite bool   `json:"rewrite,omitempty"`
	Query   string `json:"query,omitempty"`
}

// RedirectEvent is one redirect this instance served, as an observing add-on
// sees it. Every field is one click_events may carry, which abi_test.go asserts
// against the migration rather than against this struct.
type RedirectEvent struct {
	LinkID       string `json:"link_id"`
	WorkspaceID  string `json:"workspace_id"`
	OccurredAt   string `json:"occurred_at"`
	VisitorHash  string `json:"visitor_hash"`
	IsFirstVisit bool   `json:"is_first_visit"`
	Country      string `json:"country"`
	Device       string `json:"device"`
	Browser      string `json:"browser"`
	OS           string `json:"os"`
	Language     string `json:"language"`
	ReferrerHost string `json:"referrer_host"`
	IsBot        bool   `json:"is_bot"`
}

// InlineResult is what the redirect path gets back from the inline classes.
//
// The zero value is *nothing happened*, which is what an instance with no inline
// add-on produces and what [Host.Inline] returns on a nil host without touching
// anything.
type InlineResult struct {
	// Vetoed is a module refusing this redirect. The handler answers the gate
	// refusal with it, and nothing about the add-on reaches the visitor.
	Vetoed bool
	// Destination is where the visitor goes, which is the one that was handed in
	// unless a module rewrote the query and held the grant that costs.
	Destination string
	// Rewritten is whether Destination differs from what was handed in. The
	// handler does not need it; the tests do, and so does the log line that says a
	// module changed where somebody was sent.
	Rewritten bool
}

// RunsInline reports whether this add-on holds the grant that puts it on the
// redirect path.
func (l Loaded) RunsInline() bool { return l.grants.Has(PermissionRedirectInline) }

// ObservesRedirects reports whether this add-on holds the out-of-band grant.
func (l Loaded) ObservesRedirects() bool { return l.grants.Has(PermissionRedirectObserve) }

// HasInline reports whether anything on this instance runs on the redirect path.
//
// It is the check the redirect handler makes on **every** redirect, so it is a
// field read and nothing else: no lock, no allocation, no walk of the loaded set.
// Nil-safe, because an instance with no add-ons directory has no host at all and
// that is the case this has to cost nothing in.
func (h *Host) HasInline() bool { return h != nil && len(h.inline) > 0 }

// InlineAddons is every add-on on the redirect path, in load order. The boot log
// and M68's manager read it; the path itself does not.
func (h *Host) InlineAddons() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.inline))
	for _, l := range h.inline {
		out = append(out, l.Manifest.Name)
	}
	return out
}

// ObservingAddons is the same for the out-of-band class.
func (h *Host) ObservingAddons() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, l := range h.loaded {
		if l.ObservesRedirects() && l.compiled != nil {
			out = append(out, l.Manifest.Name)
		}
	}
	return out
}

// Inline runs every inline add-on against one decided redirect, in load order,
// and reports what they made of it.
//
// Each module sees the destination as the module before it left it, which is the
// only composition that makes two installed add-ons mean what an operator would
// read them to mean: a rewriter that strips tracking parameters and a second one
// that appends a privacy signal compose, rather than one of them silently
// winning. **A veto ends the walk**, because there is no destination left to ask
// anybody else about.
//
// Nil-safe and free when nothing is installed: the guard below is the whole cost
// on an instance with no inline add-on, which is every instance until an operator
// installs one.
func (h *Host) Inline(ctx context.Context, d RedirectDecision) InlineResult {
	out := InlineResult{Destination: d.Destination}
	if !h.HasInline() {
		return out
	}
	for i := range h.inline {
		l := &h.inline[i]
		d.Destination = out.Destination
		answer, ok := h.invokeInline(ctx, l, d)
		if !ok {
			continue
		}
		if answer.Verdict == abi.VerdictVeto {
			out.Vetoed = true
			// The host's own voice, not the module's: this is a refusal somebody
			// will meet, and an operator asked "why did this link stop working"
			// needs the add-on's name. At info rather than debug because a veto is
			// rare by construction — a module that vetoed every redirect would be an
			// add-on nobody would keep installed — and at info rather than warn
			// because it is the add-on doing what it was installed to do.
			h.log.Info("an add-on vetoed a redirect",
				slog.String("addon", l.Manifest.Name),
				slog.String("alias", d.Alias))
			return out
		}
		if !answer.Rewrite {
			continue
		}
		rewritten, err := applyQuery(out.Destination, answer.Query)
		if err != nil {
			// The module asked for something the host will not do to a URL. Its own
			// call was answered with a status it could branch on where the answer was
			// decidable there; what is left here is the URL arithmetic, and the
			// honest response to failing it is to leave the destination alone.
			h.log.Debug("refused an add-on's query rewrite",
				slog.String("addon", l.Manifest.Name),
				slog.Any("error", err))
			continue
		}
		if rewritten != out.Destination {
			out.Destination = rewritten
			out.Rewritten = true
		}
	}
	return out
}

// invokeInline runs one module and returns what it answered, or ok=false when it
// answered nothing — killed, refused a slot, trapped, or simply silent.
//
// A module that answered nothing means *allow, unchanged*, and the four ways of
// answering nothing agreeing is deliberate: a bug in an add-on must not be able
// to refuse somebody's links, so a failure is never a veto.
func (h *Host) invokeInline(ctx context.Context, l *Loaded, d RedirectDecision) (RedirectAnswer, bool) {
	name := l.Manifest.Name
	// Without waiting. The bound exists because each instance holds guest memory,
	// and the redirect path is the one caller that must never queue behind it: a
	// visitor waiting for an add-on's *turn* is a visitor waiting for a resource
	// this product owns, which is the half of the promise the owner's boundary did
	// not give away.
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.metrics.ObserveThrottled("addon_inline")
		return RedirectAnswer{}, false
	}

	start := time.Now()
	// The deadline covers instantiation as well as the call, because package
	// initialization runs during instantiation and a module can hang there — which
	// is not a hypothetical, it is the `spinning` fixture. WithCloseOnContextDone
	// is what makes cancelling this close the guest.
	callCtx, cancel := context.WithTimeout(ctx, h.inlineDeadline)
	defer cancel()

	instance := name + "#" + strconv.FormatUint(h.instances.Add(1), 10)
	st := h.hostState(name)
	if st == nil {
		return RedirectAnswer{}, false
	}
	st = st.forRedirect(&d, nil)
	h.mu.Lock()
	if h.states == nil {
		h.states = make(map[string]*hostState)
	}
	h.states[instance] = st
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.states, instance)
		h.mu.Unlock()
	}()

	mod, err := h.runtime.InstantiateModule(callCtx, l.compiled, guestModuleConfig(instance))
	if err != nil {
		if mod != nil {
			_ = mod.Close(context.WithoutCancel(callCtx))
		}
		h.redirectFailed(name, ClassInline, callCtx, ctx, start, "instantiate", err)
		return RedirectAnswer{}, false
	}
	defer func() { _ = mod.Close(context.WithoutCancel(callCtx)) }()

	fn := mod.ExportedFunction(abi.GuestRedirectInline)
	if fn == nil {
		// Declared the grant, exports nothing. Refused at the invocation rather
		// than at load, exactly as a missing request handler is: the export is a
		// property of the wasm and taking an instance down for it would take the
		// instance down for a redirect nobody asked the add-on about.
		h.log.Error("an add-on declared the redirect-inline permission and exports no "+
			"handler; it is doing nothing on the redirect path until it is rebuilt",
			slog.String("addon", name),
			slog.String("export", abi.GuestRedirectInline))
		return RedirectAnswer{}, false
	}
	if _, err := fn.Call(callCtx); err != nil {
		h.redirectFailed(name, ClassInline, callCtx, ctx, start, "call", err)
		return RedirectAnswer{}, false
	}
	h.metrics.ObserveAddonRedirect(name, ClassInline, time.Since(start))
	if st.answer == nil {
		return RedirectAnswer{}, false
	}
	return *st.answer, true
}

// redirectFailed is the one place a failed redirect invocation is reported, and
// it is where a **kill** is told apart from everything else.
//
// The distinction cannot be read off the error: a module closed underneath a
// running call reports what it was doing rather than why it stopped — "module
// closed with context deadline exceeded" on this wazero and something else on the
// next — which is the same reason [Host.runGuest] reports expiry separately at
// load. So the context is asked, and the parent is asked too: a redirect whose
// own request was cancelled, or an instance shutting down, is not an add-on
// overrunning, and counting it as one would put a number on the Add-on manager
// that blames the wrong party.
func (h *Host) redirectFailed(name, class string, callCtx, parent context.Context,
	start time.Time, step string, err error,
) {
	killed := errors.Is(callCtx.Err(), context.DeadlineExceeded) && parent.Err() == nil
	if killed {
		h.metrics.ObserveAddonRedirectKill(name)
		// Warn, and it is the one line in this file an operator is meant to act
		// on: a module being killed repeatedly is an add-on to go and fix, and it
		// is the fact the owner's boundary rests on being visible.
		h.log.Warn("an add-on overran its deadline on the redirect path and was killed; "+
			"the redirect completed without it",
			slog.String("addon", name),
			slog.String("class", class),
			slog.String("deadline", h.inlineDeadline.String()),
			slog.String("step", step))
		return
	}
	// Not a kill: a trap, a module that will not instantiate, or the visitor
	// leaving. Debug, because this runs per redirect and a module that traps on
	// every one of them would otherwise decide how much an instance logs.
	h.log.Debug("an add-on failed on the redirect path; the redirect proceeded without it",
		slog.String("addon", name),
		slog.String("class", class),
		slog.String("step", step),
		slog.Duration("took", time.Since(start)),
		slog.Any("error", err))
}

// Observe hands one recorded redirect to every observing add-on, out of band.
//
// **It never blocks and it never fails.** The queue is bounded and a full one
// drops, which is the contract analytics.Ingester already keeps with the redirect
// path and is the reason this is safe to call from the pipeline's own goroutine:
// an add-on that is slow must not turn into a click batch that is late.
//
// Nil-safe, and free on an instance where nothing declared the grant — the
// channel is nil then, and a send on a nil channel is not what happens, because
// the guard below returns first.
func (h *Host) Observe(ev RedirectEvent) {
	if h == nil || h.observe == nil {
		return
	}
	select {
	case h.observe <- ev:
	default:
		// Counted where "is anything being throttled" is answered, which is the one
		// place an operator looks. A dropped observation is not an error: the click
		// it describes is in the database either way, and an add-on that cannot keep
		// up with the redirect rate is being told so by this number rather than by
		// its own latency creeping into somebody's page.
		h.metrics.ObserveThrottled("addon_observe")
	}
}

// observeQueue is how many recorded redirects may wait to be shown to an
// observing add-on.
//
// Small on purpose. The queue is not a buffer against a burst, it is the width of
// the window in which a slow add-on is still worth waiting for: an observation
// that is a minute old is of no more use than one that was dropped, and a deep
// queue would hold memory to deliver stale facts. Anything past it is counted and
// discarded, and the counter is what tells an operator the add-on is not keeping
// up.
const observeQueue = 256

// observeWorkers is how many observing invocations run at once.
//
// One. Instantiating a module costs milliseconds and observation has no deadline
// to meet, so parallelism here would buy throughput at the price of guest memory
// held off the same bound the redirect path draws from — and the redirect path is
// the caller that must not be made to wait for a slot. A single worker is also
// what makes the order an add-on sees the order the clicks were recorded in,
// which is the weakest useful promise and the only one worth making.
const observeWorkers = 1

// startObserving launches the out-of-band workers, and does nothing at all when
// no add-on declared the grant.
func (h *Host) startObserving(ctx context.Context) {
	var observers []Loaded
	for _, l := range h.loaded {
		if l.ObservesRedirects() && l.compiled != nil {
			observers = append(observers, l)
		}
	}
	if len(observers) == 0 {
		return
	}
	h.observe = make(chan RedirectEvent, observeQueue)
	// Detached from the context Open was called with, which is a boot context and is
	// done long before the first redirect, and then made cancellable again so that
	// Close can end an invocation in flight. Both halves matter: without the first,
	// observation would stop the moment boot finished; without the second, shutting
	// an instance down would wait out the deadline of whatever module happened to be
	// running, which on a saturated queue is every module in turn.
	worker, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.observeStop = cancel
	for range observeWorkers {
		h.observeWG.Add(1)
		go func() {
			defer h.observeWG.Done()
			for {
				select {
				case ev := <-h.observe:
					for i := range observers {
						h.invokeObserve(worker, &observers[i], ev)
					}
				case <-worker.Done():
					return
				}
			}
		}()
	}
}

// invokeObserve runs one observing module against one recorded redirect.
//
// It takes a slot from the same bound the inline path and the routes path draw
// from, and unlike the inline path it **waits** for one: nothing is holding a
// visitor open here, so queueing is the right answer and dropping would be
// throwing away work for no gain. The deadline is the inline one, for the reason
// there is one knob rather than two — an add-on that hangs is an add-on that
// hangs, and an operator has no more information with which to choose a second
// number than they had for the first.
func (h *Host) invokeObserve(ctx context.Context, l *Loaded, ev RedirectEvent) {
	name := l.Manifest.Name
	callCtx, cancel := context.WithTimeout(ctx, h.inlineDeadline)
	defer cancel()
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-callCtx.Done():
		h.metrics.ObserveThrottled("addon_observe")
		return
	}

	start := time.Now()
	instance := name + "#" + strconv.FormatUint(h.instances.Add(1), 10)
	st := h.hostState(name)
	if st == nil {
		return
	}
	st = st.forRedirect(nil, &ev)
	h.mu.Lock()
	if h.states == nil {
		h.states = make(map[string]*hostState)
	}
	h.states[instance] = st
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.states, instance)
		h.mu.Unlock()
	}()

	mod, err := h.runtime.InstantiateModule(callCtx, l.compiled, guestModuleConfig(instance))
	if err != nil {
		if mod != nil {
			_ = mod.Close(context.WithoutCancel(callCtx))
		}
		h.redirectFailed(name, ClassObserve, callCtx, ctx, start, "instantiate", err)
		return
	}
	defer func() { _ = mod.Close(context.WithoutCancel(callCtx)) }()

	fn := mod.ExportedFunction(abi.GuestRedirectObserve)
	if fn == nil {
		h.log.Error("an add-on declared the redirect-observe permission and exports no "+
			"handler; it is seeing nothing until it is rebuilt",
			slog.String("addon", name),
			slog.String("export", abi.GuestRedirectObserve))
		return
	}
	if _, err := fn.Call(callCtx); err != nil {
		h.redirectFailed(name, ClassObserve, callCtx, ctx, start, "call", err)
		return
	}
	h.metrics.ObserveAddonRedirect(name, ClassObserve, time.Since(start))
}

// stopObserving ends the workers and waits for them.
//
// It cancels rather than signalling, so a module mid-invocation is closed by the
// runtime instead of being waited out: an instance shutting down must not hold on
// for an add-on's deadline, and a saturated queue would make that once per
// observation rather than once. Whatever is still queued is dropped, which is what
// a bounded best-effort queue already does under load and is why nothing here
// drains it — an observation is not data this product owes anybody, unlike the
// click it was derived from, which the ingester flushes.
func (h *Host) stopObserving() {
	if h.observeStop == nil {
		return
	}
	h.observeStop()
	h.observeWG.Wait()
	h.observeStop = nil
}

// --- what a module may answer with ------------------------------------------

// maxRedirectQuery bounds a query string a module writes.
//
// It is the same 2 KiB this product's own alias and destination handling treats
// as a long URL, and it is a bound on what the *host* will build rather than a
// judgement about what a URL may be: the destination the query is substituted
// into is the Location header of a redirect somebody is waiting for, and a module
// that appends a megabyte to it has made the response the slow part.
const maxRedirectQuery = 2048

// decodeRedirectAnswer reads a RedirectAnswer record and checks every bound the
// host enforces.
//
// Strict on unknown fields, for the reason the response decoder is: a record
// carrying a field this host does not know is a module expecting behaviour that
// will not happen, and there is no safe direction to guess in.
func decodeRedirectAnswer(raw []byte) (RedirectAnswer, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var a RedirectAnswer
	if err := dec.Decode(&a); err != nil {
		return RedirectAnswer{}, fmt.Errorf("decoding the answer: %w", err)
	}
	if dec.More() {
		return RedirectAnswer{}, errors.New("the answer carries more than one JSON value")
	}
	if !isVerdict(a.Verdict) {
		return RedirectAnswer{}, fmt.Errorf("verdict %q is not one of %v", a.Verdict, abi.RedirectVerdicts)
	}
	if !a.Rewrite && a.Query != "" {
		// A query with no rewrite asked for is a module that believes it changed
		// something and did not. Refused rather than ignored: the flag exists so
		// that *remove the query* is expressible, and a record where the two
		// disagree has no reading that is obviously right.
		return RedirectAnswer{}, errors.New("a query was written and rewrite is false")
	}
	if len(a.Query) > maxRedirectQuery {
		return RedirectAnswer{}, fmt.Errorf("the query is %d bytes and is bounded at %d",
			len(a.Query), maxRedirectQuery)
	}
	if a.Rewrite && !validQuery(a.Query) {
		return RedirectAnswer{}, errors.New("the query holds a character a query string may not")
	}
	return a, nil
}

func isVerdict(v string) bool {
	for _, ok := range abi.RedirectVerdicts {
		if v == ok {
			return true
		}
	}
	return false
}

// validQuery reports whether every byte is one RFC 3986 allows in a query.
//
// A byte scan rather than url.ParseQuery, and the difference is what is being
// asked. ParseQuery answers *is this a well-formed set of key=value pairs*, which
// is not the host's business — a module may write whatever shape the destination
// expects, and this product's own ForwardQuery already carries queries that are
// not pairs. What the host has to know is narrower: that substituting this string
// into a URL cannot make the URL mean something else. The characters that could
// are `#`, which starts a fragment, and anything outside the query grammar, which
// a browser or an intermediary may re-encode differently from how this host
// wrote it.
func validQuery(q string) bool {
	for i := range len(q) {
		c := q[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("-._~%!$&'()*+,;=:@/?", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// applyQuery substitutes a module's query into a destination the host decided,
// and proves that nothing else about the URL moved.
//
// **Substitution rather than acceptance is the whole of D317's bound.** The
// module never writes a URL: it writes a query, the host parses the destination
// *it* chose, replaces one field and re-serializes. So the scheme, the host, the
// port and the path cannot change, and the destination validator's single call
// site — link.Service.Judge, which every tier above the SSRF refusals reaches by
// host — keeps deciding what it decided. No add-on becomes a fourth way around
// it, and no `destinationSurfaces` row is owed.
//
// The comparison afterwards is not redundant with that argument, and it is the
// half D317 asked for in as many words: *an assertion that the rewritten URL
// differs from the decided one in its query and in nothing else, so the bound is
// enforced by the host rather than trusted to the module*. What it catches is not
// a hostile module — the module has no lever here — but this host, on a future
// url package, deciding that a RawQuery containing something surprising should be
// reflected somewhere else in the URL.
func applyQuery(destination, query string) (string, error) {
	u, err := url.Parse(destination)
	if err != nil {
		return "", fmt.Errorf("the destination is not a URL this host can re-serialize: %w", err)
	}
	before := *u
	u.RawQuery = query
	// Cleared rather than preserved, and it is the one case where "the query" and
	// "the query string" differ: a destination ending in a bare `?` parses with an
	// empty RawQuery and ForceQuery set, and a module asking for the query to be
	// removed means the `?` as well. Left set, an empty rewrite would produce a URL
	// identical to the one handed in, and the module would be told it changed
	// something it did not.
	u.ForceQuery = false
	rewritten := u.String()

	after, err := url.Parse(rewritten)
	if err != nil {
		return "", fmt.Errorf("substituting the query produced a URL that does not parse: %w", err)
	}
	if after.Scheme != before.Scheme || after.Host != before.Host ||
		after.Path != before.Path || after.EscapedPath() != before.EscapedPath() ||
		after.Opaque != before.Opaque || after.Fragment != before.Fragment ||
		after.User.String() != before.User.String() {
		return "", fmt.Errorf("substituting a query changed more than the query: %q became %q",
			destination, rewritten)
	}
	if after.RawQuery != query {
		return "", fmt.Errorf("the query did not survive substitution: %q became %q", query, after.RawQuery)
	}
	return rewritten, nil
}
