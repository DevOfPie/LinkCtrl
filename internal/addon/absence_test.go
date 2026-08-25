package addon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The other two absences m60.md claims — no route, and no table — are claims
// about the repository rather than about a running host, so they are checked over
// its files. Reading the tree from a unit test is unusual and is the same bargain
// internal/config/surface_test.go takes: the claim is about files, so only a check
// over those files can hold it.
//
// **These tests were meant to fail, at M64 and M63 respectively**, when an
// add-on first reached the page and first owned a schema. That is not drift — it
// was the point. Both have now been narrowed deliberately and in writing, exactly
// as scripts/single-instance-check.sh and demoCoverage() require of themselves,
// and neither was deleted: the route one became a bound on *which* files know
// about add-ons (below), and the migration one still holds, because an add-on's
// schema is created for an add-on that exists rather than migrated into every
// instance.
//
// What they buy in the meantime is the difference between "add-ons cost an
// instance nothing when unconfigured" as a sentence and as a fact. A route
// mounted unconditionally, or a migration that runs on every instance, is a cost
// paid by every operator who never installs an add-on — and both are the kind of
// thing that arrives in a diff nobody reads as a cost.

func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/addon -> repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate the repository root from %s: %v", root, err)
	}
	return root
}

// mentionsAddOns reports the files under rel whose contents name add-ons.
func mentionsAddOns(t *testing.T, rel string, exts ...string) []string {
	t.Helper()
	var hits []string
	root := filepath.Join(repoRoot(t), rel)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		var wanted bool
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				wanted = true
			}
		}
		if !wanted {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: a path this test walked in the repository it lives in
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(b))
		if strings.Contains(lower, "addon") || strings.Contains(lower, "add-on") {
			rel, _ := filepath.Rel(repoRoot(t), path)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", rel, err)
	}
	return hits
}

// migrationsMentioningAddOns is every migration allowed to know add-ons exist,
// and it is the narrowed form of M60's "no table" absence.
//
// **Narrowed at M65, deliberately and in writing, which is what the header above
// requires of this file.** The claim it held was that an add-on's *own* tables are
// created for an add-on that exists rather than migrated into every instance in
// advance, and that claim is unchanged and still worth having — a module's schema
// is M63's, made at load, and no migration in this directory creates one.
//
// What broke it is a table that is not an add-on's: `addon_identity_links` is
// LinkCtrl's own, in LinkCtrl's own schema, holding rows about LinkCtrl accounts.
// It has to be a migration for the reason every other table here is one — the
// product reads it whether or not any add-on is installed, and an assertion
// resolving against a table an add-on created would be an add-on deciding who it
// is allowed to be.
//
// So the absence becomes a bound: the *host's* tables about add-ons live in the
// files named here and nowhere else, and a second one arriving without being named
// is this test failing. That is the property left to keep — an instance with no
// add-ons pays for one small empty table, not for a schema per capability.
var migrationsMentioningAddOns = []string{
	// M65's linking table, and the account-deletion statement it joins is in
	// internal/store/query rather than here.
	"internal/store/migrations/04500_addon_identity_links.sql",
	// M65's provenance columns. **It creates nothing** — two nullable columns on
	// `mfa_pending_logins`, which is M53's table and is not about add-ons — so it
	// costs an instance with no add-ons two null columns on a table that lapses in
	// minutes. Named here rather than exempted by pattern, because *the migration
	// only adds columns* is exactly the sentence a migration that creates a table
	// would also be able to claim.
	"internal/store/migrations/04600_addon_mint_provenance.sql",
	// M67's permission. **It creates nothing either** — one row in `permissions`
	// and, on an instance that already has a principal, one row in
	// `instance_grants` — so what an instance with no add-ons pays for it is a
	// permission slug nobody holds but the account that administers the box.
	// Named here rather than exempted by pattern for the reason above: *the
	// migration only inserts a permission* is a sentence a migration creating an
	// add-on's tables could equally claim about its first statement.
	"internal/store/migrations/04700_addons_manage.sql",
	// M68's settings table, and it is the first migration here that creates one an
	// instance with no add-ons carries empty. What that costs is one table of three
	// text columns with no rows in it, and what it buys is the alternative refused:
	// the Add-on manager saves a declared setting somewhere, and the only other
	// candidate was the add-on's *own* schema — which the add-on's role can write,
	// so a `secret` kept there would be a credential the module could rewrite and
	// then read back as though an operator had chosen it. Host-side is the whole
	// point of the table, and the file says so at length.
	"internal/store/migrations/04800_addon_settings.sql",
}

