package addon

import (
	"context"
	"path"
	"slices"
	"strings"
)

// This file is M69.5: the one link that makes M65's minting and M64's routes
// usable by somebody who was not handed a URL.
//
// # Three things have to be true before a link is drawn
//
//  1. **The add-on asked.** Its manifest declares [Manifest.SignInLabel] and
//     [Manifest.SignInPath]. Neither is a destination — the host composes the
//     target from [RoutePrefix], the add-on's own name and the declared path, and
//     then asserts the result is still under the prefix it gave that add-on. This
//     is D364's shape on a second surface: the manifest names a place inside the
//     one it was already given, and cannot name a place outside it.
//  2. **The operator agreed.** [SignInConsentSetting] is a toggle on the Add-on
//     manager's detail page, stored in `addon_settings` like every other answer an
//     operator types there, and it is **off** until they turn it on. An add-on's
//     author must not be able to change what a visitor sees by shipping a new
//     version, which is the whole reason the manifest declares a need rather than
//     a state.
//  3. **The module is running.** The set walked below is what the host
//     [Host.routed] — loaded, holding `routes.own_prefix`, compiled — and not what
//     a directory claims.
//
// That third condition is **the opposite of the answer M67 reached** for its
// name-collision check, which runs over what a directory *claims* (D346), and the
// two are right for opposite reasons. A collision must be refused on a claim,
// because the harm is a boot that refuses both add-ons; a link must be drawn on a
// module that runs, because the harm is a dead link on the sign-in page. Stated
// here because a reader who knows M67 will otherwise think this has it backwards.
//
// # The consent is the host's setting, not the add-on's
//
// It rides M68's mechanism entirely — the same table, the same save, the same
// form, the same `PUT /api/v1/addons/{name}/settings` — so the inherited *every UI
// feature has API support* rule is discharged by M68 and no host operation is
// added here. What it does not do is come from the manifest's own `settings`
// list, and that is the load-bearing part: a manifest-declared toggle carries a
// manifest-declared **default**, so an author could ship `"default": "true"` and
// put themselves on the front door of every instance that installed them. The
// host declares this one, its default is off, and [Manifest.Validate] refuses an
// add-on that tries to declare a setting by the same name.
//
// It is deliberately **not** in what [Host.resolveSettings] hands the module, so
// `config_get` cannot read it: whether this product draws a link on its own
// sign-in page is not the add-on's business, and a module that could read the
// operator's answer would be a module that could behave differently depending on
// it.

// SignInConsentSetting is the toggle an operator turns on to let an add-on's
// sign-in link appear.
//
// A name, spelled like any other setting, because it is rendered by M68's
// existing form and saved by [Host.SaveSettings] — nothing about it is special to
// the page. What is special is who declares it: this host, for every add-on that
// asked, and no manifest may take the name.
const SignInConsentSetting = "sign_in_link"

// signInConsentDeclaration is the setting the host adds to an add-on that
// declared a sign-in link.
//
// A toggle with an explicit `"false"` default, so the manager renders an unticked
// box and [Host.settingViews]' unset branch says the same thing the store does:
// off until somebody says otherwise.
func signInConsentDeclaration() Setting {
	return Setting{Name: SignInConsentSetting, Type: SettingToggle, Default: "false"}
}

// managedSettings is what the Add-on manager renders and saves for one add-on:
// everything its manifest declared, plus the host's consent toggle when it asked
// for a sign-in link.
//
// **Not what the module reads.** [Host.resolveSettings] and [settingNames] stay on
// the manifest's own list, which is what keeps `config_get` unable to see the
// operator's answer — see this file's header.
func managedSettings(m Manifest) []Setting {
	if m.SignInLabel == "" {
		return m.Settings
	}
	out := make([]Setting, 0, len(m.Settings)+1)
	out = append(out, m.Settings...)
	return append(out, signInConsentDeclaration())
}

// SignInLink is one add-on's offer on the sign-in page.
//
// [Href] is the host's composition and never the add-on's string; [Label] is the
// add-on's string and is hostile input, rendered through html/template like every
// other value on every other page. [Addon] is the module it came from, which the
// page does not draw — it is what a log line or a test names.
type SignInLink struct {
	Addon string
	Label string
	Href  string
}

