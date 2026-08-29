package ui

import (
	"strings"
	"testing"
)

// The Add-on manager's two pages, where what is being asserted is what the page
// says rather than what the handler computed (M68).

// The purge confirmation names all four things a purge leaves standing, and
// names them whether or not there are any.
//
// `PurgeAddonSchema` is `DROP SCHEMA … CASCADE` and nothing else. Four things
// survive it — the `addon_<name>` login role, any large objects that role owns,
// every `addon_identity_links` row written under the name, and every
// `addon_settings` row written under it — and docs/SECURITY.md, docs/usage.md and
// CHANGELOG.md all say this confirmation states all four. It stated one: the role,
// plus the large objects when the count happened to be non-zero, and the identity
// links nowhere at all; the settings were added last, when M68's fifth review
// found the page saying *three* over a tree that left four.
//
// **The settings are the one with no route out at all.** A settings write is
// refused for a name that is not loaded, so once the add-on is gone nothing in the
// product can delete them — and the values, a stored secret among them, are read
// by whatever is installed under the name next.
//
// The links are the one worth the most here. They are keyed on the add-on's
// **name**, so whatever is installed under that name next inherits every account
// mapping the previous module wrote and can mint a session against them — and a
// purge is the act an operator takes believing it clears what the add-on left.
//
// Rendered in both states, because the defect this replaces was a sentence that
// existed only for a non-zero count: an assertion against one fixture would have
// passed against it.
func TestThePurgeConfirmationNamesEverythingItLeaves(t *testing.T) {
	subjects := []struct{ needle, why string }{
		{"login role", "the role survives, and re-installing under the name re-uses it"},
		{"large object", "large objects belong to the role, not the schema, so the drop leaves them"},
		{"external-identity link", "the mappings are keyed on the add-on's name, so " +
			"whatever is installed under it next inherits them"},
		{"saved setting", "the values are keyed on the name too, nothing in the " +
			"product deletes them, and one of them may be a secret"},
	}
	for _, state := range []struct {
		name string
		row  map[string]any
	}{
		{"nothing left over", map[string]any{
			"Name": "legacy_geo", "Schema": "addon_legacy_geo", "Size": "2.4 MiB",
			"LargeObjects": int64(0), "IdentityLinks": int64(0), "StoredSettings": int64(0),
		}},
		{"every count non-zero", map[string]any{
			"Name": "legacy_geo", "Schema": "addon_legacy_geo", "Size": "2.4 MiB",
			"LargeObjects": int64(3), "IdentityLinks": int64(11), "StoredSettings": int64(4),
		}},
	} {
		t.Run(state.name, func(t *testing.T) {
			body := renderPage(t, "addons", map[string]any{"PurgingOrphan": state.row})
			if !strings.Contains(body, "Delete this data") {
				t.Fatal("the purge confirmation did not render at all")
			}
			for _, s := range subjects {
				if !strings.Contains(body, s.needle) {
					t.Errorf("the confirmation does not mention %q: %s", s.needle, s.why)
				}
			}
		})
	}

	// And the counts are on the page when there are any, because a warning nobody
	// can size is a warning nobody can act on.
	loud := renderPage(t, "addons", map[string]any{"PurgingOrphan": map[string]any{
		"Name": "legacy_geo", "Schema": "addon_legacy_geo", "Size": "2.4 MiB",
		"LargeObjects": int64(3), "IdentityLinks": int64(11), "StoredSettings": int64(4),
	}})
	for _, want := range []string{
		"3 large object", "11 external-identity link", "4 saved setting",
	} {
		if !strings.Contains(loud, want) {
			t.Errorf("the confirmation does not carry %q", want)
		}
	}
	// And the count is a number, not a word: the sentence says *four things* and
	// then has to be able to say how many of each.
	if !strings.Contains(loud, "Four things are") {
		t.Error("the confirmation does not say how many things it leaves")
	}
}

