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

// A permission in the vocabulary that no host grants yet is a different case and
// **loads**: the class is declarable on purpose, so refusing the module for
// naming it would make the declaration unusable and M66 would have nothing to
// turn on.
func TestADeclaredButUngrantablePermissionStillValidates(t *testing.T) {
	m := valid()
	m.Permissions = []string{"redirect.inline"}
	if err := m.Validate(); err != nil {
		t.Errorf("a manifest declaring redirect.inline was refused, and the class exists "+
			"to be declarable: %v", err)
	}
}

// --- what a module holds ----------------------------------------------------

// m62.md: "the redirect-inline grant exists and is refused for every module …
// nothing may hold it until M66 lands. The refusal is the test." This is it, and
// it is asserted against a real loaded add-on rather than against resolveGrants,
// because holding is a property of what the host registered and not of a function
// nobody has to call.
func TestNothingHoldsRedirectInline(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	// Everything, including the one that may not be held — so the assertion is
	// about redirect.inline rather than about a manifest that declared little.
	m.Permissions = append(grantable(), "redirect.inline")
	install(t, dir, m, code)

	metrics := observability.NewMetrics()
	sink := &logSink{}
	h, err := Open(t.Context(), Options{
		Dir:     dir,
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("an add-on declaring redirect.inline did not load, and the class is "+
			"declarable on purpose: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}

	grants := h.Addons()[0].Grants()
	if grants.Has("redirect.inline") {
		t.Error("an add-on holds redirect.inline, and no host grants it until an add-on " +
			"may run inside the redirect path")
	}
	// The rest of what it declared it does hold, which is what makes the line above
	// an assertion about one permission rather than about a resolution that failed.
	for _, p := range grantable() {
		if !grants.Has(p) {
			t.Errorf("the add-on declared %q and does not hold it", p)
		}
	}
	if got := grants.Len(); got != len(grantable()) {
		t.Errorf("the add-on holds %d grants and declared %d grantable ones",
			got, len(grantable()))
	}

	// Said out loud, because an operator who wrote that line in a manifest is
	// entitled to know it bought nothing.
	if logs := sink.String(); !strings.Contains(logs, "redirect.inline") ||
		!strings.Contains(logs, "no host grants yet") {
		t.Errorf("the load said nothing about the withheld permission\n%s", logs)
	}
	// And it is not on the identity series either, which is where an operator looks
	// for what an add-on holds rather than what it asked for.
	series := seriesLike(scrape(t, metrics), "linkctrl_addon_info")
	if strings.Contains(series, "redirect.inline") {
		t.Errorf("linkctrl_addon_info names a permission the add-on does not hold: %s", series)
	}
	if !strings.Contains(series, "redirect.observe") {
		t.Errorf("linkctrl_addon_info does not name the permissions the add-on holds: %s", series)
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
	g, withheld := resolveGrants(Manifest{Permissions: grantable()})
	if len(withheld) != 0 {
		t.Fatalf("resolving the grantable vocabulary withheld %v", withheld)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if !g.Has("redirect.observe") {
			t.Fatal("the grant is not held, so this measured the wrong branch")
		}
		if g.Has("redirect.inline") {
			t.Fatal("an ungranted permission was held")
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
