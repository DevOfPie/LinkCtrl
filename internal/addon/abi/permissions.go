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

// Permissions is the add-on permission vocabulary: every grant an add-on may
// declare, and the whole of what one can be trusted with.
//
// **Closed and enumerated**, the same discipline `?src=` uses: adding an entry is
// a code change, and a test asserts the set literally, so a vocabulary that grew
// a seventh token cannot do it quietly. Six entries, one per limb this phase
// lands, because the enforcement (M62) had to be built before any capability
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
// # Two grants for the redirect path, and only one of them exists
//
// `redirect.observe` and `redirect.inline` are deliberately separate, which is
// the owner's first requirement on the redirect answer: an add-on declares
// whether it watches redirects out of band or sits in the path itself, and a
// module cannot acquire the second by accident. `redirect.inline` is therefore
// **[Permission.Grantable] false** and no host grants it until M66 lands. It is
// defined now rather than then so that M66 enforces behaviour against a
// permission that is already enforced.
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
		Name: "session.mint", Grantable: true, BackedBy: "M65",
		Doc: "Tell the host that somebody authenticated, and ask for a session. The " +
			"highest-value grant in this vocabulary: a module holding it decides who is " +
			"signed in, subject to the host's own judgement about whether an account " +
			"exists and what the session may do.",
	},
	{
		Name: "redirect.observe", Grantable: true, BackedBy: "M66",
		Doc: "Observe redirects this instance served, out of band. What crosses is at " +
			"most what click_events may carry — prefix-derived and country-level, and no " +
			"client address in any form — so this grant cannot be widened into one by " +
			"the host implementing it.",
	},
	{
		Name: "redirect.inline", Grantable: false, BackedBy: "M66",
		Doc: "Run inside the redirect path itself, where a module's own latency is added " +
			"to the response. Distinct from redirect.observe so that a module cannot " +
			"acquire it by accident, and **no host grants it yet**: it is declared here " +
			"so the milestone that admits an add-on onto that path enforces behaviour " +
			"against a permission that is already enforced.",
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
