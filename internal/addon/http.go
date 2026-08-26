package addon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
)

// This file is the routes limb (M64): what an add-on may be asked, what it may
// answer, and what the host does with the answer.
//
// # An add-on returns data, and the host is the only thing that renders
//
// The load-bearing security claim of the milestone, and it is structural rather
// than careful. A module never produces markup: [Response.Body] is text, the
// [Response.ContentType] vocabulary is closed and **does not contain text/html**,
// and the only path by which a module's bytes reach a browser as HTML is the
// host's own page template, which escapes them as data. So a module answering
// with a script tag renders the characters of a script tag. There is no
// sanitizer to get wrong, because there is no markup path to sanitize — and the
// Content-Security-Policy is untouched, since nothing this file does needs an
// inline anything.
//
// # Prefix, and why it is not a registry
//
// One prefix per add-on, `/addons/<name>/`, derived from the name as the Postgres
// schema (M63) and the cookie namespace (D232) are. Two add-ons cannot contend
// for one prefix, none can be denied its own by whichever loaded first, and there
// is no allocation table to keep. The route lives on the application tree only;
// internal/httpx mounts it and the redirect tree never sees it.
//
// The three derivations are not equally safe, and the difference is what is
// joined onto the name. Here the whole name is one path segment and the router
// matches it exactly, so `/addons/oidc/` and `/addons/oidc_x/` are as distinct as
// the names — the same for the schema, which is one identifier. A cookie prefix
// and a settings variable are the name with *more* joined onto them, and that is
// where `oidc` reached `oidc_x`; nameCollisions (host.go) is the load-time
// refusal that closes it, and nothing in this file needed to change for it.
//
// # One instance per request
//
// D260. A request gets its own instance of the module, instantiated from the
// already-compiled form and closed when the response is written. Compiling per
// request was ruled out by M60's measurement — a few hundred milliseconds
// against a 250 ms page budget — and instantiating was not: about 2 ms.
// TestRoutingCostsAnInstantiation measures the whole round trip rather than
// inheriting that number, and it was 3.96 ms when D260 was written.
//
// What the 2 ms buys is that guest memory does not cross requests. A module
// cannot hold one visitor's state where another visitor's request can read it,
// which for an authentication add-on is the difference between a nonce and a
// vulnerability — and it means an add-on keeping state between two requests of
// one flow has to put it in its own schema, where it survives a restart and is
// visible to every replica.
//
// **That sentence is about this file's requests and no longer about the whole
// host** (M66.5, D335). The redirect path pools its instances, because starting
// one cost a visitor 11.05 ms against a 20 ms target, and it keeps the same
// property by a different means: the host copies the guest's linear memory the
// moment package initialization ends and writes the copy back before the instance
// is handed out again, so what one redirect left is not what the next one reads.
// pool.go is the mechanism and the argument. It is not applied here, and that is
// scope rather than a claim it should not be: a page request has a 250 ms budget
// where a redirect has 20 ms, so this path has never been the one paying.
//
// The cost of an instance is that memory: a fixture holds about 2.4 MB of it at
// load, the host allows one instance [maxGuestMemoryPages], and
// [maxConcurrentRoutes] bounds how many are alive at once *in flight* —
// [DefaultPoolSize] is what may additionally be held at rest since the redirect
// path started keeping them. The three bounds add into the ceiling an operator
// sizes a host by; the 2.4 MB is a measurement of one fixture, which is a
// different kind of sentence and was for a while being read as this one.

// RoutePrefix is the path every add-on's routes live under. One segment, so the
// reserved-word list needs exactly one entry for the whole feature.
const RoutePrefix = "/addons/"

// PathPrefix is the prefix this add-on owns, with both slashes.
func (l Loaded) PathPrefix() string { return RoutePrefix + l.Manifest.Name + "/" }

// ServesRoutes reports whether this add-on holds the grant its prefix costs.
func (l Loaded) ServesRoutes() bool { return l.grants.Has(PermissionRoutes) }

// PermissionRoutes is the grant a route costs. Named here for the reason
// [abi.PermissionStorage] is named in the ABI: this file branches on it, and a
// second spelling of a permission is the drift a closed vocabulary exists to
// stop. A test holds it against the vocabulary.
const PermissionRoutes = "routes.own_prefix"

// PermissionSessionContext is the grant [SessionContext] costs, and it is
// separate from PermissionRoutes on purpose — D258.
const PermissionSessionContext = "session.context"

// maxConcurrentRoutes bounds how many add-on invocations are in flight at once,
// across every add-on and every reason for invoking one.
//
// It exists because add-on routes are reachable without a session (D261) and
// each in-flight instance holds linear memory. Unbounded, a flood of anonymous
// requests to an add-on's prefix is a memory exhaustion with no session to
// rate-limit against; bounded, it is a queue.
//
// **Three callers draw on it and they do not wait alike** — the name predates
// two of them and is kept because renaming a constant is not what makes the
// bound true. Each is documented where an operator meets its symptom:
//
//   - A **page request** waits, on the request's own context, so what bounds the
//     wait is the deadline every application request already carries
//     (LINKCTRL_HTTP_REQUEST_TIMEOUT). It is [ErrBusy] after that rather than a
//     page that arrives too late to be read.
//   - An **inline redirect invocation** (M66) never waits. A visitor is being held
//     open, and queueing for a resource this product owns is the half of the
//     redirect promise the owner's boundary did not give away — so the redirect is
//     served with the add-on skipped, counted on
//     linkctrl_rate_limited_total{limit="addon_inline"}.
//   - An **out-of-band observation** (M66) waits on the add-on deadline rather
//     than on any request, because nothing is waiting for it, and is dropped and
//     counted on {limit="addon_observe"} when that runs out.
//
// So an instance running an inline add-on and serving no add-on pages still
// spends this budget, which is why docs/deployment.md sizes a host by it whatever
// the add-on is for.
const maxConcurrentRoutes = 16

