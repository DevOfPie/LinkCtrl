package abi

import "slices"

// # The calling convention
//
// Every function in this ABI has the same shape, and the uniformity is the
// point: one convention is one thing for a publisher to learn, and it is what
// lets the SDK and the host module both be derived from [Functions] rather than
// written twice.
//
//   - Every function returns a single i32. Zero or a positive number is success;
//     a negative number is one of the [Statuses] below. Nothing is returned
//     through memory that is not also described by a parameter.
//   - A value the guest passes in crosses as a (pointer, length) pair —
//     [String] for UTF-8 text, [Bytes] for anything else. A zero length is
//     legal and a nil pointer with it is legal; the host reads nothing.
//   - A value the host passes back crosses into a buffer the *guest* owns,
//     described by an [OutString] or [OutBytes] parameter as a (pointer,
//     capacity) pair. The return value is the number of bytes the value
//     occupies. If that exceeds the capacity the guest offered, **nothing was
//     written** and the guest retries with a larger buffer — which is the whole
//     protocol, and is why a caller never has to ask the host for a size first.
//     The generated SDK does the retry; a hand-written consumer has to.
//   - At most one out parameter, and it is last. Asserted by test.
//
// The host never allocates in the guest, which is why the convention looks like
// this rather than returning a pointer. A guest that exports an allocator hands
// the host a way to run guest code at a moment the guest did not choose, and the
// first thing that reaches for is a module that traps inside the allocator while
// the host holds a lock.
//
// # Structured payloads
//
// Where a function carries more than one value it carries a [Record], and a
// record crosses as a JSON object. JSON because the boundary is a published
// contract between two repositories on different release cycles, and adding a
// field to a JSON object is additive for every consumer that ignores what it
// does not know — which is exactly what docs/addon-abi.md needs "not breaking"
// to mean. The cost is a marshal per call and it is accepted; the redirect path's
// budget is M66's to price, against a shape that is already in the tree.

// Kind is how one parameter crosses the boundary.
type Kind uint8

const (
	// String is guest-supplied UTF-8, as (pointer, length). The host refuses
	// invalid UTF-8 with StatusInvalid rather than substituting replacement
	// characters, because a name it cannot round-trip is a name it should not
	// have accepted.
	String Kind = iota + 1
	// Bytes is guest-supplied opaque bytes, as (pointer, length).
	Bytes
	// Int32 is a scalar.
	Int32
	// Int64 is a scalar.
	Int64
	// OutString is a guest-owned buffer the host fills with UTF-8, as
	// (pointer, capacity).
	OutString
	// OutBytes is a guest-owned buffer the host fills with bytes, as
	// (pointer, capacity).
	OutBytes
)

// Out reports whether this kind is a buffer the host writes into.
func (k Kind) Out() bool { return k == OutString || k == OutBytes }

// Words is how many i32/i64 wasm parameters this kind occupies.
func (k Kind) Words() int {
	if k == Int32 || k == Int64 {
		return 1
	}
	return 2
}

// GoType is how the generated SDK spells this kind on the Go side. An out
// parameter has no Go parameter at all — it becomes the wrapper's result.
func (k Kind) GoType() string {
	switch k {
	case String:
		return "string"
	case Bytes:
		return "[]byte"
	case Int32:
		return "int32"
	case Int64:
		return "int64"
	case OutString:
		return "string"
	case OutBytes:
		return "[]byte"
	default:
		return ""
	}
}

// Param is one parameter of one ABI function.
type Param struct {
	Name string
	Kind Kind
	// Doc is one sentence, emitted into the SDK's doc comment. When the value
	// crosses as a Record, the sentence names it — "as an HTTPRequest record" —
	// and abi_test.go holds the sentence and the Records slice together, because
	// this sentence is what an add-on's author reads and the record is what the
	// privacy and credential assertions walk.
	Doc string
	// GuestShaped marks a JSON payload this ABI deliberately does not describe,
	// because the host does not author it: an add-on's own query arguments, the
	// rows of its own schema, the data for its own template. It is the only
	// alternative to naming a record, and it is a declaration rather than a
	// silence — an out parameter that said "a JSON object" and named nothing was
	// how session_mint came to promise a payload no test could see.
	GuestShaped bool
}

// Status is the negative return value a host function uses to refuse.
//
// Negative, so a successful read can return a length in the same i32 without a
// second out parameter to hold the error. Small and closed: a guest switches on
// these, and a status invented per call site is a status nobody can handle.
type Status int32

