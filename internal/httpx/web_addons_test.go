package httpx

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// The Add-on manager's own arithmetic and its refusal vocabulary (M68).
//
// What the page *does* — install through M67's service, remove a selection, purge
// a schema — is driven end to end in test/integration/addon_manager_test.go
// against a real host and a real database, because none of it means anything
// without one. What is here is the part that is decidable in this package: how a
// figure is written, what a refusal is allowed to say, and that the trailing
// column of the table is one template rather than two.

// The name an orphan purge is addressed under cannot collide with an add-on.
//
// `GET /api/v1/addons/{name}` and `GET /api/v1/addons/orphaned-data` are two
// patterns under one prefix, and this product does not resolve that kind of
// ambiguity by precedence — D263 refused to, twice, in this same subsystem. It
// does not have to here, and the reason is a property of the grammar rather than
// a list somebody maintains: an add-on's name admits no hyphen. Asserted against
// the validator itself, so a grammar that ever admitted one would fail here
// rather than silently shadow the endpoint.
func TestOrphanPathCannotBeAnAddonName(t *testing.T) {
	if store.ValidAddonName(AddonOrphanPath) {
		t.Errorf("%q is a legal add-on name, so an add-on could take the path the "+
			"orphan endpoints live on and shadow them", AddonOrphanPath)
	}
	if !strings.Contains(AddonOrphanPath, "-") {
		t.Errorf("%q no longer contains the character that makes it unclaimable; "+
			"the collision argument in api_addon_manager.go rests on it", AddonOrphanPath)
	}
}

// Every segment the manager mounts beside `{name}` is unclaimable by an add-on.
//
// The one above covers the orphan endpoints. This covers the other two, and they
// are the ones that were wrong: the manager's removal and purge forms posted to
// `/instance/addons/remove` and `/instance/addons/purge`, beside
// `GET /instance/addons/{name}`, and both segments match the add-on name grammar.
// Nothing collided only because the HTTP methods differed — which is precedence,
// the resolution api_addon_manager.go argues at length must not be relied on, and
// no test held either name.
//
// The templates are checked here too, because a form action is a literal a Go
// constant cannot reach: the reservation is worth nothing if the page posts
// somewhere else.
func TestTheManagersOwnPathsCannotBeAddonNames(t *testing.T) {
	for _, seg := range []string{AddonOrphanPath, AddonRemoveSegment, AddonPurgeSegment} {
		if store.ValidAddonName(seg) {
			t.Errorf("%q is a legal add-on name, so an add-on could claim the path the "+
				"manager mounts it on and the two would be resolved by method alone", seg)
		}
	}
	page := readRepoFile(t, "internal/ui/templates/pages/addons.html")
	detail := readRepoFile(t, "internal/ui/templates/pages/addon_manager.html")
	for _, want := range []string{
		AddonManagerPath + "/" + AddonRemoveSegment,
		AddonManagerPath + "/" + AddonPurgeSegment,
	} {
		if !strings.Contains(page, `action="`+want+`"`) {
			t.Errorf("pages/addons.html posts to no form at %s; the constant and the "+
				"template have drifted, and the page's buttons 404", want)
		}
	}
	if !strings.Contains(detail, `action="`+AddonManagerPath+"/"+AddonRemoveSegment+`"`) {
		t.Errorf("pages/addon_manager.html does not post its removal to %s/%s",
			AddonManagerPath, AddonRemoveSegment)
	}
}