// maxGuestMemoryPages is how much linear memory one instance may hold, in
// WebAssembly pages of 64 KiB — 8 MiB, and it is the other half of the price
// [maxConcurrentRoutes] was chosen to bound.
//
// **This is the bound F289's sibling F290 found missing.** Until it existed the
// runtime carried wazero's default of 65536 pages, so the "about 2.4 MB per
// in-flight request" this file used to state as the cost was a *typical* figure
// and not a bound at all: one request that allocated took the host from 78 MB
// resident to 1604 MB, and sixteen concurrent reached 4332 MB, on a product whose
// documented floor is 1 GB. The typical figure was never wrong — a real fixture
// measures 2.31 MiB of linear memory at load — and a typical figure is not what
// an operator sizing a host needs.
//
// The two numbers multiply, and their product is the sentence docs/SECURITY.md,
// docs/deployment.md, docs/configuration.md and CHANGELOG.md now make: **16 x
// 8 MiB = 128 MiB of guest memory at saturation**, whatever the modules are.
// TestTheGuestMemoryCeilingIsTheOneDocumented holds all three numbers against
// every sentence that states one — **each part where it is stated, not only the
// product**, because 32 instances of 4 MiB is also 128 MiB and those files state
// the concurrency and the per-instance bound in their own words. Which sentences
// those are is not a list anybody maintains by remembering to:
// TestEveryDocumentedNumberIsTied sweeps the documentation for these numbers and
// fails on an occurrence the first test does not account for.
//
// Eight is measured rather than guessed, from both directions. The fixture the
// standard toolchain produces holds 2.4 MB at load, allocates a 4 MiB block on top
// and answers, and traps at 5 MiB — and 4 MiB of working room is well past what a
// handler needs, since the largest request that can cross this boundary is 64 KiB
// and the largest response 256 KiB. From the other direction, eight is what keeps
// the whole ceiling inside the 1 GB floor docs/deployment.md documents. A module
// wanting more is refused where an operator can act on it: a memory section
// declaring a *minimum* over this bound fails at load with the add-on named, and
// a module that grows past it traps mid-request and answers 502. Neither is
// silent, which is the difference between a bound and a cliff.
//
// A declared **maximum** over the bound is the case four documents got wrong, and
// it is neither of those: wazero does not refuse it, it *replaces* it with the
// runtime's limit while decoding (internal/wasm/binary/decoder.go:224), so the
// module loads and its instance is held to 128 pages anyway. Nothing is lost by
// that — a module held to the bound is bounded — but "refused at load" was not
// what happened, and nothing checked. TestWhatAMemorySectionMayDeclare measures
// both limbs now, on wasm written by hand because Go emits no maximum at all,
// which is why no fixture in this repository could ever have shown either.
const maxGuestMemoryPages = 128

// maxResponseBody bounds what one add-on response may carry.
//
// Not the same bound as maxStringIn, which is a liveness bound on one value
// crossing the boundary: this is what a page may be. 256 KiB is longer than any
// server-rendered page this product draws and short enough that a module cannot
// make the host buffer a megabyte per in-flight request. The whole record is
// already bounded by maxStringIn at 64 KiB, so this is the second, looser bound
// and it exists so that raising the first one does not silently raise this.
const maxResponseBody = 256 << 10

// Errors the routing path answers with. Each maps to one HTTP status in
// internal/httpx, and they are distinguishable because the operator's fix
// differs: a name nobody installed is a link somebody typed, a module with no
// handler is a packaging bug, and a busy host is capacity.
var (
	// ErrNoRoute is no such add-on, or one that did not declare
	// routes.own_prefix. The two are deliberately one answer: an add-on that did
	// not ask for a prefix does not have one, and telling a visitor which
	// add-ons are installed is not this surface's job.
	ErrNoRoute = errors.New("no add-on serves this prefix")
	// ErrNoHandler is an add-on that declared the grant and exports no handler.
	ErrNoHandler = errors.New("the add-on exports no request handler")
	// ErrNoResponse is a handler that returned without writing one.
	ErrNoResponse = errors.New("the add-on's handler wrote no response")
	// ErrBusy is maxConcurrentRoutes, reached.
	ErrBusy = errors.New("too many add-on requests are in flight")
	// ErrRequestTooLarge is a request whose record does not fit one value.
	//
	// Separate from the rest because it is the only one of them the *client*
	// caused: a body somebody sent is what made the record too big, so it is a
	// 413 and not a 502, and it is not written to the log at error. Nothing about
	// the add-on is wrong.
	ErrRequestTooLarge = errors.New("the request record is larger than one value may be")
	// ErrGuestFailed is the module trapping, or returning a status.
	ErrGuestFailed = errors.New("the add-on's handler failed")
)

// RequestIn is everything about an HTTP request that *could* cross this
// boundary, handed over by internal/httpx before the host decides what does.
//
// It exists so that "no cookie of the host's reaches an add-on" is enforced in
// one place, by the code that knows the manifest, rather than by every caller
// remembering to filter. internal/httpx hands over the cookies the browser sent
// — including the session cookie, deliberately — and [RequestIn.record] is what
// drops all but the ones an add-on declared a prefix for. A test sends a real
// session cookie and asserts it does not cross.
type RequestIn struct {
	Method string
	// Path is already relative to the add-on's prefix, and must begin with "/".
	Path           string
	Query          string
	ContentType    string
	AcceptLanguage string
	Body           []byte
	Cookies        []*http.Cookie

	// ClientIP and UserAgent are the request's, and they are here for exactly one
	// consumer: a session minted through `session_mint` records where the sign-in
	// came from, in the same columns and by the same reduction the password path
	// uses. **Neither reaches the guest.** RequestIn is the host's input type and
	// [RequestIn.record] is what turns it into the record a module sees; neither
	// field is copied into it, no ABI record has a field for either, and
	// abi.AddressBearing fails the surface test if one ever acquires a name that
	// reads like an address. The address itself becomes a /24 or /48 prefix inside
	// internal/auth before it reaches a column, which is where every other address
	// in this product is reduced.
	ClientIP  netip.Addr
	UserAgent string

	// Identity is whoever the *host* resolved for this request, or nil for
	// nobody. It is the single source of truth about who is signed in, and
	// [RequestIn.session] is what a module is allowed to see of it.
	//
	// **One value rather than two**, which M65 is why. Two host functions have
	// opposite requirements about a session — `identity_link` refuses unless
	// somebody is signed in, `session_mint` refuses unless nobody is — and the
	// record a module sees is blanked for an add-on that did not declare
	// `session.context`. Passing the record as well would therefore have let an
	// add-on's own manifest decide whether the host noticed a session, which is
	// the wrong thing to be able to arrange from inside a manifest.
	Identity *auth.Identity
}

// session is the SessionContext record for this request, and the whole of what a
// module may learn about who is signed in.
//
// Built here rather than by internal/httpx because this package owns the ABI's
// records, and because the property worth asserting — nothing in it is a
// credential (D232) — is a property of this mapping. No cookie, no token and no
// session identifier appears below; abi.CredentialBearing walks the record's field
// names for the same claim from the surface.
func (in RequestIn) session() SessionContext {
	id := in.Identity
	if id == nil {
		// Not an error. Add-on routes are reachable without a session, because a
		// sign-in flow could not otherwise begin, so this is the ordinary state of a
		// module drawing its first page.
		return SessionContext{}
	}
	out := SessionContext{
		SignedIn:    true,
		UserID:      id.UserID.String(),
		Email:       id.Email,
		DisplayName: id.Name,
		Role:        id.Role,
	}
	// The zero UUID crosses as an empty string rather than as a run of zeroes: a
	// module comparing what it was handed against "" is doing the obvious thing,
	// and a module that stored 00000000-0000-0000-0000-000000000000 as a tenant
	// would have stored a tenant that does not exist.
	if id.WorkspaceID != uuid.Nil {
		out.WorkspaceID = id.WorkspaceID.String()
	}
	if id.OrgID != uuid.Nil {
		out.OrganizationID = id.OrgID.String()
	}
	return out
}