const (
	// StatusOK is not returned as such — a success is a length, and zero is a
	// legal length. It exists so that a reader of this list is not left
	// wondering what zero means.
	StatusOK Status = 0
	// StatusInternal is the host failing at something that is not the guest's
	// fault. The host has already logged it with the detail; the guest gets a
	// number because an error string across this boundary is an error string an
	// add-on can print into somebody's page.
	StatusInternal Status = -1
	// StatusNotAvailable is a function that this ABI declares and this host does
	// not yet implement. It is the distinguishable refusal m61.md requires: the
	// contract is complete on paper one milestone before it is complete in
	// behaviour, and a module that probes for a capability gets an answer it can
	// branch on rather than a link failure it cannot.
	//
	// Making a declared function available is **not** a breaking change —
	// docs/addon-abi.md says so — which is what stops the declared-but-refused
	// pattern from costing a major version per limb.
	StatusNotAvailable Status = -2
	// StatusDenied is a capability the add-on did not declare, or declared and
	// is not permitted. M62 is where most of these come from; config_get
	// already returns it for a key the manifest never declared.
	StatusDenied Status = -3
	// StatusNotFound is a well-formed request for something that is not there.
	StatusNotFound Status = -4
	// StatusInvalid is the guest's fault: a length that does not fit its memory,
	// text that is not UTF-8, an argument outside its vocabulary.
	StatusInvalid Status = -5
)

// StatusDoc is one status, named for both sides of the boundary.
type StatusDoc struct {
	// Go is the identifier the SDK exports, as an error value.
	Go     string
	Status Status
	Doc    string
}

// Statuses is every status a host function may return, in the order the SDK and
// the documentation list them. The SDK's error values are generated from this,
// so a status added here is an error a consumer can compare against without
// anybody editing the SDK.
var Statuses = []StatusDoc{
	{"ErrInternal", StatusInternal, "the host failed at something that is not the add-on's fault; it has logged the detail"},
	{"ErrNotAvailable", StatusNotAvailable, "this ABI declares the function and this host does not implement it yet"},
	{"ErrDenied", StatusDenied, "the add-on did not declare this capability, or declared it and may not have it"},
	{"ErrNotFound", StatusNotFound, "a well-formed request for something that is not there"},
	{"ErrInvalid", StatusInvalid, "the arguments were the add-on's fault: a length outside its memory, text that is not UTF-8, or a value outside the vocabulary"},
}

// LogLevels is the vocabulary the log function's level argument accepts, and the
// SDK's level constants are generated from it.
//
// Text rather than an integer, deliberately. An integer needs a second
// enumeration on the guest side to be readable, and the two drift; a string is
// validated once by the host, is what slog uses anyway, and costs four bytes per
// log line on a path that is writing a log line.
var LogLevels = []string{"debug", "info", "warn", "error"}

// HostModule is the wasm module name every function below is imported from.
//
// One module, not one per capability group, and not a name carrying the version.
// Path versioning was the recommendation the owner declined; encoding the
// generation in the module name would be that recommendation arriving through
// the back door, and it would make the load-time check in CheckGeneration
// unreachable — a mismatch would surface as an unresolved import instead of as
// a refusal that names the version.
const HostModule = "linkctrl"

// GuestHTTPHandler is the one function this ABI requires a *module* to export:
// the host calls it to hand a request to an add-on holding `routes.own_prefix`.
//
// It takes no arguments and returns an i32 the same way a host function does — a
// negative number is one of [Statuses], anything else is success. The request is
// not passed in and the response is not returned out, because the convention
// already has a way to move a record across and inventing a second one for this
// direction would double what a publisher has to learn: the guest calls
// `http_request_read` to see what it was asked and `http_response_write` to
// answer, and both are refused outside a request.
//
// Prefixed, because a module's export namespace is flat and shared with whatever
// the toolchain puts in it. Named here rather than in the host so that the host
// looks up the same string a publisher writes in `//go:wasmexport` — the constant
// cannot appear in that directive, which is why the fixture writes the literal
// and a test proves the two agree by the module actually answering.
//
// A module that declares the routes grant and exports nothing is refused at the
// request rather than at load: the export is a property of the wasm the manifest
// names, and refusing an instance for it would take an instance down for a page
// nobody asked for. The host answers 500 and logs which export was missing.
const GuestHTTPHandler = "linkctrl_http_handle"