func TestNoMigrationCreatesAnAddOnsOwnTables(t *testing.T) {
	hits := mentionsAddOns(t, "internal/store/migrations", ".sql")
	for _, hit := range hits {
		if !slices.Contains(migrationsMentioningAddOns, hit) {
			t.Errorf("%s mentions add-ons and is not one of the migrations allowed to: %v\n"+
				"An add-on's own schema is made at load, for an add-on that exists. A "+
				"migration about add-ons is the host's own table and is a decision — "+
				"name it here rather than deleting this test", hit, migrationsMentioningAddOns)
		}
	}
	for _, allowed := range migrationsMentioningAddOns {
		if !slices.Contains(hits, allowed) {
			t.Errorf("%s is listed here and no longer mentions add-ons; the list is stale", allowed)
		}
	}
}

// httpSurfaceMentioningAddOns is every file of the HTTP surface allowed to know
// add-ons exist, and it is the narrowed form of M60's "no route" absence.
//
// **Narrowed at M64, deliberately and in writing, which is what the header above
// requires of this file.** M60's claim was that no route was mounted and no page
// rendered, and M64 mounts one and renders one, so the absence is gone and what
// replaces it is a bound: the feature is allowed to exist in the files listed
// here and nowhere else. That is the property worth keeping — an add-on's page is
// four files, not a concept that leaks into the link tree, the analytics reader
// or a dozen templates — and it is the property the two claims below cannot
// state on their own.
//
// A file added here is a decision. A file that starts naming add-ons without
// being added is this test failing, which is the point.
var httpSurfaceMentioningAddOns = []string{
	// The handler, its tests, and the interface the router registers it through.
	"internal/httpx/addons.go",
	"internal/httpx/addons_test.go",
	"internal/httpx/router.go",
	"internal/httpx/router_test.go",
	"internal/httpx/web.go",
	// The page the host wraps an add-on's answer in, and its fixture.
	"internal/ui/templates/pages/addon.html",
	"internal/ui/ui_test.go",
	// The redirect handler and the tests that drive it (M66). **This is the
	// deliberate change the comment above asks for**, and it is the one the
	// milestone was for: an add-on may now run inside the redirect path, so the
	// file that serves it has to know add-ons exist. What is bounded is what it
	// knows — one interface with two methods, consulted at one named point — and
	// the second half of this test is what says the *prefix* still is not here.
	"internal/httpx/redirect.go",
	"internal/httpx/redirect_addon_test.go",
	// The lifecycle surface and its tests (M67). **The second deliberate change
	// this list has taken, and it is a larger one than M66's**: until now every
	// file here served an add-on's *own* pages, and this one changes which add-ons
	// exist. It is bounded the same way — two handlers, one multipart reader, one
	// interface the router registers them through — and the bound is what the list
	// is for: an add-on's lifecycle is allowed to exist in these files and not to
	// spread into the link tree or the settings pages on the way to M68's manager.
	"internal/httpx/api_addons.go",
	"internal/httpx/api_addons_test.go",
	// The Add-on manager (M68), which is the third and largest deliberate change
	// this list has taken — and the last one the phase plans. Until M67 the
	// question was whether the HTTP surface may *change* which add-ons exist; this
	// one is whether the dashboard may have a page about them, and the answer is
	// one page, one detail page, one nav entry and the files that draw them.
	//
	// The bound is still what the list is for, and it is worth stating what it
	// excludes now that it admits a dashboard page: nothing in the link tree, the
	// analytics reader, the account page or the workspace surfaces knows add-ons
	// exist, and the manager reaches the rest of the dashboard through exactly two
	// shared things — a badge helper beside `statusBadge`, and one entry in the
	// identity menu, both named below.
	"internal/httpx/api_addon_manager.go",
	"internal/httpx/web_addons.go",
	"internal/httpx/web_addons_test.go",
	"internal/ui/templates/pages/addons.html",
	"internal/ui/templates/pages/addon_manager.html",
	// The badge classes for the two class columns, beside statusBadge — the one
	// Go file Tailwind scans for utility strings, so a badge cannot be built
	// anywhere else without the stylesheet losing it.
	"internal/ui/funcs.go",
	// The identity menu's entry, and the script tag that puts a count in the
	// removal button. Both are one line in a file about the whole shell, which is
	// the smallest form this page's nav integration could take.
	"internal/ui/templates/partials/nav.html",
	"internal/ui/templates/layout.html",
	"internal/ui/header_test.go",
	// The manager's two pages asserted as *renders* rather than as templates,
	// which is one file and not a widening: what it reads is what the purge
	// confirmation says in both of its states, and that is checkable only against
	// a rendered page — a scan of the template passes on a sentence drawn inside a
	// branch that a zero count never enters.
	"internal/ui/addons_test.go",
}

