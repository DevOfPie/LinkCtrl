package addon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// grantable is every permission an add-on may actually hold, which is what a
// manifest in these tests declares when it wants everything.
func grantable() []string {
	var out []string
	for _, p := range abi.Permissions {
		if p.Grantable {
			out = append(out, p.Name)
		}
	}
	return out
}

// originSetting is what a manifest declaring every grantable permission has to
// carry from M68.5 on: `network.fetch` and an origin-marked setting are refused
// apart, so a fixture that takes the whole vocabulary takes this too.
//
// Empty by default, which is the state that matters most — an add-on holding the
// grant and pointed at nothing reaches nothing — and a test that wants it pointed
// somewhere sets the value through Options.Settings.
func originSetting() Setting {
	return Setting{Name: "provider_origins", Type: SettingText, Origin: true}
}

// The two spellings of one rule, held together. The vocabulary is authored in
// internal/addon/abi and the manifest's shape check is permissionRe here, and a
// token the host publishes that a manifest may not declare would be a grant
// nobody can ask for — enforcement against a permission no add-on can hold looks
// exactly like enforcement working.
//
// The direction matters: this walks the vocabulary through the manifest's own
// regexp rather than re-stating the regexp in the abi package, because the
// manifest is where the token arrives from outside.
func TestEveryVocabularyTokenIsASpellingAManifestMayDeclare(t *testing.T) {
	for _, p := range abi.Permissions {
		if !permissionRe.MatchString(p.Name) {
			t.Errorf("the vocabulary carries %q and permissionRe refuses it, so no manifest "+
				"can declare it", p.Name)
		}
	}
	// And the other half: a manifest declaring the whole grantable vocabulary
	// validates, so the vocabulary is usable rather than merely well spelled.
	m := valid()
	m.Permissions = grantable()
	if err := m.Validate(); err != nil {
		t.Errorf("a manifest declaring every grantable permission was refused: %v", err)
	}
}

// A token outside the closed vocabulary refuses the add-on, and the refusal names
// what was allowed — the operator reading it is publishing or installing an
// add-on and the list is the fix.
//
// Refused rather than ignored, for the reason DisallowUnknownFields refuses an
// unknown manifest field: a declaration this host cannot interpret is a manifest
// whose author expects behaviour that will not happen, and there is no safe
// direction to guess in. The near-miss case is the one worth having a test for —
// `storage.own` reads like the real token and is not it.
func TestAPermissionOutsideTheVocabularyIsRefused(t *testing.T) {
	for _, token := range []string{
		"storage.own",             // a plausible near-miss of storage.own_schema
		"links.read",              // one of this product's own permissions, not an add-on grant
		"storage.own_schema.rows", // finer than the schema, which is deliberately not a thing
	} {
		m := valid()
		m.Permissions = []string{token}
		err := m.Validate()
		if err == nil {
			t.Errorf("%q was accepted and it is not in the vocabulary", token)
			continue
		}
		if !strings.Contains(err.Error(), token) {
			t.Errorf("the refusal of %q does not name it: %v", token, err)
		}
		for _, want := range abi.PermissionNames() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %q does not name %q, and the vocabulary is the fix: %v",
					token, want, err)
			}
		}
	}
}

// Validation asks whether a token is **in the vocabulary** and never whether this
// host grants it, and the difference is what made the declared-but-ungrantable
// pattern usable: a class defined ahead of the milestone that implements it has to
// be declarable, or the milestone has nothing to turn on and no add-on can be
// written against it in the meantime.
//
// It named `redirect.inline` until M66, which is the milestone that granted it.
// **There is no ungrantable token today**, so the test walks the vocabulary
// instead of naming one — which is what it should always have done: written
// against a literal, it would have gone quietly vacuous the moment that literal
// became grantable, which is exactly what happened.
func TestGrantabilityIsNotAValidationQuestion(t *testing.T) {
	for _, p := range abi.Permissions {
		m := valid()
		m.Permissions = []string{p.Name}
		if err := m.Validate(); err != nil {
			t.Errorf("a manifest declaring %q was refused and the token is in the "+
				"vocabulary; grantable=%v, and validation is not supposed to be asking: %v",
				p.Name, p.Grantable, err)
		}
	}
}