// GuestRedirectObserve and GuestRedirectInline are the two exports the redirect
// classes are called through (M66). A module exports the one its manifest's grant
// names, both, or neither; an add-on holding a redirect grant and exporting
// nothing is not an error, it is an add-on the host has nothing to call.
//
// **Two exports rather than one with a class argument**, and it is the same
// argument that made the two grants two: the classes differ in when they run,
// what they may read and whether anything they do can affect a visitor, so a
// module holding both writes two functions and cannot confuse which one it is in.
// A single entry point told *which mode this is* would put that distinction
// inside the guest, where the host cannot enforce it.
//
// Neither takes an argument and both return an i32 the way a host function does.
// The observe export reads its subject with `redirect_event_read`; the inline
// export reads `redirect_decision_read` and answers with `redirect_answer_write`,
// which is the same read-and-write convention the request handler uses and for
// the same reason — there is already a way to move a record across, and a second
// one would double what a publisher has to learn.
//
// A negative return is a refusal in the ABI's own vocabulary. On the inline path
// it is **not** a veto: a veto is a verdict and is written, and a module that
// failed is a module the host has no answer from, so the redirect proceeds
// unchanged. Making a trap mean *refuse the visitor* would turn every bug in an
// add-on into an outage of somebody's links.
const (
	GuestRedirectObserve = "linkctrl_redirect_observe"
	GuestRedirectInline  = "linkctrl_redirect_inline"
)

// InlineSafe is every function an inline redirect invocation may call, and it is
// the whole of the redirect tree's rule as it crosses this boundary.
//
// m66.md: *an inline module's host functions are the redirect-safe subset only —
// no storage I/O on the hot path*. Stated as a list rather than as a property of
// each function, for the reason the ungated set is: what belongs on the hot path
// is a judgement about the path and not about the function, and a boolean beside
// each entry would be a judgement made fifteen times by whoever added the
// sixteenth. Anything not here is [StatusDenied] inside an inline invocation,
// **whatever the manifest declared** — the grant is what an add-on may do, and
// this is where it may do it.
//
// What is on it: the four ungated host facts, this add-on's own settings, and the
// two redirect functions the class exists for. Every one of those is an in-memory
// read of something the host already has in hand. What is off it is everything
// that touches Postgres, the request, the session or a template — and the storage
// pair is the one the milestone names, because an add-on that owns tables is
// exactly the add-on that would write to them from here.
//
// A test holds the list against [Functions] in both directions, and asserts that
// nothing requiring [PermissionStorage] is on it.
//
// `network_fetch` is off it, and that is the one absence worth naming: it means an
// inline invocation's egress refusal is *this* one — [StatusDenied] from dispatch —
// and not the `class_refused` outcome the observing class gets. Two refusals for
// one rule, and a guest branches on two different things.
var InlineSafe = []string{
	"abi_version",
	"log",
	"random_bytes",
	"time_now",
	"config_get",
	"redirect_decision_read",
	"redirect_answer_write",
}

// CallableInline reports whether an inline redirect invocation may call this
// function.
func CallableInline(name string) bool { return slices.Contains(InlineSafe, name) }

// RedirectVerdicts is the closed vocabulary of [RedirectAnswer]'s verdict field.
// Empty is the ordinary answer and means allow, which is what a module that only
// watches writes.
var RedirectVerdicts = []string{"", "allow", "veto"}

// VerdictVeto is the one verdict that changes anything, named because the host
// branches on it.
const VerdictVeto = "veto"

// FetchMethods is the closed pair [Function] `network_fetch` will carry. Empty is
// GET, which is what a module reading a discovery document writes.
//
// Two, and the second exists only because a token exchange is a POST: OIDC's
// authorization-code flow turns a code into a token by posting a form, and
// nothing else this ABI is for needs a body at all. A method outside this pair is
// [StatusInvalid] rather than passed through, so the surface cannot grow a PUT by
// somebody forgetting to check.
var FetchMethods = []string{"", "GET", "POST"}

