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
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
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
// visible to every replica. The cost is that memory, about 2.4 MB of guest
// linear memory per in-flight request, which is what [maxConcurrentRoutes]
// bounds.

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

// maxConcurrentRoutes bounds how many add-on requests are in flight at once,
// across every add-on.
//
// It exists because add-on routes are reachable without a session (D261) and
// each in-flight request holds an instance's linear memory — about 2.4 MB
// measured at M60. Unbounded, a flood of anonymous requests to an add-on's
// prefix is a memory exhaustion with no session to rate-limit against; bounded,
// it is a queue. Sixteen is the number: about 38 MB of guest memory at
// saturation, which is affordable on the smallest deployment this product
// documents, and well above what a dashboard's add-on pages see.
//
// A request that cannot get a slot waits, and waits on the request's own
// context, so what bounds the wait is the deadline every application request
// already carries (LINKCTRL_HTTP_REQUEST_TIMEOUT). It is [ErrBusy] after that rather
// than a page that arrives too late to be read.
const maxConcurrentRoutes = 16

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
}

// record turns a request into the HTTPRequest a guest sees, keeping only what
// the ABI says crosses.
func (in RequestIn) record(prefixes []string) Request {
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
		for _, p := range prefixes {
			if strings.HasPrefix(c.Name, p) {
				req.Cookies[c.Name] = c.Value
				break
			}
		}
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
func (h *Host) Route(ctx context.Context, name string, in RequestIn, sess SessionContext) (Response, error) {
	if h == nil {
		return Response{}, ErrNoRoute
	}
	var target *Loaded
	for i := range h.loaded {
		if h.loaded[i].Manifest.Name == name && h.loaded[i].ServesRoutes() {
			target = &h.loaded[i]
			break
		}
	}
	if target == nil || target.compiled == nil {
		return Response{}, ErrNoRoute
	}

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
	if !target.grants.Has(PermissionSessionContext) {
		// The grant is what the session read costs, and the cheapest place to
		// enforce it is to have nothing to read: an add-on that did not declare it
		// gets dispatch's StatusDenied, and this makes the value absent as well, so
		// no future edit to that check can turn into a disclosure.
		sess = SessionContext{}
	}
	req := in.record(target.Manifest.CookiePrefixes)
	st := base.forRequest(&req, sess)

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
	mod, err := h.runtime.InstantiateModule(ctx, target.compiled,
		wazero.NewModuleConfig().
			WithName(instance).
			WithStartFunctions(StartFunction))
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
		// an error page is not where an add-on's stack belongs.
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
	return *st.response, nil
}

// RoutedAddons is every loaded add-on holding the routes grant, in discovery
// order. It is what an operator's boot log and M68's manager read; the routing
// path itself does not consult it.
func (h *Host) RoutedAddons() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, l := range h.loaded {
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
	return r, nil
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
	var owned bool
	for _, p := range prefixes {
		if strings.HasPrefix(c.Name, p) {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("cookie %q is outside the prefixes this add-on declared (%v); "+
			"a namespace is owned in both directions", c.Name, prefixes)
	}
	return nil
}

// EncodeRequestBody is how internal/httpx puts a body into a [Request]: as text
// when it is UTF-8, and as base64 when it is not, saying which.
func EncodeRequestBody(body []byte) (string, bool) {
	if utf8.Valid(body) {
		return string(body), false
	}
	return base64.StdEncoding.EncodeToString(body), true
}