// record turns a request into the HTTPRequest a guest sees, keeping only what
// the ABI says crosses.
func (in RequestIn) record(addon string, prefixes []string) Request {
	body, encoded := EncodeRequestBody(in.Body)
	req := Request{
		Method:         in.Method,
		Path:           in.Path,
		Query:          in.Query,
		Cookies:        map[string]string{},
		ContentType:    in.ContentType,
		AcceptLanguage: in.AcceptLanguage,
		Body:           body,
		BodyBase64:     encoded,
	}
	for _, c := range in.Cookies {
		if ownedName(c.Name, prefixes) {
			req.Cookies[c.Name] = c.Value
		}
	}
	// And the jar, which is where an add-on's cookies actually live since M64 was
	// reopened. It is read after the loop above rather than instead of it: the
	// loop is what keeps "no cookie of the host's crosses" true of the raw
	// header, and a value the host wrote for this add-on outranks a same-named
	// one the browser holds from anywhere else.
	session, kept := jarsFrom(addon, in.Cookies, prefixes, time.Now())
	for _, e := range append(session, kept...) {
		req.Cookies[e.Name] = e.Value
	}
	return req
}

// Request is the HTTPRequest record, host-side.
//
// Every field is one the ABI declares, and the absences are the point: no
// header map, no client address in any spelling, and no Cookie header — only
// the cookies whose names begin with a prefix this add-on's manifest declared
// (D232). internal/httpx builds it; nothing here reads an *http.Request, so
// what crosses is decided in one place and is the same for every add-on.
type Request struct {
	Method string `json:"method"`
	// Path is the path *within* the add-on's prefix, always beginning with "/".
	// An add-on therefore cannot tell which prefix it was mounted under from the
	// request, which is deliberate: the prefix is its name and it knows its name.
	Path           string            `json:"path"`
	Query          string            `json:"query"`
	Cookies        map[string]string `json:"cookies"`
	ContentType    string            `json:"content_type"`
	AcceptLanguage string            `json:"accept_language"`
	Body           string            `json:"body"`
	// BodyBase64 says how to read Body, and it is the field that makes the
	// record's own sentence — "the body, base64 when it is not UTF-8" —
	// decidable. Without it a guest could not tell a base64 body from a body
	// that happens to look like base64, which is D262.
	BodyBase64 bool `json:"body_base64"`
}

// SessionContext is the SessionContext record, host-side: who is signed in on
// the request an add-on is answering.
//
// Nothing here is a credential. No cookie, no token, no session identifier —
// D232's rule applied to the read half, and the reason the ABI's credential
// blocklist walks this record's field names.
type SessionContext struct {
	SignedIn       bool   `json:"signed_in"`
	UserID         string `json:"user_id"`
	Email          string `json:"email"`
	DisplayName    string `json:"display_name"`
	WorkspaceID    string `json:"workspace_id"`
	OrganizationID string `json:"organization_id"`
	Role           string `json:"role"`
}

// Cookie is one entry of a response's set_cookie array.
//
// Name, value and a lifetime, and nothing else. Path, Secure, HttpOnly and
// SameSite are the host's — see [Response] — because a cookie an add-on could
// scope for itself is a cookie it could scope to the whole origin.
type Cookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// MaxAge is seconds. Zero is a session cookie; negative deletes.
	MaxAge int `json:"max_age"`
}

// Response is the HTTPResponse record, host-side, after the host has checked it.
//
// The bounds are enforced at the moment the guest writes it — see
// http_response_write in hostabi.go — so a module learns its answer was refused
// by getting StatusInvalid from the call it made, rather than by a page that
// silently differs from what it asked for.
type Response struct {
	Status      int      `json:"status"`
	ContentType string   `json:"content_type"`
	Location    string   `json:"location"`
	SetCookie   []Cookie `json:"set_cookie"`
	Body        string   `json:"body"`
	// Jar is what the host writes to the browser for the cookies above, and it
	// is not part of the record: a module neither sends it nor sees it.
	//
	// [Host.Route] fills it and **empties SetCookie doing so**, which is the
	// structural half of F289's fix. The list a module wrote is gone by the time
	// a response leaves this package, so a writer — the one in internal/httpx, or
	// one a later milestone adds without having read this comment — has nothing
	// to loop over but the jar, and the jar is at most two cookies however many a
	// module named.
	Jar []JarCookie `json:"-"`
	// Minted is the session the host minted while this module was answering, or
	// nil. Also not part of the record: a module neither sends it nor sees it, and
	// what it holds — the session token — is the one value M65 exists to keep on
	// this side of the sandbox.
	//
	// It is on the response rather than a second return value because the two
	// travel together to exactly one place: internal/httpx writes a cookie and then
	// writes the module's answer, in that order, and a caller that ignored this
	// field would produce a page for somebody the host had already signed in.
	//
	// A module that mints and then **fails** — traps, writes no response, answers a
	// refusal — loses this, because Route returns an error and there is no response
	// to carry it on. The session row still exists and expires on its own; the
	// visitor gets a 502 and is not signed in. That is the safe direction of the two
	// and it is stated rather than fixed: writing a session cookie alongside a
	// failure page would sign somebody in on the strength of a module that crashed.
	Minted *Minted `json:"-"`
}

// ContentTypeWrapped is the empty content type: the host wraps the body in the
// dashboard's own page template, escaped. It is the default, and it is the only
// way an add-on's output reaches a browser as part of an HTML document.
const ContentTypeWrapped = ""

// ResponseMediaTypes is the closed vocabulary of media types an add-on may name
// for itself.
//
// **text/html is not in it and will not be**: the host owns the HTML, which is
// what makes "an add-on cannot inject a script tag" a property of the shape
// rather than of a filter. text/plain and application/json are here because
// neither is a document a browser executes and both are things an add-on's own
// endpoint legitimately answers — a webhook receiver, a JSON fragment its page
// fetches. Both are served with X-Content-Type-Options: nosniff by the
// application tree's own middleware, so neither can be sniffed into markup.
var ResponseMediaTypes = []string{"text/plain", "application/json"}

