package abi

import "slices"

// PermissionStorage is the one entry in the vocabulary another file branches on
// by name, so it is the one with a constant.
//
// The host's manifest validation needs it: an add-on that ships migrations has to
// have declared this, because the schema those migrations run inside is what this
// grant grants. A second spelling of a permission name is the drift a closed
// vocabulary exists to prevent, and a test holds this constant against the slice
// below.
const PermissionStorage = "storage.own_schema"

// PermissionNetworkFetch is the second entry another file branches on by name,
// and for the same reason [PermissionStorage] is the first: the host's manifest
// validation needs it. An add-on that declares an origin setting has to have
// declared this grant, and one that declares this grant has to declare an origin
// setting — a manifest holding only the first half is asking an operator to
// authorize a reach nothing will use, and one holding only the second could
// never fetch anything it was pointed at.
const PermissionNetworkFetch = "network.fetch"

// Permissions is the add-on permission vocabulary: every grant an add-on may
// declare, and the whole of what one can be trusted with.
//
// **Closed and enumerated**, the same discipline `?src=` uses: adding an entry is
// a code change, and a test asserts the set literally, so a vocabulary that grew
// a token cannot do it quietly. It grew one at M64 — `session.context`, which the
// phase plan had not separated from the routes grant — and that is what the
// discipline is for: the seventh entry arrived in a diff, with the test that names
// the set edited in the same commit and D258 saying what riding on
// `routes.own_prefix` would have cost instead.
//
// Eight entries, one per limb this phase lands plus that one and the one M66
// added, because the enforcement (M62) had to be built before any capability
// worth abusing existed — a grant declared here and implemented later is refused
// by an already-enforced permission rather than by a check somebody remembers to
// add.
//
// It lives in this package and not in the host, for the reason [Functions] does:
// this is the ABI's authoring point, the SDK and the published table are
// generated from it, and a publisher needs to know which permission a function
// costs before they write the manifest that declares it. The host resolves a
// manifest's declarations against this slice and answers [StatusDenied] for
// anything it does not hold; nothing in this package enforces, and nothing in it
// needs a runtime to be read.
//
// # Three grants for the redirect path, and each is a step down the same ladder
//
// `redirect.observe` and `redirect.inline` are deliberately separate, which is
// the owner's first requirement on the redirect answer: an add-on declares
// whether it watches redirects out of band or sits in the path itself, and a
// module cannot acquire the second by accident. `redirect.inline` was
// **[Permission.Grantable] false** from M62 until M66, which is the milestone
// that admitted an add-on onto that path — so the behaviour landed against a
// permission that was already enforced, which is the whole reason it was
// declared two milestones early.
//
// `redirect.rewrite_query` is the third rung and it is M66's own (D317). Holding
// `redirect.inline` buys observation and refusal; **altering the destination's
// query costs a token of its own**, because the owner's rule is that an add-on
// says what it will do before it is allowed to do it. That is the same argument
// that made observe and inline two grants rather than one, applied one level
// down: a manifest that declared *run on the path* should not turn out to have
// declared *and edit where the visitor goes*.
var Permissions = []Permission{
	{
		Name: "config.read", Grantable: true, BackedBy: "M61",
		Doc: "Read this add-on's own declared settings. It is the narrowest grant here " +
			"and it is still a grant: the manifest's `settings` list says which keys " +
			"exist, and this says whether the module may read any of them at all.",
	},
	{
		Name: PermissionStorage, Grantable: true, BackedBy: "M63",
		Doc: "Read and write the Postgres schema this add-on owns, whole. The schema " +
			"boundary is the whole of the grant — there is no row-level or column-level " +
			"form of it, and nothing here names another add-on's schema or this " +
			"product's own tables. It does not stop you giving your own schema away: " +
			"a `GRANT` on what you own works, and the host reports it at your next " +
			"load and refuses you until it is revoked. Migrations are the host's and " +
			"are not this grant.",
	},
	{
		Name: "routes.own_prefix", Grantable: true, BackedBy: "M64",
		Doc: "Serve requests under the path prefix this add-on owns, and render its own " +
			"templates through the host's renderer. One grant rather than two: a module " +
			"renders a fragment in order to answer a request, and a template rendered " +
			"for nobody is not a capability.",
	},
	{
		Name: "session.context", Grantable: true, BackedBy: "M64",
		Doc: "Ask the host who is signed in: identity, workspace and organization, and " +
			"nothing else. Its own token rather than a thing a page-serving add-on gets " +
			"for free, because `routes.own_prefix` is read as *this add-on draws a page* " +
			"and identity is a second answer — a manifest declaring one grant should not " +
			"turn out to have declared two. It is the read half and the whole of it: " +
			"there is no cookie, no token and no session row behind it, and minting or " +
			"destroying a session is session.mint.",
	},
	{
		Name: "session.mint", Grantable: true, BackedBy: "M65",
		Doc: "Tell the host that somebody authenticated, and ask for a session — and " +
			"connect an external identity to the account of whoever is already signed " +
			"in, which is `identity_link` and is the same grant. **Two functions, one " +
			"token**, because a module that can vouch for a person can already decide " +
			"who is signed in; splitting them would let an operator grant the writing " +
			"of a standing credential without the asserting that spends it, which is " +
			"not a safer half. The highest-value grant in this vocabulary: a module " +
			"holding it decides who is signed in, subject to the host's own judgement " +
			"about whether an account exists and what the session may do.",
	},
	{
		Name: "redirect.observe", Grantable: true, BackedBy: "M66",
		Doc: "Observe redirects this instance served, out of band. What crosses is at " +
			"most what click_events may carry — prefix-derived and country-level, and no " +
			"client address in any form — so this grant cannot be widened into one by " +
			"the host implementing it.",
	},
	{
		Name: "redirect.inline", Grantable: true, BackedBy: "M66",
		Doc: "Run inside the redirect path itself, where a module's own latency is added " +
			"to the response. Distinct from redirect.observe so that a module cannot " +
			"acquire it by accident. What it buys is the decision and a verdict on it: " +
			"the module is handed the destination this instance has chosen and may let " +
			"it stand or veto it, and a veto is the same refusal a gate answers with. " +
			"What it does not buy is the rest of the ABI — an inline invocation may call " +
			"only the redirect-safe subset, so there is no storage, no request, no " +
			"session and no template on the hot path, whatever the manifest declared. " +
			"Nor does it buy editing the destination, which is redirect.rewrite_query. " +
			"The host bounds how long the module holds the path and completes the " +
			"redirect without it when that runs out; the latency it adds inside that " +
			"bound is the add-on's own, and this product's published redirect promise " +
			"is measured with no inline add-on on the path.",
	},
	{
		Name: "redirect.rewrite_query", Grantable: true, BackedBy: "M66",
		Doc: "Alter the query string of the destination an inline module was handed, and " +
			"nothing else about it: the scheme, the host, the port and the path are the " +
			"host's and are unchanged by construction, because the host substitutes the " +
			"query into the URL it already decided rather than accepting one the module " +
			"wrote. That bound is what keeps the destination validator's single door " +
			"single — every tier above the SSRF refusals judges by host, so a query the " +
			"module chose cannot change any tier's verdict. It is a second token rather " +
			"than something redirect.inline implies (D317): stripping fbclid or " +
			"appending a privacy parameter is a sharper power than watching and " +
			"refusing, and a module cannot acquire it by having asked for the weaker " +
			"one. Useless on its own — an add-on that declares this and not " +
			"redirect.inline is never on the path to use it.",
	},
	{
		Name: PermissionNetworkFetch, Grantable: true, BackedBy: "M68.5",
		Doc: "Make an outbound request from the host, to an origin **the operator named** " +
			"and to no other. This grant carries no hosts, no patterns and no URLs, and it " +
			"cannot: an add-on's author declares that the add-on talks to something, and the " +
			"person running the instance decides what that something is, by filling in a " +
			"setting the manifest declared as carrying origins. An add-on holding this and " +
			"configured with nothing reaches nothing, which is the ordinary state of one " +
			"that has just been installed. What the host enforces beyond the origin is not " +
			"negotiable by either party: https only, GET and form-encoded POST only, no " +
			"request headers of the add-on's choosing, every address the name resolves to " +
			"checked at the moment of dialling so that loopback, link-local, unique-local " +
			"and the private ranges are refused, no redirect followed off the origin it " +
			"started on, a response size cap and a request timeout. It is the sharpest " +
			"grant here after session.mint, and it composes with storage.own_schema into " +
			"something worth stating plainly: an add-on holding both can read its own " +
			"tables and send what it finds to the origin the operator authorized.",
	},
}