// SignInHref is where this add-on's sign-in link points, and whether there is one.
//
// **The composition is the host's, and the assertion is the point.** The declared
// path is joined onto the prefix this add-on was already given and the result is
// held against that same prefix, so a path that climbs out of it draws no link
// whatever the manifest said. [Manifest.Validate] refuses the shapes that would
// try — a leading separator, a scheme, a dot segment — and this is the second
// half: validation is what a publisher hears, and this is what a visitor gets,
// and neither is trusted to be the only one.
func (l Loaded) SignInHref() (string, bool) {
	label, ok := l.SignInLabel()
	if !ok || label == "" {
		return "", false
	}
	if l.Manifest.SignInPath == "" {
		return "", false
	}
	prefix := l.PathPrefix()
	href := path.Join(prefix, l.Manifest.SignInPath)
	// path.Join cleans, so `..` has already been resolved by the time this asks —
	// which is exactly why the question is asked of the *result* rather than of the
	// declaration.
	if !strings.HasPrefix(href, prefix) {
		return "", false
	}
	return href, true
}

// SignInLabel is the words this add-on asked to have drawn, held against the
// bound a second time.
//
// [Manifest.Validate] refuses an over-long label at load, so this can only fire
// on a [Loaded] built by hand — and it fires rather than truncating, because a
// label cut in half is a button whose text somebody chose and nobody wrote.
func (l Loaded) SignInLabel() (string, bool) {
	s := l.Manifest.SignInLabel
	if s == "" || len(s) > MaxSignInLabelBytes {
		return "", false
	}
	return s, true
}

// SignInLinks is what the sign-in page draws, ordered by add-on name.
//
// **Sorted here rather than taken from the loaded set**, which is the one place
// the two available answers come apart. The loaded set is discovery order —
// [os.ReadDir]'s over the add-ons directory, which is sorted, and an add-on's
// directory *is* its name — but M67's runtime install **appends**, so an add-on
// installed without a restart sits last until the next boot and then moves. Both
// halves of that are orders the host controls, and neither is stable: a link's
// position would change on a restart nobody connected to it. Sorting by name is
// the same order at boot and the only one that survives an install.
//
// It is deliberately nothing a manifest can influence: *which sign-in method is
// listed first* is worth gaming, and an ordering field would be the one thing in
// this design an author could use to outrank another author.
//
// Nil-safe on the host, which is every instance that configured no add-ons
// directory, and returns nil when nothing qualifies — the sign-in page renders
// byte-identically for both.
func (h *Host) SignInLinks(ctx context.Context) []SignInLink {
	if h == nil {
		return nil
	}
	var out []SignInLink
	for _, l := range h.current().loaded {
		if !l.CanMint() {
			continue
		}
		label, ok := l.SignInLabel()
		if !ok {
			continue
		}
		href, ok := l.SignInHref()
		if !ok {
			continue
		}
		// Loaded is not enough: the link points at a route, so the test is the one
		// the router itself makes. A module that did not compile, or that never held
		// `routes.own_prefix`, would be a link to this instance's own 404.
		if h.routed(l.Manifest.Name) == nil {
			continue
		}
		if !h.signInConsented(ctx, l.Manifest.Name) {
			continue
		}
		out = append(out, SignInLink{Addon: l.Manifest.Name, Label: label, Href: href})
	}
	slices.SortFunc(out, func(a, b SignInLink) int { return strings.Compare(a.Addon, b.Addon) })
	return out
}

// CanMint reports whether this add-on holds the grant that lets it sign somebody
// in. Read off the resolved grants rather than the manifest, because a link is
// drawn on what a module can *do* — the same direction [Host.SignInLinks] takes
// for everything else about the offer.
func (l Loaded) CanMint() bool { return l.grants.Has(PermissionSessionMint) }

// signInConsented is the operator's answer, read from the same rows M68 stores.
//
// Read at render rather than held, and the cost is one indexed query on the
// sign-in page **only for an instance that installed an add-on which asked** — a
// stock instance reaches this for nothing, because the walk above filters on the
// manifest first. Holding it would mean a fourth thing the pool drain and the
// install path have to keep true, for a page nobody renders in a loop.
//
// A host with no database has no stored answers and therefore no consent, which
// is the safe direction: off until an operator says otherwise is exactly what the
// absence of a row means.
func (h *Host) signInConsented(ctx context.Context, name string) bool {
	row, ok := h.storedSettings(ctx, name)[SignInConsentSetting]
	return ok && row.Value == "true"
}