// A `select` can be left unset, which needs an element to say it with.
//
// Every editable field on this form posts on every submission and a blank means
// *unset* — that is the reading `SaveSettings` takes, and it is what makes
// clearing a value possible without a control per setting. A `<select>` with no
// empty option cannot express it: the browser posts whichever option happens to
// be first, so a setting nobody has chosen arrives looking chosen, and one that
// has been chosen can never be put back.
//
// Both directions. The option is selected when there is no value, and not when
// there is — an empty option that is always selected would be a control that
// silently unsets whatever it renders.
func TestASelectCanBeLeftUnset(t *testing.T) {
	const empty = `<option value="" `
	unset := renderPage(t, "addon_manager", map[string]any{
		"Settings": []addonSettingStub{{
			Name: "grouping", Kind: "select", Options: []string{"day", "week"},
		}},
	})
	if !strings.Contains(unset, empty) {
		t.Fatal("a select with no stored value draws no empty option, so it cannot " +
			"stay unset through a save")
	}
	if !strings.Contains(unset, `<option value="" selected>`) {
		t.Error("the empty option is not the one selected for a setting that has no value")
	}

	chosen := renderPage(t, "addon_manager", map[string]any{
		"Settings": []addonSettingStub{{
			Name: "grouping", Kind: "select", Options: []string{"day", "week"},
			Value: "week", Configured: true,
		}},
	})
	if !strings.Contains(chosen, empty) {
		t.Error("a configured select draws no empty option, so its value cannot be cleared")
	}
	if strings.Contains(chosen, `<option value="" selected>`) {
		t.Error("the empty option is selected over a stored value, so rendering the " +
			"page and saving it would clear the setting")
	}
	if !strings.Contains(chosen, `<option value="week" selected>`) {
		t.Error("the stored value is not the selected option")
	}
}

// A module with no redirect grant shows no redirect figures, kills included.
//
// m68.md: "Modules holding no redirect grant show no redirect figures rather
// than zeros." The class table honoured that from the start; the two kill
// figures beside it did not, and rendered "Killed at call: 0 / Killed at
// instantiate: 0" for every module that has never been on the redirect path —
// which is every `none`-class module, the commonest kind there is. A zero there
// is a claim that the host tried and did not kill anything, when nothing ever
// ran.
//
// Both directions, because "draws nothing when unobserved" alone would pass on a
// page that had lost the figures altogether.
func TestTheDetailPageShowsNoRedirectFiguresForAModuleThatHasNone(t *testing.T) {
	const (
		killsCall        = "Killed at call"
		killsInstantiate = "Killed at instantiate"
		nothingToShow    = "There are no figures to show"
	)

	// A `none`-class module: no grant, so no class rows and no kills.
	quiet := renderPage(t, "addon_manager", map[string]any{
		"Row": map[string]any{
			"Name": "oidc", "Version": "1.2.1",
			"Declaration": "none", "Failure": "required",
			"Permissions": []string{"session.mint"},
			"Withheld":    map[string]bool{},
			"P99":         "—", "Kills": "—", "Observed": false,
			"Schema": "", "SchemaSize": "",
		},
		"Classes":          []map[string]any{},
		"KillsInstantiate": uint64(0), "KillsCall": uint64(0),
	})
	if !strings.Contains(quiet, nothingToShow) {
		t.Error("the page does not say why there are no figures")
	}
	for _, unwanted := range []string{killsCall, killsInstantiate} {
		if strings.Contains(quiet, unwanted) {
			t.Errorf("the page renders %q for a module that has never run on the redirect path", unwanted)
		}
	}

	// And the observed module still gets them, or the fix above would be a
	// deletion rather than a gate.
	loud := renderPage(t, "addon_manager", nil)
	for _, want := range []string{killsCall, killsInstantiate} {
		if !strings.Contains(loud, want) {
			t.Errorf("the page does not render %q for a module that has run", want)
		}
	}
	if strings.Contains(loud, nothingToShow) {
		t.Error("the page says there are no figures for a module that has figures")
	}
}