// --- what a module holds ----------------------------------------------------

// M62 shipped this test asserting that **nothing may hold `redirect.inline`**,
// and M66 is the milestone that makes that assertion false. So it is amended
// here, deliberately and by counter-edit, rather than deleted and rewritten under
// a new name — which is the tripwire-amendment discipline applied to the phase's
// own scaffolding, and m66.md puts it in scope in as many words.
//
// What it asserted: *"the redirect-inline grant exists and is refused for every
// module … nothing may hold it until M66 lands. The refusal is the test."* Read
// as written, that sentence contains its own expiry — *until M66 lands* — and
// what replaces it is the enforcement the refusal was standing in for: an add-on
// that declared the class holds it and is on the redirect path, an add-on that
// did not is not, and the boot log and the identity series both say which. The
// four milestones the grant spent declarable and ungrantable are what this
// milestone got for free: the class was already being enforced, so turning it on
// changed a flag rather than adding a check.
//
// It is asserted against a real loaded add-on rather than against resolveGrants,
// for the reason it always was: holding is a property of what the host registered
// and not of a function nobody has to call.
func TestDeclaringRedirectInlineIsWhatPutsAnAddonOnTheRedirectPath(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	m.Permissions = grantable()
	install(t, dir, m, code)

	// A second add-on that declared everything **except** the two redirect classes,
	// so the assertions below are about the declaration and not about a host that
	// puts whatever loaded onto the path.
	quiet := manifestFor("quiet", ClassRequired, code)
	for _, p := range grantable() {
		if p == PermissionRedirectInline || p == PermissionRedirectObserve {
			continue
		}
		quiet.Permissions = append(quiet.Permissions, p)
	}
	install(t, dir, quiet, code)

	metrics := observability.NewMetrics()
	sink := &logSink{}
	h, err := Open(t.Context(), Options{
		Dir:     dir,
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("an add-on declaring redirect.inline did not load: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	if h.Len() != 2 {
		t.Fatalf("loaded %d add-ons, want 2", h.Len())
	}

	grants := h.Addons()[0].Grants()
	if !grants.Has(PermissionRedirectInline) {
		t.Error("an add-on declared redirect.inline and does not hold it; M66 is the " +
			"milestone that turns the class on and this is what turning it on means")
	}
	for _, p := range grantable() {
		if !grants.Has(p) {
			t.Errorf("the add-on declared %q and does not hold it", p)
		}
	}
	if got := grants.Len(); got != len(grantable()) {
		t.Errorf("the add-on holds %d grants and declared %d grantable ones",
			got, len(grantable()))
	}

	// **Nothing is withheld any more**, and that is the other half of the
	// amendment: the withheld path exists for a class declared ahead of the
	// milestone that implements it, and there is no such class today. The log line
	// that used to say so must therefore be absent — an operator told a permission
	// bought them nothing, when it bought them the redirect path, is worse than
	// silence.
	if logs := sink.String(); strings.Contains(logs, "no host grants yet") {
		t.Errorf("the load withheld a permission and every entry in the vocabulary is "+
			"grantable\n%s", logs)
	}

	// Held, and therefore on the identity series, which is where an operator looks
	// for what an add-on holds rather than what it asked for.
	series := seriesLike(scrape(t, metrics), "linkctrl_addon_info")
	if !strings.Contains(series, PermissionRedirectInline) {
		t.Errorf("linkctrl_addon_info does not name a permission the add-on holds: %s", series)
	}

	// And the enforcement the refusal was standing in for: declaring the class is
	// what puts a module on the path, and the host resolved that once at load
	// rather than filtering per redirect.
	if !h.HasInline() {
		t.Error("an add-on holds redirect.inline and the host reports nothing on the " +
			"redirect path")
	}
	if got := h.InlineAddons(); len(got) != 1 || got[0] != "minimal" {
		t.Errorf("the redirect path carries %v, want only the add-on that declared the "+
			"class", got)
	}
	if got := h.ObservingAddons(); len(got) != 1 || got[0] != "minimal" {
		t.Errorf("the observe class carries %v, want only the add-on that declared it", got)
	}
}

// Grants are resolved once, at load, and this is the falsifiable form of it: the
// manifest the host was given is edited afterwards and what the add-on holds does
// not move.
//
// It matters because of where the check ends up. From M66 a grant check sits on
// the redirect path, where the inherited rule is a cached p99 under 20 ms, and a
// check that re-read the manifest — or walked the vocabulary — would be paying for
// that on every redirect. A test that only asserted the answer would pass either
// way.
func TestGrantsAreResolvedOnceAtLoad(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	m.Permissions = []string{"config.read"}
	install(t, dir, m, code)

	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded := h.Addons()[0]

	// The same backing array the host read, mutated the way a caller holding a
	// Manifest could: the resolved set must not follow it.
	loaded.Manifest.Permissions[0] = "session.mint"
	if !loaded.Grants().Has("config.read") {
		t.Error("editing the manifest took a grant away, so the check reads the manifest " +
			"rather than a set resolved at load")
	}
	if loaded.Grants().Has("session.mint") {
		t.Error("editing the manifest granted a permission, so the check reads the manifest " +
			"rather than a set resolved at load")
	}
	// The host's own copy answers the same way, which is the one that matters: it is
	// what dispatch consults.
	if st := h.hostState("minimal"); st == nil || !st.grants.Has("config.read") ||
		st.grants.Has("session.mint") {
		t.Error("the host's own grant set followed the manifest")
	}
}

// The check is a lookup and allocates nothing, which is the shape m62.md's second
// risk requires and the closest a test can come to asserting "not I/O": a
// resolution that read a file, built a slice or walked the vocabulary would show
// up here.
func TestAGrantCheckAllocatesNothing(t *testing.T) {
	// Everything except one, so both branches below are measured against a real
	// declaration rather than against a token outside the vocabulary — which is a
	// miss the manifest parser would have refused long before a grant check saw it.
	//
	// It used to withhold `redirect.inline`, which was the ungrantable class until
	// M66 turned it on. Nothing is ungrantable now, so the miss has to be
	// constructed from something simply not declared, which is the ordinary shape
	// anyway.
	var declared []string
	for _, p := range grantable() {
		if p != PermissionRewriteQuery {
			declared = append(declared, p)
		}
	}
	g, withheld := resolveGrants(Manifest{Permissions: declared})
	if len(withheld) != 0 {
		t.Fatalf("resolving the grantable vocabulary withheld %v", withheld)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if !g.Has(PermissionRedirectObserve) {
			t.Fatal("the grant is not held, so this measured the wrong branch")
		}
		if g.Has(PermissionRewriteQuery) {
			t.Fatal("an undeclared permission was held")
		}
	}); allocs != 0 {
		t.Errorf("a grant check allocated %v times per run; from M66 it sits on the "+
			"redirect path and has to be a lookup on a resolved set", allocs)
	}
}

// The zero Grants is a valid empty set, because that is what an add-on that
// declared nothing holds and nothing should have to construct one to say so.
func TestTheZeroGrantsHoldsNothing(t *testing.T) {
	var g Grants
	if g.Has("config.read") || g.Len() != 0 || g.String() != "" || len(g.Names()) != 0 {
		t.Errorf("the zero Grants is not empty: len=%d names=%v string=%q",
			g.Len(), g.Names(), g.String())
	}
}

// The label and the log line are sorted, so two boots of one instance produce the
// same series identity and an operator diffing them sees only real changes. A
// manifest is hand-written JSON and the order in it is whatever the author typed.
func TestGrantNamesAreSorted(t *testing.T) {
	g, _ := resolveGrants(Manifest{Permissions: []string{
		"session.mint", "config.read", "redirect.observe",
	}})
	if got := g.String(); got != "config.read,redirect.observe,session.mint" {
		t.Errorf("the label form is %q and is not sorted", got)
	}
}