// The HTTP surface knows about add-ons in the files M64 gave it and in no
// others, and the API surface still knows nothing at all.
func TestOnlyTheNamedHTTPFilesMentionAddOns(t *testing.T) {
	allowed := make(map[string]bool, len(httpSurfaceMentioningAddOns))
	for _, f := range httpSurfaceMentioningAddOns {
		allowed[filepath.FromSlash(f)] = true
	}

	var hits []string
	hits = append(hits, mentionsAddOns(t, "internal/httpx", ".go")...)
	hits = append(hits, mentionsAddOns(t, "internal/ui", ".go", ".html")...)

	seen := map[string]bool{}
	for _, hit := range hits {
		seen[hit] = true
		if !allowed[hit] {
			t.Errorf("%s mentions add-ons and is not in httpSurfaceMentioningAddOns; "+
				"an add-on's reach into the HTTP surface is bounded by that list, so "+
				"either this file should not know about add-ons or the list is what "+
				"needs the deliberate change", hit)
		}
	}
	// The other direction, which is what stops the list outliving the code: a
	// named file that no longer mentions add-ons is a bound describing nothing.
	for _, f := range httpSurfaceMentioningAddOns {
		if !seen[filepath.FromSlash(f)] {
			t.Errorf("%s is named as an add-on-aware file and does not mention add-ons; "+
				"the list has outlived the code", f)
		}
	}

	// **The half of M60's absence that narrowed at M66, and what is left of it.**
	//
	// It used to say no file serving a short link may know add-ons exist at all.
	// That stopped being the claim the moment an add-on could run inside the
	// redirect path, and the honest replacement is not "nothing here mentions
	// add-ons" — it is that what the redirect tree knows about them is the
	// *extension point* and never the *prefix*. `/addons/` is an application-tree
	// path (m64.md, D261) and a route under it on the link host would be a session
	// lookup and a template render on the tree whose whole rule is that it has
	// neither, which internal/httpx's own split-host test asserts from the other
	// side.
	//
	// So the sweep below is on the prefix rather than on the word, and it still
	// covers rootredirect.go, which has no extension point and must not acquire
	// one by copying.
	for _, hit := range hits {
		base := filepath.Base(hit)
		if !strings.HasPrefix(base, "redirect") && !strings.HasPrefix(base, "rootredirect") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repoRoot(t), hit))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), RoutePrefix) {
			t.Errorf("%s serves the redirect path and names %q; the prefix is "+
				"application-tree only", hit, RoutePrefix)
		}
	}

	// **M64's third claim, narrowed at M67 rather than deleted.**
	//
	// It read: the API surface knows nothing about add-ons at all. M64 added no
	// operations and the sentence was exactly true, and the question it left open
	// was whether a *third party's* surface — an add-on's own pages — is bound by
	// *every UI feature has API support*. That question is still open and is still
	// M69's.
	//
	// What M67 adds is not a third party's surface. It is the host's own lifecycle:
	// two operations that install and remove a module, under `addons.manage`, which
	// the inherited rule requires be in the document because M68's manager will
	// drive them. So the absence becomes a bound with the same shape as the two
	// above — the document may describe the operations named here, and an add-on's
	// own routes still have no API surface at all, which is what the second half
	// checks by asserting that `/addons/{addon}/` never appears as a path.
	//
	// **M68 adds three more of the host's own, and the bound is unchanged in kind.**
	// The manager reads what is installed, writes an add-on's declared settings and
	// drops an orphaned schema, all under the same scope, and the inherited rule
	// requires each in the document because a page drives it. What none of them is
	// is a path an add-on serves: every one of these is answered by this product's
	// code, about an add-on, and the allowed set below is exhaustive precisely so
	// that the day something answers *from* a module the list cannot absorb it
	// quietly.
	spec, err := os.ReadFile(filepath.Join(repoRoot(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(spec), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") || !strings.HasSuffix(trimmed, ":") {
			continue
		}
		path := strings.TrimSuffix(trimmed, ":")
		if !strings.HasPrefix(path, "/addons") {
			continue
		}
		if slices.Contains([]string{
			"/addons", "/addons/{name}", "/addons/{name}/settings",
			"/addons/orphaned-data", "/addons/orphaned-data/{name}",
		}, path) {
			continue
		}
		t.Errorf("api/openapi.yaml describes %q; the document carries the host's own "+
			"lifecycle operations (M67) and no path an add-on serves — whether a "+
			"third-party surface is bound by *every UI feature has API support* is "+
			"M69's question, with a real case in front of it", path)
	}
	if !strings.Contains(string(spec), "\n  /addons:") {
		t.Error("api/openapi.yaml does not describe POST /addons; M67's install and " +
			"remove are in the document because M68's manager drives them, and this " +
			"list has outlived the code")
	}
}