// Route hands one request to the add-on that owns the prefix it arrived under,
// and returns what the module answered.
//
// Nil-safe: an instance with no add-ons directory answers ErrNoRoute, which is
// the 404 a visitor typing the path gets.
//
// **Every error out of here is neutralized, at the exit and not at the site that
// built it** (D286). What internal/httpx does with one is log it, and a failure on
// this path carries the module's own text more often than not: a wasm trap names
// the guest's symbols out of a name section nothing constrains, and so does the
// instantiation failure a module can arrange for the per-request instance alone —
// hostState is registered before InstantiateModule and carries the request, so a
// guest can read that it is answering one and trap only then, loading clean and
// failing per visit. Neutralizing at each site was the shape that missed that one.
// Unwrap survives, so errors.Is on ErrNoRoute, ErrBusy and the rest still decides
// what it decided.
func (h *Host) Route(ctx context.Context, name string, in RequestIn) (Response, error) {
	// The route deadline (M68.5), and this is the whole of where it is applied: one
	// timeout around the invocation, so instantiating the module, running it and
	// every host function it calls — including the one that reaches the network —
	// share one budget. The runtime is built WithCloseOnContextDone, so a module
	// still running when it elapses is closed underneath rather than waited for.
	//
	// Nested inside whatever the caller brought, which on the application path is
	// LINKCTRL_HTTP_REQUEST_TIMEOUT's own context deadline: WithTimeout takes the
	// earlier of the two, so this fires first only because internal/config refuses
	// a route deadline that is not shorter. That is the point of it — see
	// [DefaultRouteDeadline] for what the host does with the margin.
	//
	// Outside the slot wait deliberately: a request that spends the deadline
	// queueing for one of [maxConcurrentRoutes] and is then refused ErrBusy is the
	// right answer, and starting the clock inside would make a saturated host
	// answer more slowly rather than sooner.
	ctx, cancel := context.WithTimeout(ctx, h.deadlineForRoute())
	defer cancel()
	resp, err := h.route(ctx, name, in)
	return resp, neutralize(err)
}

// deadlineForRoute is [Host.routeDeadline] with the zero value defaulted, because
// a Host a test built as a literal has never been through Open.
//
// Nil-safe for the reason [Host.routed] is: an instance with no add-ons directory
// has no host at all, and Route is called on it.
func (h *Host) deadlineForRoute() time.Duration {
	if h == nil {
		return DefaultRouteDeadline
	}
	return routeDeadlineFrom(h.routeDeadline)
}

func (h *Host) route(ctx context.Context, name string, in RequestIn) (Response, error) {
	target := h.routed(name)
	if target == nil {
		return Response{}, ErrNoRoute
	}
	// Announced before the slot, so that a removal in flight stops admitting
	// requests at the same moment it stops resolving them (M67). A request that
	// resolved the add-on a microsecond before the set was swapped is refused here
	// rather than allowed to hold an instance of a module about to be closed, and
	// what it gets is the 404 an add-on that is not installed gets — which by the
	// time the answer is written is true.
	if !target.live.enter() {
		return Response{}, ErrNoRoute
	}
	defer target.live.leave()

	// The slot, before anything is instantiated: the point of the bound is the
	// memory an instance holds, so waiting has to happen before the allocation
	// rather than beside it.
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-ctx.Done():
		return Response{}, ErrBusy
	}

	base := h.hostState(name)
	if base == nil {
		// Registered at load and removed only when the add-on is gone, so this is
		// the same "no state" case dispatch refuses, reached from the other side.
		return Response{}, fmt.Errorf("%w: the host has no state for %q", ErrGuestFailed, name)
	}
	sess := in.session()
	if !target.grants.Has(PermissionSessionContext) {
		// The grant is what the session read costs, and the cheapest place to
		// enforce it is to have nothing to read: an add-on that did not declare it
		// gets dispatch's StatusDenied, and this makes the value absent as well, so
		// no future edit to that check can turn into a disclosure.
		//
		// It blanks the *record* and nothing else. in.Identity is untouched, which is
		// what keeps the two session-shaped host functions answering about the
		// request rather than about the manifest.
		sess = SessionContext{}
	}
	req := in.record(name, target.Manifest.CookiePrefixes)
	st := base.forRequest(&req, sess, in)

	// The record has to fit one value, and this is where that is decided.
	//
	// Nothing else decides it. http_request_read writes the record into a buffer
	// the guest sized from the host's own answer, so the host→guest direction is
	// bounded by guest memory rather than by maxStringIn — a 393 KB record was
	// measured crossing intact. What that leaves is a request a module can be
	// handed and cannot answer about: [maxResponseBody] is 256 KiB but a response
	// *record* crosses the other way, through readString, where maxStringIn does
	// bind — so a module that reflects what it was sent is refused its own answer
	// and the visitor gets a 502 for a body they chose the size of.
	//
	// Refused here instead, before anything is instantiated, as the 413 it is. The
	// bound is maxStringIn because that is the ABI's single-value bound and a record
	// over it is a record the boundary would not carry in the other direction
	// either.
	//
	// **What it does not close, measured**: a module reflecting its whole input
	// *plus* anything of its own is over the response bound while the request was
	// under it, and the fixture shows the band — a plain body of 65,350 answers 200
	// and one of 65,380 does not, roughly fifty bytes wide. No request bound can
	// close that, because the decoration is the module's and this host does not know
	// it. It is also not the host mislabelling anything: the module is told
	// StatusInvalid by http_response_write and can answer something smaller. The
	// fixture returns -1 instead, which is what a 502 is for.
	encoded, err := st.encodedRequest()
	if err != nil {
		// Unreachable from a Request — every field is a string, a bool or a map of
		// strings — and answered rather than ignored because the guest would
		// otherwise be instantiated to read a record that does not exist.
		return Response{}, fmt.Errorf("%w: encoding the request record: %w", ErrGuestFailed, err)
	}
	if len(encoded) > maxStringIn {
		return Response{}, fmt.Errorf("%w: the record is %d bytes and one value is bounded at %d",
			ErrRequestTooLarge, len(encoded), maxStringIn)
	}

	// A name no manifest could carry, so a per-request instance can never shadow
	// an add-on: nameRe admits neither `#` nor a digit in first position.
	instance := name + "#" + strconv.FormatUint(h.instances.Add(1), 10)
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

	// Instantiated with the *request's* context, which is what makes a module
	// that will not return a bounded failure rather than a stuck goroutine: the
	// runtime is built WithCloseOnContextDone, so the deadline every application
	// request carries closes the instance underneath a spinning guest.
	mod, err := h.runtime.InstantiateModule(ctx, target.compiled, guestModuleConfig(instance))
	if err != nil {
		if mod != nil {
			_ = mod.Close(ctx)
		}
		return Response{}, fmt.Errorf("%w: instantiate: %w", ErrGuestFailed, err)
	}
	defer func() { _ = mod.Close(ctx) }()

	fn := mod.ExportedFunction(abi.GuestHTTPHandler)
	if fn == nil {
		h.log.Error("an add-on declared the routes permission and exports no handler; "+
			"its pages answer 502 until it is rebuilt",
			slog.String("addon", name),
			slog.String("export", abi.GuestHTTPHandler))
		return Response{}, ErrNoHandler
	}
	out, err := fn.Call(ctx)
	if err != nil {
		// A trap, or the context closing the instance. The detail goes to the log
		// and not to the visitor: a wasm trace names the guest's own symbols, and
		// an error page is not where an add-on's stack belongs. Neutralized by the
		// handler on the way to the log, and by Route on the way out.
		h.log.Error("an add-on's request handler failed",
			slog.String("addon", name),
			slog.Any("error", err))
		return Response{}, fmt.Errorf("%w: %w", ErrGuestFailed, err)
	}
	// A handler may refuse in the ABI's own vocabulary rather than by trapping,
	// and a negative answer is that refusal. It is logged and answered 502,
	// because a module that declines to handle its own route has nothing else to
	// show the person waiting.
	if len(out) > 0 {
		if status := int32(out[0]); status < 0 { //nolint:gosec // G115: a wasm i32 result, read back as one
			h.log.Error("an add-on's request handler returned a refusal",
				slog.String("addon", name),
				slog.Int("status", int(status)))
			return Response{}, fmt.Errorf("%w: it answered status %d", ErrGuestFailed, status)
		}
	}
	if st.response == nil {
		h.log.Error("an add-on's request handler wrote no response",
			slog.String("addon", name))
		return Response{}, ErrNoResponse
	}
	// The jar, and the emptying that makes it the only thing there is to write.
	// Here rather than in internal/httpx because this is the code that knows the
	// manifest, and because a response leaving this package still carrying a
	// module's own cookie list is a response some other caller could write
	// verbatim — which is exactly what F289 was.
	resp := *st.response
	jar, dropped := jarCookies(name, in.Cookies, target.Manifest.CookiePrefixes,
		resp.SetCookie, time.Now())
	if dropped > 0 {
		h.log.Warn("an add-on's cookie jar is full and its oldest values were dropped; "+
			"an add-on keeps at most a few kilobytes in a browser, and anything larger "+
			"belongs in its own schema",
			slog.String("addon", name),
			slog.Int("dropped", dropped))
	}
	resp.SetCookie = nil
	resp.Jar = jar
	// Read off the per-request state rather than out of anything the module wrote,
	// which is the whole of why a module cannot fabricate one: the only writer of
	// this field is the session_mint host function, after internal/auth agreed.
	resp.Minted = st.minted
	return resp, nil
}

