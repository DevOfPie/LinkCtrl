package abi

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
		"applies its own Secure, HttpOnly and SameSite attributes",
}