// FetchOutcomes is the closed vocabulary of [Record] FetchResponse's `outcome`
// field, and it is one vocabulary doing two jobs: a guest branches on it, and an
// operator reads the same word as the `outcome` label of
// `linkctrl_addon_fetch_total`.
//
// **One vocabulary rather than the five [Statuses]**, which is a deliberate
// departure from how every other refusal in this ABI is reported. The statuses
// cannot tell a timeout from a size cap from a refused address, and those are
// three different things for both readers — an add-on retries one of them and an
// operator investigates another. So the negative statuses keep what they are for
// here, the guest's own faults, and everything that happened because of where the
// add-on pointed the host comes back inside the record. Nothing traps and nothing
// is silently substituted.
//
// The set is closed and a test holds it against the host's mapping in both
// directions, for the reason [Permissions] is closed: a vocabulary that can grow a
// member in a diff nobody read is a label cardinality nobody bounded.
var FetchOutcomes = []string{
	// A response arrived. It says nothing about the status code, which is the
	// record's own field: reaching a server that answered 500 is this host doing
	// exactly what it was asked.
	"ok",
	// The operator has named no origin for this add-on, so it may reach nothing.
	// The ordinary state of an add-on nobody has configured yet, and the reason a
	// module that talks outward should say so on its own page rather than failing
	// mysteriously.
	"unconfigured",
	// The URL's origin is not one the operator named. A discovery document
	// pointing somewhere else lands here, which is the attack this bound exists
	// to stop.
	"origin_refused",
	// The invocation is not one that may fetch at all, whatever the manifest
	// declared: an observing redirect invocation, or the instance made at load. A
	// route handler is the only caller.
	//
	// **The inline class does not produce this word**, and the difference is
	// where the refusal happens rather than what it means. `network_fetch` is
	// outside [InlineSafe], so an inline invocation is refused by dispatch before
	// the fetch machinery is entered and gets [StatusDenied] — see that list's own
	// note. It is therefore not a counter label for that class either, which
	// docs/operations.md says in the operator's terms.
	"class_refused",
	// The request record was the add-on's fault — not a URL, not https, a method
	// outside FetchMethods, a body on a GET.
	"invalid_request",
	// The name did not resolve.
	"dns_failed",
	// The name resolved, and an address it resolved to is one this host will not
	// dial. The rule is an allowlist: an address is dialled only if it falls in
	// globally-routable unicast space, so loopback, link-local, unique-local, the
	// private ranges and anything nobody has thought about are all refused by
	// default rather than by an entry. Checked when the connection is made rather
	// than when the URL is parsed, so a name that answers differently the second
	// time is refused the second time. The host's log names which rule refused it,
	// under `address_rule=`.
	"address_refused",
	// A redirect pointed off the origin it started on. Same-origin redirects are
	// followed; nothing else is.
	"redirect_refused",
	// The response was larger than the host's cap. No body comes back — a
	// truncated document is a parse error blamed on the add-on's author.
	"too_large",
	// The host's fetch timeout elapsed, or what was left of the invocation's own
	// budget did.
	"timeout",
	// Anything else that went wrong on the wire: the connection refused, the TLS
	// handshake rejected, the peer hanging up mid-body. One outcome rather than a
	// taxonomy, because the add-on's response to all of them is the same and the
	// detail is in the host's log.
	"connect_failed",
}

// FetchOK is the one outcome that means a response arrived, named because both
// the host and a consumer branch on it.
const FetchOK = "ok"

// Function is one entry in the ABI: what a module may import.
type Function struct {
	// Name is the wasm import name, and it is the identity the SemVer promise is
	// about. Renaming one is a breaking change.
	Name string
	// Go is the identifier the generated SDK exports.
	Go string
	// Since is the ABI version this function first appeared in.
	Since string
	// BackedBy is the milestone whose behaviour this is. For a function that is
	// not Live, it is the milestone that will implement it — and it is why the
	// mid-phase review (M64.9) can read this set against what was built.
	BackedBy string
	// Live is whether this host implements it. False means declared and refused
	// with StatusNotAvailable.
	Live bool
	// Requires is the [Permission] the calling add-on must hold, or empty for a
	// function every module may call.
	//
	// The host checks it before it does anything else, including before it refuses
	// a function it has not implemented: an add-on that declared nothing gets
	// StatusDenied from `storage_query` rather than StatusNotAvailable, so the
	// ABI's own capability probe is not a way to enumerate a host's limbs without
	// declaring them. Empty is a deliberate answer and not an omission — a test
	// names the ungated functions literally.
	Requires string
	// Deprecated, when set, is the sentence the SDK emits as a Go "Deprecated:"
	// marker — which is what makes a deprecation reach a consumer's vet output
	// rather than only a changelog. RemovedNotBefore is the ABI version it may
	// be removed in, which docs/addon-abi.md's window fixes.
	Deprecated       string
	RemovedNotBefore string
	Params           []Param
	// Carries names the Records this function passes, in either direction. It is
	// what the privacy assertion in abi_test.go walks.
	Carries []string
	// Doc is the paragraph the SDK and the documentation table both carry.
	Doc string
}