// Permission is one entry in the vocabulary.
type Permission struct {
	// Name is what a manifest declares, and it is spelled the way this product's
	// own permissions are — dotted lowercase, as in links.read — because an
	// add-on's grants will be read beside API-key scopes on the same page.
	Name string
	// Grantable is whether any add-on may hold this permission on this host. False
	// is the deliberate case, not an error state: a class defined ahead of the
	// milestone that implements it is declarable, refused, and counted.
	Grantable bool
	// BackedBy is the milestone whose behaviour this grant admits. Present for the
	// reason [Function.BackedBy] is: it is what lets the mid-phase review read the
	// vocabulary against what was actually built.
	BackedBy string
	// Doc is the paragraph the published table carries.
	Doc string
}

// PermissionByName looks one up. Absent means the token is outside the closed
// vocabulary, which the host treats as a manifest it cannot interpret.
func PermissionByName(name string) (Permission, bool) {
	i := slices.IndexFunc(Permissions, func(p Permission) bool { return p.Name == name })
	if i < 0 {
		return Permission{}, false
	}
	return Permissions[i], true
}

// Grantable reports whether a declared permission may be held on this host. A
// token outside the vocabulary is not grantable either, and the caller is
// expected to have refused it earlier with a better message.
func Grantable(name string) bool {
	p, ok := PermissionByName(name)
	return ok && p.Grantable
}

// PermissionNames is the vocabulary as a list, for an error message that has to
// name what was allowed.
func PermissionNames() []string {
	out := make([]string, 0, len(Permissions))
	for _, p := range Permissions {
		out = append(out, p.Name)
	}
	return out
}