// **Both install shapes are on the manager, and the digest is beside the URL**
// (M68.6).
//
// The page is where the second shape becomes something an evaluator can find:
// the API had it the moment the field existed, and a capability reachable only by
// `curl` is one nobody opening this instance will see. What is asserted is the
// controls and their pairing, because the pairing is the safety property — a
// reader in a hurry who fills in a URL and not a digest has asked this instance to
// fetch and execute whatever an address answers with, and the two inputs sitting
// together in one form is what makes that hard to do by accident.
//
// The warning is asserted too, and it is the one sentence on this page no
// mechanism can replace: a URL and a digest copied from the same web page
// authenticate nothing.
func TestTheInstallControlOffersBothShapes(t *testing.T) {
	body := renderPage(t, "addons", map[string]any{"MaxUpload": "32 MiB"})

	for _, want := range []struct{ needle, why string }{
		{`name="manifest"`, "M67's upload keeps working unchanged"},
		{`name="module"`, "M67's upload keeps working unchanged"},
		{`name="url"`, "a module can arrive from a URL"},
		{`name="sha256"`, "the operator supplies the digest, never the URL"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("the install control has no %s control: %s", want.needle, want.why)
		}
	}

	// One form for the URL shape, holding both of its fields: the digest cannot be
	// submitted without the URL and the URL cannot be submitted without the digest,
	// which is what "beside" has to mean to be worth anything.
	url := strings.Index(body, `name="url"`)
	digest := strings.Index(body, `name="sha256"`)
	if url < 0 || digest < 0 || digest < url {
		t.Fatalf("the digest field is not after the URL field (%d, %d)", url, digest)
	}
	between := body[url:digest]
	if strings.Contains(between, "</form>") {
		t.Error("the URL and the digest are in different forms, so one can be sent " +
			"without the other — which is a request to fetch and execute whatever " +
			"an address answers with")
	}
	if !strings.Contains(body[digest:], "required") {
		t.Error("the digest field is not required")
	}

	if !strings.Contains(body, "copied from the same place prove nothing") {
		t.Error("the page does not say that a URL and a digest from the same page " +
			"authenticate nothing, which is the one risk here no mechanism removes")
	}
	// The upload's own bound is still stated, on the shape it bounds.
	if !strings.Contains(body, "32 MiB") {
		t.Error("the install control no longer states its size bound")
	}
}

// M69.5. The consent to an add-on's sign-in link is drawn as a toggle like any
// other, and it carries a sentence no other toggle does — because turning it on
// is the only setting on this page whose consequence is visible to somebody who
// has not signed in.
//
// Both directions, because "draws a warning" alone would pass on a page that put
// the same sentence under every checkbox.
func TestTheSignInConsentSaysWhatTurningItOnDoes(t *testing.T) {
	const warning = "sign-in page</strong>"

	consent := renderPage(t, "addon_manager", map[string]any{
		"Settings": []addonSettingStub{{
			Name: "sign_in_link", Kind: "toggle", Default: "false", SignIn: true,
		}},
	})
	if !strings.Contains(consent, `name="setting_sign_in_link"`) {
		t.Fatal("the consent is not rendered as a saveable setting at all")
	}
	if strings.Contains(consent, `name="setting_sign_in_link" value="true" checked`) {
		t.Error("the consent is ticked before an operator has agreed to anything")
	}
	if !strings.Contains(consent, warning) {
		t.Errorf("the consent does not say that turning it on changes what every "+
			"visitor sees:\n%s", consent)
	}

	ordinary := renderPage(t, "addon_manager", map[string]any{
		"Settings": []addonSettingStub{{Name: "pkce", Kind: "toggle", Default: "true"}},
	})
	if strings.Contains(ordinary, warning) {
		t.Error("an ordinary toggle carries the sign-in warning, so the sentence " +
			"says nothing about this particular setting")
	}
}