// routed selects the loaded add-on that would be handed a request under this
// name, or nil.
//
// One function, because two callers ask the same question for different reasons
// and an answer that differed between them would be a defect neither could see:
// [Host.Route] asks in order to dispatch, and [Host.ServesRoutes] asks so that
// internal/httpx can answer 404 and decline to charge without instantiating
// anything. Nil-safe on the host for the reason Route is — an instance with no
// add-ons directory has no host at all.
func (h *Host) routed(name string) *Loaded {
	if h == nil {
		return nil
	}
	loaded := h.current().loaded
	for i := range loaded {
		if loaded[i].Manifest.Name == name && loaded[i].ServesRoutes() &&
			loaded[i].compiled != nil {
			return &loaded[i]
		}
	}
	return nil
}

// ServesRoutes reports whether a request naming this add-on would reach a module.
//
// It is the shape test internal/httpx makes before the login limiter charges
// (D309): a path under `/addons/` naming nothing this instance serves is a 404
// that costs its caller nothing, and only a request that would actually reach a
// module is charged against the budget somebody needs to sign in. Reading it off
// [Host.routed] is what keeps that from drifting away from what Route does.
//
// No lock, no instance, no allocation: it runs on every request under the prefix
// including the flood it exists to make cheap.
func (h *Host) ServesRoutes(name string) bool { return h.routed(name) != nil }

// RoutedAddons is every loaded add-on holding the routes grant, in discovery
// order. It is what an operator's boot log and M68's manager read; the routing
// path itself does not consult it.
func (h *Host) RoutedAddons() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, l := range h.current().loaded {
		if l.ServesRoutes() {
			out = append(out, l.Manifest.Name)
		}
	}
	return out
}

// --- what a guest may answer ------------------------------------------------