// Out returns the function's out parameter, and whether it has one.
func (f Function) Out() (Param, bool) {
	for _, p := range f.Params {
		if p.Kind.Out() {
			return p, true
		}
	}
	return Param{}, false
}

// Field is one field of a Record.
type Field struct {
	Name string
	// Type is the JSON type, for the documentation table.
	Type string
	Doc  string
}

// Record is a structured payload an ABI function carries, as a JSON object.
type Record struct {
	Name string
	Doc  string
	// ClickDerived marks a record describing a redirect this instance served.
	// Such a record is bound by what click_events may carry — prefix-derived and
	// country-level — and abi_test.go enforces that against the column list in
	// the migration rather than against a list copied into this file. It is how
	// the privacy stance crosses the boundary as an ABI property instead of as
	// an audit of somebody else's DDL.
	ClickDerived bool
	// PrefixedCookies marks a record that carries cookies inbound, and it is a
	// claim about *which*: only the ones whose names match a prefix the add-on's
	// manifest declares, never the Cookie header. abi_test.go holds the two
	// together — a record carrying cookies and not marked, or marked and not
	// carrying them as an object, fails — so the flag cannot become a comment
	// that stopped being true.
	PrefixedCookies bool
	Fields          []Field
}

// AddressBearing is every field or parameter name that would put a client's
// address across this boundary, including the forms this product uses
// internally.
//
// A blocklist beside the shape test in abi_test.go, and it exists because the
// shape test alone would pass a field called `forwarded` or `visitor_addr`. No
// host function hands an add-on any of these: an add-on cannot store what it is
// never handed, which is the whole of m61.md's privacy bullet and the fifth
// inherited-rule collision.
var AddressBearing = []string{
	"ip", "ips", "ip_address", "ip_prefix", "client_ip", "remote_ip", "remote_addr",
	"peer_addr", "visitor_ip", "visitor_addr", "addr", "address", "cidr", "subnet",
	"x_forwarded_for", "forwarded", "x_real_ip", "true_client_ip", "cf_connecting_ip",
}

// CredentialBearing is every field or parameter name that would put a
// credential of the *host's* across this boundary. It is AddressBearing's
// counterpart, and it exists for the same reason: a bound stated as a property
// of the surface can be tested, and a bound stated as a promise about add-on
// code cannot be, because this project did not write that code.
//
// LinkCtrl's sessions are server-side and opaque, so the Cookie header **is**
// the credential — an add-on handed it verbatim could act as whoever is signed
// in, not by escaping the sandbox but by being given the key. D232 is the
// owner's answer; m64.md's "it cannot read the cookie" and m65.md's "never sees
// a token, a cookie, or the session row" are the two shipped assertions this
// list keeps true.
var CredentialBearing = []string{
	"cookie", "cookie_header", "raw_cookie", "cookies_raw", "http_cookie",
	"authorization", "auth_header", "session_cookie", "session_id",
	"session_token", "csrf", "csrf_token", "bearer", "token",
}

// CookieFields is every field name in this ABI allowed to carry cookies at all,
// each paired with the property that makes it safe. Anything else that reads as
// a cookie is refused by abi_test.go — the field that would have leaked the
// session was called `cookie`, and the next one to try would be called
// something else.
var CookieFields = map[string]string{
	"cookies": "inbound, and prefix-filtered: only the cookies whose names match " +
		"a prefix the add-on's manifest declares, and a declared prefix cannot " +
		"reach a cookie of the host's",
	"set_cookie": "outbound, and bounded by the same declared prefixes, because a " +
		"namespace an add-on owns is one it owns in both directions; the host " +
		"applies its own Secure, HttpOnly and SameSite attributes, and carries the " +
		"whole set inside one cookie of its own, so an add-on cannot fill a browser's " +
		"cookie store until this product's session cookie is evicted from it",
}