// The literal in router.go is the constant, spelled twice on purpose.
//
// TestOpenAPICoversEveryRoute reads route patterns out of router.go's source with
// a regular expression, and a concatenated constant is invisible to it — so the
// patterns are written as literals there and this is what stops the two spellings
// drifting. The duplication is deliberate; an untied duplication would not be.
func TestOrphanPathIsSpelledOnceInTheRouter(t *testing.T) {
	src := readRepoFile(t, "internal/httpx/router.go")
	for _, want := range []string{
		`"/addons/` + AddonOrphanPath + `"`,
		`"/addons/` + AddonOrphanPath + `/{name}"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("router.go does not register %s; either the path moved and this "+
				"test is stale, or the constant and the literal have drifted", want)
		}
	}
}

// A latency is written the way an operator reads one, and never with more
// significant figures than an estimate off ten buckets supports.
func TestALatencyIsWrittenAtTheScaleItIsRead(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{451 * time.Microsecond, "451µs"},
		{1084500 * time.Nanosecond, "1.1ms"},
		{167 * time.Millisecond, "167.0ms"},
		{2500 * time.Millisecond, "2.5s"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A measured size is rounded; a configured bound is not.
//
// The two are different kinds of sentence and this product already had one
// function for the second. `byteSize` renders exact multiples only, because it
// words a bound an operator set — "32 MiB" — and a schema's size is never a round
// number, so rendering one through it produced "2465792 bytes".
func TestAMeasuredSizeIsRoundedAndABoundIsNot(t *testing.T) {
	if got := addonSize(2465792); got != "2.4 MiB" {
		t.Errorf("addonSize(2465792) = %q, want a rounded figure", got)
	}
	if got := addonSize(0); got != "0 B" {
		t.Errorf("addonSize(0) = %q", got)
	}
	if got := addonSize(512); got != "512 B" {
		t.Errorf("addonSize(512) = %q", got)
	}
	if got := byteSize(addon.MaxUploadBytes); got != "32 MiB" {
		t.Errorf("the upload bound reads %q; it has to match the documented figure "+
			"exactly, which is why it is a different function", got)
	}
}

// Every sentence this page shows after a redirect is a literal in this package.
//
// The manager renders attacker-influenced strings — an add-on's name, its version,
// every field of an uploaded manifest — and a refusal assembled out of a query
// string is the classic way one of those becomes an injection. So the redirect
// carries a *code*, the code is looked up in a closed set, and anything outside it
// gets the same generic sentence. This asserts the closure rather than the
// wording: a code nobody wrote must not reach the page.
func TestAFailureMessageIsNeverBuiltFromTheURL(t *testing.T) {
	hostile := "<script>alert(1)</script>"
	notice, failure := addonNotice(url.Values{"failed": {hostile}})
	if strings.Contains(failure, "script") || strings.Contains(failure, hostile) {
		t.Errorf("the refusal echoes the query string: %q", failure)
	}
	if failure != addonFailureMessage("anything-unrecognised") {
		t.Errorf("an unknown code produced %q rather than the generic refusal", failure)
	}
	// A refusal is never the green flash. Drawing one there reads as a success,
	// which on the page that deletes data is the worst place to be ambiguous.
	if notice != "" {
		t.Errorf("a refusal was returned as a success notice: %q", notice)
	}
	// And the counts are re-parsed rather than echoed, so "removed=<script>" is
	// not a sentence at all.
	if n, f := addonNotice(url.Values{"removed": {hostile}}); n != "" || f != "" {
		t.Errorf("a non-numeric count produced %q / %q", n, f)
	}
}

// The one refusal the install form has to word for itself reaches the page.
//
// Everything the install refuses is domain.ValidationErrors, so everything mapped
// to `invalid` and the page said *the manifest, the module or the digest did not
// check out* — which for an add-on shipping `.sql` files names a digest that is
// fine, names neither migrations nor the route on, and left docs/usage.md's claim
// that "the page says so" false. The API carried the message all along; the form
// carries a code, so the code is what had to survive the mapping.
func TestTheInstallFormWordsTheMigrationsRefusalItself(t *testing.T) {
	err := domain.ValidationErrors{{
		Field: "manifest", Code: addon.CodeMigrationsUnsupported,
		Message: "this add-on declares migration files",
	}}
	code := addonFailureCode(err)
	if code == "invalid" {
		t.Fatal("an add-on shipping migrations is refused as an ordinary invalid " +
			"upload, so the page tells the reader to check a digest that is correct")
	}
	msg := addonFailureMessage(code)
	for _, want := range []string{"migration", "LINKCTRL_ADDONS_DIR", "restart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, "digest") {
		t.Errorf("the refusal blames the digest: %q", msg)
	}
	// And nothing else widens: an ordinary validation failure still gets the
	// general sentence, because the specific one would be a lie about it.
	other := domain.ValidationErrors{{Field: "module", Code: "checksum_mismatch"}}
	if got := addonFailureCode(other); got != "invalid" {
		t.Errorf("a checksum mismatch mapped to %q", got)
	}
}

// The counts in the removal notice are arithmetic, and they say the right thing
// about what happened to the data.
//
// Three outcomes and they are genuinely different to an operator: everything
// purged, some purged, nothing purged. The last is the default — every purge box
// starts unticked — so it is the sentence most people will see, and it has to
// point at where the leftovers now are.
func TestTheRemovalNoticeSaysWhatHappenedToTheData(t *testing.T) {
	all, _ := addonNotice(url.Values{"removed": {"2"}, "purged": {"2"}})
	if !strings.Contains(all, "2 add-ons removed") || !strings.Contains(all, "deleted too") {
		t.Errorf("purging both said %q", all)
	}
	some, _ := addonNotice(url.Values{"removed": {"3"}, "purged": {"1"}})
	if !strings.Contains(some, "3 add-ons removed") || !strings.Contains(some, "orphaned data") {
		t.Errorf("purging one of three said %q", some)
	}
	none, _ := addonNotice(url.Values{"removed": {"1"}, "purged": {"0"}})
	if !strings.Contains(none, "1 add-on removed") || !strings.Contains(none, "orphaned data") {
		t.Errorf("purging none said %q", none)
	}
}

// A bulk removal that stops partway says how far it got.
//
// There is no transaction over a sequence of removals — each module is unloaded
// and its files taken out on its own — so a run of three that fails on the third
// has removed two, and they are gone. The page answered with a failure code alone,
// which put *nothing was changed* in front of a reader looking at a table two rows
// shorter than the one they pressed the button on.
//
// The counts travel on the same query string the successful path uses and are
// re-parsed as integers, so this asserts the sentence and not the mechanism: what
// must be true is that a reader is told, and told in the red flash rather than the
// green one — a run that stopped is a refusal however much of it landed.
func TestAStoppedRemovalSaysWhatItAlreadyDid(t *testing.T) {
	notice, failure := addonNotice(url.Values{
		"failed": {"unavailable"}, "removed": {"2"}, "purged": {"1"},
	})
	if notice != "" {
		t.Errorf("a stopped run drew a success notice: %q", notice)
	}
	for _, want := range []string{"2 add-ons removed", "1 of the schemas", "not touched"} {
		if !strings.Contains(failure, want) {
			t.Errorf("the refusal does not carry %q: %q", want, failure)
		}
	}
	// The reason is still there. Losing it to make room for the counts would trade
	// one missing half for the other.
	if !strings.Contains(failure, "add-ons directory") {
		t.Errorf("the refusal no longer says why it stopped: %q", failure)
	}
	// And nothing in the closed set claims the run changed nothing, which is the
	// sentence the counts exist to contradict.
	if strings.Contains(failure, "nothing was changed") {
		t.Errorf("the refusal says nothing was changed after two removals: %q", failure)
	}

	// A refusal with nothing behind it is unchanged — an install that fails has
	// removed no add-ons, and a count of zero there would be noise.
	_, alone := addonNotice(url.Values{"failed": {"unavailable"}})
	if strings.Contains(alone, "removed before") {
		t.Errorf("a refusal with nothing behind it carries a count: %q", alone)
	}
}

// A refused save comes back with what the reader typed, and without their secret.
//
// The 422 path re-reads the add-on from the host, which is by definition the
// *stored* state and therefore not what was just submitted — so a form refused for
// one bad field came back with every other field reverted, and the work went with
// it. Every other form in this product re-renders what arrived.
//
// The exception is the whole point of the exception: a `secret` is never rendered
// back, so a rejected save leaves the password box empty and the credential is
// retyped. And a setting the environment answers is not editable at all, so
// nothing the form carried for one may overwrite what the deployment says.
func TestARefusedSaveKeepsWhatWasTyped(t *testing.T) {
	views := []addon.SettingView{
		{Name: "endpoint", Type: addon.SettingText, Value: "https://stored.example"},
		{Name: "mode", Type: addon.SettingSelect, Options: []string{"fast", "slow"}, Value: "fast"},
		{Name: "client_secret", Type: addon.SettingSecret, Configured: true},
		{Name: "issuer", Type: addon.SettingText, Value: "",
			Source: addon.SourceEnvironment, EnvVar: "LINKCTRL_ADDON_X_ISSUER"},
	}
	keepSubmittedSettings(views, map[string]string{
		"endpoint": "https://typed.example", "mode": "slow",
		"client_secret": "typed-credential", "issuer": "https://not-yours.example",
	})
	if views[0].Value != "https://typed.example" {
		t.Errorf("the typed text value was thrown away: %q", views[0].Value)
	}
	if views[1].Value != "slow" {
		t.Errorf("the chosen option was thrown away: %q", views[1].Value)
	}
	if views[2].Value != "" {
		t.Errorf("a secret was rendered back into the form: %q", views[2].Value)
	}
	if views[3].Value != "" {
		t.Errorf("a setting the environment answers took the form's value: %q", views[3].Value)
	}
}

// The trailing column of the table is one template, in both states.
//
// m68.md: "same column template in both states, so the table does not shift". The
// browser harness measures the rendered column; this asserts the mechanism behind
// the measurement, because a matched pair of hard-coded widths would pass the
// measurement today and drift the first time either was edited. What it looks for
// is that `addon_row_end` is defined once and that the row invokes it once,
// passing the state — rather than branching in the row and rendering two cells.
func TestTheSelectColumnIsOneTemplateInBothStates(t *testing.T) {
	src := readRepoFile(t, "internal/ui/templates/pages/addons.html")
	if n := strings.Count(src, `{{define "addon_row_end"}}`); n != 1 {
		t.Fatalf("addon_row_end is defined %d times; the shared cell is what keeps the "+
			"two states the same width", n)
	}
	if n := strings.Count(src, `{{template "addon_row_end"`); n != 1 {
		t.Errorf("addon_row_end is invoked %d times; the row must render one cell "+
			"whichever state it is in", n)
	}
	// The `<td>` itself is inside the define and its geometry is written once.
	// Two `<td class="w-12` would be the pair of hard-coded widths this test
	// exists to refuse.
	if n := strings.Count(src, `<td class="w-12`); n != 1 {
		t.Errorf("the trailing cell's geometry is written %d times, want 1", n)
	}
}

// The confirmation's purge boxes are unticked, every one of them.
//
// The owner's amendment and m68.md's second risk: one dialog for many modules
// means several irreversible decisions taken in one breath, and a default-on box
// is exactly where a mis-tick lands. Asserted against the template because that is
// where a `checked` would be added.
func TestEveryPurgeBoxStartsUnticked(t *testing.T) {
	src := readRepoFile(t, "internal/ui/templates/pages/addons.html")
	purgeBox := regexp.MustCompile(`name="purge_\{\{\.Name}}"[^>]*`)
	m := purgeBox.FindString(src)
	if m == "" {
		t.Fatal("the confirmation renders no per-module purge box at all")
	}
	if strings.Contains(m, "checked") {
		t.Errorf("a purge box is ticked by default: %q", m)
	}
}

// readRepoFile reads a file by its path from the repository root.
//
// Two of the assertions above are about a template and about router.go's source
// rather than about a rendered response, and both are deliberate: what they check
// is a *mechanism* — one shared cell, one spelling of a path — which a rendering
// would agree with today whatever the mechanism underneath.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	// This package sits two levels down, which is a fact about the layout rather
	// than something to discover: internal/httpx.
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEveryFetchOutcomeHasAnOperatorsReading (M68.5).
//
// The vocabulary is closed and it is the guest's branch vocabulary, which means it
// is written for an add-on's author: `origin_refused` tells a module what to do
// about a refusal, and tells an operator nothing about which field to fill in. The
// manager's page is where the second reading lives, so a word without one renders
// a blank cell — and a tenth word added to the ABI would render eleven of them.
//
// Both directions, because either gap is a defect: a word with no sentence is a
// blank cell, and a sentence with no word is a row nobody will ever see, which is
// how a map like this comes to describe an outcome that was renamed.
func TestEveryFetchOutcomeHasAnOperatorsReading(t *testing.T) {
	for _, o := range abi.FetchOutcomes {
		if fetchOutcomeMeaning[o] == "" {
			t.Errorf("the ABI publishes the outcome %q and the manager's page has no "+
				"sentence for it, so an operator meeting it reads a blank cell", o)
		}
	}
	for o := range fetchOutcomeMeaning {
		if !slices.Contains(abi.FetchOutcomes, o) {
			t.Errorf("the page explains the outcome %q and the ABI does not publish it; "+
				"it is a row nobody can reach", o)
		}
	}
}

// --- M68.6: the second install shape ------------------------------------------

// **Every bound a URL install can hit words itself, and none of them collapses to
// *the upload was refused*.**
//
// The mechanism is the same one the migrations refusal uses — the field error's
// own code, never a substring of a message — and the reason there are now
// fourteen of them rather than one is that they need different things done about
// them. A refused address means the URL points somewhere private; a digest
// mismatch means the bundle is not the one you named; a 404 means the release page
// is wrong. One sentence covering all three is a sentence that helps with none.
//
// Both directions, for [TestEveryFetchOutcomeHasAnOperatorsReading]'s reason: a
// code with no sentence is a blank flash, and a sentence for a code the service
// cannot produce is one nobody will ever read.
func TestEveryUrlInstallRefusalWordsWhichBoundBit(t *testing.T) {
	generic := addonFailureMessage("anything-unrecognised")
	seen := map[string]bool{}
	for _, code := range addon.URLInstallCodes {
		err := domain.ValidationErrors{{Field: "url", Code: code, Message: "detail"}}
		got := addonFailureCode(err)
		if got != code {
			t.Errorf("the service refuses a URL install with %q and the page maps it to "+
				"%q, so the reader is told the upload was refused when nothing was "+
				"uploaded", code, got)
			continue
		}
		msg := addonFailureMessage(code)
		if msg == generic || msg == "" {
			t.Errorf("%q has no sentence of its own on the page, so an operator meeting "+
				"it reads %q and cannot act on it", code, msg)
			continue
		}
		if seen[msg] {
			t.Errorf("%q is worded with a sentence another code already uses; the codes "+
				"exist because the bounds need different things done about them", code)
		}
		seen[msg] = true
		// Every one of them says the same true thing about state, because the
		// question an operator has after a refused install is whether anything
		// landed. Nothing ever does: the digest is checked before the bundle is
		// parsed and the parse before anything is written.
		if !strings.Contains(msg, "Nothing was written") {
			t.Errorf("%q does not say that nothing was written: %q", code, msg)
		}
	}
	// And nothing here widens the migrations refusal or the generic one.
	if got := addonFailureCode(domain.ValidationErrors{{
		Field: "module", Code: "checksum_mismatch",
	}}); got != "invalid" {
		t.Errorf("an upload's checksum mismatch mapped to %q", got)
	}
}

// The refusals are still literals, and a URL install is where that matters most:
// the field error's message is assembled out of a bundle somebody else served.
func TestAUrlInstallRefusalNeverRendersWhatTheOriginSent(t *testing.T) {
	hostile := "<script>alert(1)</script>"
	err := domain.ValidationErrors{{
		Field: "url", Code: addon.CodeBundleInvalid,
		Message: "the bundle holds " + hostile,
	}}
	msg := addonFailureMessage(addonFailureCode(err))
	if strings.Contains(msg, "script") || strings.Contains(msg, hostile) {
		t.Errorf("the page rendered text that came out of a fetched archive: %q", msg)
	}
}