// decodeResponse reads an HTTPResponse record and checks every bound the host
// enforces, returning the reason as an error so the caller can log it.
//
// Strict on unknown fields, for the reason the manifest parser is: a record
// carrying a field this host does not know is a module expecting behaviour that
// will not happen, and there is no safe direction to guess in.
func decodeResponse(raw []byte, prefixes []string) (Response, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var r Response
	if err := dec.Decode(&r); err != nil {
		return Response{}, fmt.Errorf("the response is not an HTTPResponse record: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return Response{}, errors.New("trailing content after the response object")
	}

	if len(r.Body) > maxResponseBody {
		return Response{}, fmt.Errorf("the body is %d bytes and the bound is %d",
			len(r.Body), maxResponseBody)
	}
	if !utf8.ValidString(r.Body) {
		// Reachable: JSON escapes can spell a lone surrogate, which Go's decoder
		// turns into replacement bytes rather than refusing.
		return Response{}, errors.New("the body is not valid UTF-8")
	}

	if r.Location != "" {
		if err := checkLocation(r.Location); err != nil {
			return Response{}, err
		}
		switch r.Status {
		case 0, http.StatusFound:
			r.Status = http.StatusFound
		case http.StatusMovedPermanently, http.StatusPermanentRedirect:
			// Refused rather than downgraded. The inherited rule is that this
			// product never answers a permanent redirect, and silently turning a
			// module's 301 into a 302 would leave its author believing the
			// opposite of what happened.
			return Response{}, fmt.Errorf("status %d is a permanent redirect, and this "+
				"product never answers one; a redirect here is 302", r.Status)
		default:
			return Response{}, fmt.Errorf("status %d with a location: a redirect from an "+
				"add-on is 302, which the host writes; leave the status unset", r.Status)
		}
		if r.Body != "" || r.ContentType != ContentTypeWrapped {
			return Response{}, errors.New("a redirect carries no body and no content type")
		}
	} else {
		if r.Status == 0 {
			r.Status = http.StatusOK
		}
		if r.Status < 200 || r.Status > 599 || (r.Status >= 300 && r.Status < 400) {
			return Response{}, fmt.Errorf("status %d: an add-on answers 2xx, 4xx or 5xx, "+
				"and reaches 3xx by setting a location", r.Status)
		}
		if err := checkContentType(&r); err != nil {
			return Response{}, err
		}
	}

	for i, c := range r.SetCookie {
		if err := checkCookie(c, prefixes); err != nil {
			return Response{}, fmt.Errorf("set_cookie[%d]: %w", i, err)
		}
	}
	// What a module set, packed as though the browser held nothing, so that a
	// response nobody could store is refused at the call the module made rather
	// than at the moment the host writes the answer. This is where the flood
	// stops being possible to *ask* for: 1200 cookies are far past the jar's
	// bound, and the module is told so and can answer something smaller.
	//
	// It is not the only check — [jarCookies] packs again with what the browser
	// already held, where an add-on filling its jar over many responses is
	// bounded by eviction rather than by refusal, because by then the response is
	// written and the module has nothing left to decide.
	if err := checkJarFits(r.SetCookie); err != nil {
		return Response{}, err
	}
	return r, nil
}

// checkJarFits refuses a set of cookies that would not pack into a jar even on
// an empty browser.
func checkJarFits(set []Cookie) error {
	if len(set) == 0 {
		return nil
	}
	now := time.Now()
	var session, kept []jarEntry
	for _, c := range set {
		session, kept = applyToJar(session, kept, c, now)
	}
	for _, entries := range [][]jarEntry{session, kept} {
		if _, err := packJar(entries); err != nil {
			return fmt.Errorf("set_cookie: %w; an add-on's cookies are carried in one "+
				"cookie of the host's, so what bounds them is what a browser will store "+
				"for one", err)
		}
	}
	return nil
}

// checkContentType holds the response to [ResponseMediaTypes], and normalizes
// what it accepts so that a module writing "text/plain" and one writing
// "text/plain; charset=utf-8" get the same answer.
func checkContentType(r *Response) error {
	if r.ContentType == ContentTypeWrapped {
		return nil
	}
	media, params, err := mime.ParseMediaType(r.ContentType)
	if err != nil {
		return fmt.Errorf("content_type %q is not a media type: %w", r.ContentType, err)
	}
	if charset, ok := params["charset"]; ok && strings.ToLower(charset) != "utf-8" {
		return fmt.Errorf("content_type %q: this boundary carries UTF-8 text and nothing else",
			r.ContentType)
	}
	switch media {
	case "text/plain":
		r.ContentType = "text/plain; charset=utf-8"
	case "application/json":
		r.ContentType = "application/json"
	default:
		return fmt.Errorf("content_type %q is not one of %v, and text/html is deliberately "+
			"not among them: the host wraps your body in its own page, escaped, which is "+
			"what stops an add-on putting markup in somebody's dashboard",
			r.ContentType, ResponseMediaTypes)
	}
	return nil
}

// checkLocation refuses what a Location header must never carry.
//
// A scheme-relative "//host" is refused because it is the form that reads as a
// path and behaves as another origin. An absolute http or https URL is allowed:
// an authentication add-on's whole job is to send somebody to an identity
// provider, so the routes grant includes sending a visitor off this origin, and
// docs/SECURITY.md says so where an operator will read it.
func checkLocation(loc string) error {
	if strings.ContainsAny(loc, "\r\n\x00") {
		return errors.New("location carries a control character")
	}
	if strings.HasPrefix(loc, "//") {
		return errors.New("location is scheme-relative, which reads as a path and behaves " +
			"as another origin; write the scheme, or a path")
	}
	if strings.HasPrefix(loc, "/") {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil {
		return fmt.Errorf("location %q does not parse: %w", loc, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("location %q: a redirect is a path or an http(s) URL", loc)
	}
	return nil
}

// checkCookie holds a response's cookies to the namespace the manifest declared,
// which is the outbound half of D232: a namespace an add-on owns is one it owns
// in both directions, or it could overwrite a cookie it is not allowed to read.
//
// It bounds the lifetime as well, and [maxCookieAge] says why that one is
// arithmetic rather than policy.
func checkCookie(c Cookie, prefixes []string) error {
	if c.Name == "" {
		return errors.New("a cookie needs a name")
	}
	if strings.ContainsAny(c.Name, "\r\n\x00 ;=,\t") {
		return fmt.Errorf("cookie name %q carries a character a cookie name may not", c.Name)
	}
	if strings.ContainsAny(c.Value, "\r\n\x00 ;,\"\\") {
		return fmt.Errorf("cookie %q: the value carries a character a cookie value may not", c.Name)
	}
	if c.MaxAge > maxCookieAge {
		return fmt.Errorf("cookie %q: max_age %d is longer than the %d seconds (%d days) a "+
			"cookie may live here; a browser would not keep it that long either, and the "+
			"host refuses rather than quietly writing something else",
			c.Name, c.MaxAge, maxCookieAge, maxCookieAge/86400)
	}
	if !ownedName(c.Name, prefixes) {
		return fmt.Errorf("cookie %q is outside the prefixes this add-on declared (%v); "+
			"a namespace is owned in both directions", c.Name, prefixes)
	}
	return nil
}

// --- the jar: how many cookies an add-on occupies ---------------------------

// A browser's cookie store is a shared, small, evictable resource, and until
// M64 was reopened an add-on could fill it. `set_cookie` is an array, the host
// wrote every entry of it, and nothing bounded the array or the number of
// requests an add-on could answer — so 180 cookies inside the add-on's **own**
// declared namespace overflowed Chromium's per-domain cap and the eviction took
// `linkctrl_session`, signing the visitor out of LinkCtrl on every visit to the
// add-on's page. Measured in real Chromium against a real signed-in account:
// n=179 stays on /dashboard, n=180 lands on /login (F289).
//
// # Why the fix is not a number
//
// The obvious answer is a bound on the array, and it is the wrong shape. A
// browser cookie is *persistent* — the add-on's occupancy is the sum over every
// response it has ever given, not the size of one — so a cap of eight per
// response is a cap of eight per response times however many times somebody
// visits the page, which the add-on also controls, because it can answer with a
// redirect back to itself. A total-bytes bound has the same hole for the same
// reason. Any threshold on one response is a threshold an add-on sits just under
// and repeats.
//
// So the host stopped writing an add-on's cookies at all. What it writes is a
// **jar**: one cookie of the host's own, named for the add-on and outside every
// namespace an add-on may declare, carrying the add-on's cookies inside its
// value. A module still names its cookies, still reads them back by name, and
// still gets the lifetimes it asked for; what it no longer has is a say in how
// many *slots* of the browser's store it occupies. That number is now a property
// of the code — at most one jar per lifetime class, so at most two — and it does
// not move when a module answers a thousand cookies or is visited a thousand
// times.
//
// The count an operator's browser can be made to hold is therefore
// installed-add-ons times two, and installing an add-on is the operator's act.
// An add-on cannot raise its own occupancy at all, which is the property F289
// needed and the reason this is not a threshold.
//
// # Two jars, because a lifetime is not decoration
//
// [Cookie.MaxAge] is zero for a session cookie and positive for a persistent
// one, and one jar cannot be both: a session entry packed beside a year-long one
// would outlive the browser being closed, which is the opposite of what the
// module asked for. So entries are partitioned by lifetime class and each class
// gets its own jar — the session jar written with no Max-Age, the kept jar
// written with the longest lifetime any entry in it still has. Inside the kept
// jar every entry carries its own absolute expiry, so a ten-minute value in a
// jar held open by a year-long one is still gone in ten minutes: the host drops
// it on the way in.
//
// # What an add-on can still do to itself
//
// Fill its own jar. maxCookieJar bounds the packed value, because a cookie a
// browser will not store is not storage; over it, the oldest entries are dropped
// and the operator's log says which add-on did it. That is a threshold, and it is
// named as one here — but it bounds only what the add-on can keep for itself, and
// no value of it changes how many slots the store gives up.

// cookieJarPrefix is the host's name for an add-on's jar. Inside this product's
// own `linkctrl_` namespace, which no manifest may declare a cookie prefix
// inside (reachesHostCookie, manifest.go), so a module can neither read its own
// jar nor forge another add-on's.
const cookieJarPrefix = "linkctrl_addon_"

// keptJarSuffix distinguishes the persistent jar from the session one. Two
// add-ons cannot collide here for the reason they cannot collide over a cookie
// namespace: `a` and `a_kept` would produce the same name, and two names standing
// in a `name + "_"` prefix relation are both refused at load (nameCollisions,
// host.go — D267).
const keptJarSuffix = "_kept"

// maxCookieJar bounds one packed jar value.
//
// 3 KiB, under the 4096 bytes browsers give a cookie's name and value together —
// measured at M64.9's reproduction, where Chromium kept a 4000-byte cookie and
// dropped a 4090-byte one — with the jar's own name and the encoding's overhead
// inside the difference. A jar a browser silently refuses to store is worse than
// a small one, because a module would read back nothing and be told nothing.
const maxCookieJar = 3072

// maxCookieAge bounds [Cookie.MaxAge], and it is here because the arithmetic
// underneath that field was not total.
//
// applyToJar turns a lifetime into an absolute expiry with
// `now.Add(time.Duration(c.MaxAge) * time.Second)`. A time.Duration is int64
// nanoseconds, so above about 9.2e9 seconds that multiplication wraps: measured,
// `max_age=10000000000` produced an expiry 8446744074 seconds *before* now and
// `max_age=1<<62` produced one exactly equal to it. Both entries were then
// dropped by keepLive on the very next read, and the module was told 200 — a
// cookie it was informed it had set, which silently did not exist. It was also a
// regression, because before the jar the same value went to http.SetCookie and
// the browser clamped it, so the cookie worked.
//
// The published contract decides the shape of the fix rather than leaving it to
// taste: http_response_write says every bound on this record is *ErrInvalid
// rather than a silently corrected response*, so an over-long lifetime is
// refused at the call the module made. A clamp is the one answer that sentence
// forbids.
//
// 400 days, 34560000 seconds. Not a safe-looking round number: it is the limit
// draft-ietf-httpbis-rfc6265bis puts on a cookie's age, which a user agent MUST
// reduce a longer lifetime to, and which Chromium has applied since 2022. So a
// module inside the bound loses nothing it used to have, and one outside it was
// never going to get what it asked for from any browser. It clears the overflow
// point by a factor of 266 — 3.456e7 seconds against the 9.223e9 where int64
// nanoseconds wrap — which is what makes the arithmetic total; being the number
// browsers already enforce is why it costs a publisher nothing.
//
// That margin is the whole of the safety here, so it is stated as the number it
// is. This comment said "four orders of magnitude" and the margin is nearer two
// and a half: still total, and still a sentence claiming more than it had, which
// in this milestone is not a small kind of wrong.
//
// Negative is untouched, and deliberately: a negative max_age is a deletion, and
// applyToJar removes the entry without doing any arithmetic on the number, so
// its magnitude reaches nothing.
const maxCookieAge = 400 * 24 * 60 * 60

// JarCookie is one cookie the **host** writes for an add-on: the jar, not
// anything a module named.
//
// internal/httpx writes these and nothing else — [Response.SetCookie] is emptied
// by the time a response leaves [Host.Route], so a caller that reaches for the
// module's own list finds it gone rather than finding it writable. The path and
// every attribute are the writer's, as they always were.
type JarCookie struct {
	Name  string
	Value string
	// MaxAge is seconds, and follows the same convention a [Cookie] does: zero is
	// a session cookie, negative deletes. Negative is how an emptied jar is
	// cleared from a browser that still holds one.
	MaxAge int
}

// jarEntry is one add-on cookie inside a jar. Short field names because they are
// paid for in every request that carries the jar.
type jarEntry struct {
	Name  string `json:"n"`
	Value string `json:"v"`
	// Exp is the absolute unix second this entry dies, and is zero in the session
	// jar, where the browser's own session is the lifetime.
	Exp int64 `json:"e,omitempty"`
}

// jarName is the cookie name for one add-on's jar of one lifetime class.
func jarName(addon string, kept bool) string {
	if kept {
		return cookieJarPrefix + addon + keptJarSuffix
	}
	return cookieJarPrefix + addon
}

// packJar encodes a jar, and refuses one no browser would keep.
//
// base64 because a cookie value may not carry a comma, a semicolon, a space or a
// quote, and JSON carries all four; RawURLEncoding because padding is one of the
// characters at issue.
func packJar(entries []jarEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	if len(value) > maxCookieJar {
		return "", fmt.Errorf("the cookie jar is %d bytes packed and the bound is %d",
			len(value), maxCookieJar)
	}
	return value, nil
}

// unpackJar decodes what a browser sent back, and answers nothing for anything
// it does not understand.
//
// Silent, deliberately. The value is under the visitor's hand — an old jar from
// before a format change, a truncated one, one somebody edited — and none of
// those is the add-on's fault or the operator's problem. A module reads its
// cookies back as absent, which is the state it has to handle anyway, because a
// browser that never stored them looks the same.
func unpackJar(value string) []jarEntry {
	if value == "" || len(value) > maxCookieJar {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	var entries []jarEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	return entries
}

// jarsFrom reads both of an add-on's jars off a request, dropping what has
// expired and what the manifest no longer declares.
//
// The prefix filter is the same one the request record applies, and it is here
// as well because a manifest can lose a prefix between two visits: a value
// written when the add-on declared `pages_` is not one it may read after it
// stopped, and the browser is under no obligation to have forgotten it.
//
// **One name can arrive twice, and the host does not get to assume otherwise.**
// The jar is written at the add-on's own path, and a visitor can plant a cookie
// of the same name at `/` — which the writer, scoped as it is, then has no way
// to delete. So the jars are merged rather than assigned.
func jarsFrom(addon string, sent []*http.Cookie, prefixes []string, now time.Time) (session, kept []jarEntry) {
	for _, c := range sent {
		switch c.Name {
		case jarName(addon, false):
			session = mergeJar(session, unpackJar(c.Value))
		case jarName(addon, true):
			kept = mergeJar(kept, unpackJar(c.Value))
		}
	}
	return keepLive(session, prefixes, now), keepLive(kept, prefixes, now)
}

// mergeJar folds a second jar of the same name into the first, and the first
// wins every name the two share.
//
// Assigning per match instead — which is what this did until the finding — made
// the *last* cookie of a name win, so a jar a visitor planted at `/` shadowed
// the host's own on every later visit and the add-on's state was permanently
// void, unreadable and unclearable. First-wins is RFC 6265 §5.4's order, in
// which a user agent sends the more specifically scoped cookie first, so the one
// the host wrote at `/addons/<name>/` is the one that survives.
//
// What a planted jar can still do is carry names the real jar does not hold. That
// is not a hole this closes and is not one it opens: a value under a declared
// prefix already reaches the module from the plain cookie header, by design, and
// a visitor's own browser is the one place their own add-on state was always
// theirs to write. Nothing here crosses to another add-on or to another visitor.
func mergeJar(first, second []jarEntry) []jarEntry {
	if len(second) == 0 {
		return first
	}
	held := make(map[string]bool, len(first))
	for _, e := range first {
		held[e.Name] = true
	}
	for _, e := range second {
		if !held[e.Name] {
			held[e.Name] = true
			first = append(first, e)
		}
	}
	return first
}

// keepLive drops entries that have expired or left the add-on's namespace.
func keepLive(entries []jarEntry, prefixes []string, now time.Time) []jarEntry {
	live := entries[:0]
	for _, e := range entries {
		if e.Exp != 0 && e.Exp <= now.Unix() {
			continue
		}
		if !ownedName(e.Name, prefixes) {
			continue
		}
		live = append(live, e)
	}
	return live
}

// ownedName reports whether a cookie name is inside one of the declared
// prefixes. The one place that question is answered — [RequestIn.record] and
// [keepLive] on the way in, [checkCookie] on the way out — so the read half and
// the write half cannot drift apart. checkCookie kept its own copy of this loop
// until the reopening, which is how a comment came to claim a uniqueness the
// package did not have.
func ownedName(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// applyToJar folds one cookie a module set into the jars, and returns them.
//
// Setting a name it already holds replaces the value **and** moves the entry to
// the end, so that the eviction packJar's bound forces takes the least recently
// written rather than the alphabetically unlucky. A name changing lifetime class
// moves between jars rather than existing in both.
func applyToJar(session, kept []jarEntry, c Cookie, now time.Time) (_, _ []jarEntry) {
	session = withoutName(session, c.Name)
	kept = withoutName(kept, c.Name)
	switch {
	case c.MaxAge < 0:
		// A deletion, and it is already done: the entry is out of both jars.
	case c.MaxAge == 0:
		session = append(session, jarEntry{Name: c.Name, Value: c.Value})
	default:
		kept = append(kept, jarEntry{
			Name: c.Name, Value: c.Value,
			Exp: now.Add(time.Duration(c.MaxAge) * time.Second).Unix(),
		})
	}
	return session, kept
}

func withoutName(entries []jarEntry, name string) []jarEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return out
}

// packWithEviction packs a jar, dropping the oldest entries until it fits, and
// returns what survived along with the value.
//
// **The survivors are returned because they are not the entries it was handed**,
// and the difference reaches a browser. [jarMaxAge] writes the kept jar's own
// lifetime from the longest-lived entry in it; given the list before eviction it
// writes a lifetime for an entry that is no longer there, which is exactly the
// cookie-kept-in-order-to-hand-back-nothing that function exists to avoid. The
// oldest entry goes first and the longest-lived one is often the oldest — a
// 400-day value set once at the start of a flow, then buried under short-lived
// ones — so this is the ordinary case rather than a contrived one.
//
// It returns how many it dropped so the caller can say so once, in the log,
// naming the add-on: an add-on that overfills its own jar is losing values it
// thinks it wrote, and the only person who can act on that is the operator who
// installed it.
func packWithEviction(entries []jarEntry) (string, []jarEntry, int, error) {
	dropped := 0
	for {
		value, err := packJar(entries)
		if err == nil {
			return value, entries, dropped, nil
		}
		if len(entries) == 0 {
			// One entry, alone, over the bound. Not reachable through the routing
			// path — decodeResponse packs a module's own cookies before the response
			// is accepted, so a value this large was refused at the call the module
			// made — and answered rather than looped on.
			return "", nil, dropped, err
		}
		entries = entries[1:]
		dropped++
	}
}

// jarCookies is the whole of what the host writes for one add-on's response:
// what the browser already held, with what the module just set folded in,
// packed, and expressed as at most one cookie per lifetime class.
//
// A jar that has become empty is written as a deletion when the browser sent one
// and omitted entirely when it did not, so the count of Set-Cookie headers on an
// add-on's response is at most two whatever happens, and is zero on the ordinary
// response that sets nothing.
func jarCookies(addon string, sent []*http.Cookie, prefixes []string, set []Cookie,
	now time.Time) (out []JarCookie, dropped int) {
	if len(set) == 0 {
		return nil, 0
	}
	session, kept := jarsFrom(addon, sent, prefixes, now)
	for _, c := range set {
		session, kept = applyToJar(session, kept, c, now)
	}
	held := func(name string) bool {
		for _, c := range sent {
			if c.Name == name {
				return true
			}
		}
		return false
	}
	for _, class := range []struct {
		kept    bool
		entries []jarEntry
	}{{false, session}, {true, kept}} {
		name := jarName(addon, class.kept)
		value, survived, lost, err := packWithEviction(class.entries)
		dropped += lost
		if err != nil || value == "" {
			if held(name) {
				out = append(out, JarCookie{Name: name, MaxAge: -1})
			}
			continue
		}
		c := JarCookie{Name: name, Value: value}
		if class.kept {
			c.MaxAge = jarMaxAge(survived, now)
		}
		out = append(out, c)
	}
	return out, dropped
}

// jarMaxAge is how long the kept jar has to live: as long as the longest-lived
// thing in it, and no longer. A jar outliving every entry it holds would be a
// cookie a browser keeps in order to hand back nothing.
//
// *In it* is the load-bearing word and is why the entries reach here from
// [packWithEviction] rather than from the list handed to it. Given the list
// before eviction this wrote a lifetime for a value the browser was not being
// sent — the sentence above, made false by the line that called it.
//
// Held to [maxCookieAge] at the top, and that is not the same check checkCookie
// makes. An entry's Exp reaches this function from one of two places: applyToJar,
// where checkCookie has already bounded it, or a jar the visitor's browser handed
// back — which is a value under the visitor's hand, so an Exp of 1<<62 is a thing
// to write a sane attribute for rather than a thing to refuse. This is the host's
// own cookie and its own attribute; the ABI's no-silent-correction promise is
// about a module's record, and no module wrote this one.
func jarMaxAge(entries []jarEntry, now time.Time) int {
	longest := 1
	for _, e := range entries {
		if remaining := int(e.Exp - now.Unix()); remaining > longest {
			longest = remaining
		}
	}
	if longest > maxCookieAge {
		return maxCookieAge
	}
	return longest
}

// EncodeRequestBody is how internal/httpx puts a body into a [Request]: as text
// when it is UTF-8, and as base64 when it is not, saying which.
func EncodeRequestBody(body []byte) (string, bool) {
	if utf8.Valid(body) {
		return string(body), false
	}
	return base64.StdEncoding.EncodeToString(body), true
}
