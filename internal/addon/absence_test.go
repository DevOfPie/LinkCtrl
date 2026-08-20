package addon

import (
	"os"
	"path/filepath"
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

// No table. An add-on's schema is M63's, created for an add-on that exists rather
// than migrated into every instance in advance — which is what "DDL is additive"
// would otherwise be spent on for a feature nobody has enabled.
func TestNoMigrationMentionsAddOns(t *testing.T) {
	if hits := mentionsAddOns(t, "internal/store/migrations", ".sql"); len(hits) > 0 {
		t.Errorf("migrations mention add-ons: %v\n"+
			"M60 creates no table. If this is M63 or later, narrow this test "+
			"deliberately rather than deleting it", hits)
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

	// The redirect tree is the half of M60's absence that does not narrow. An
	// add-on's prefix is on the application tree only (m64.md), so no file serving
	// a short link may know add-ons exist — and every one of them is inside the
	// package the sweep above covers, which is why this is a second read of the
	// same directory rather than a claim about somewhere else.
	for _, hit := range hits {
		base := filepath.Base(hit)
		if strings.HasPrefix(base, "redirect") || strings.HasPrefix(base, "rootredirect") {
			t.Errorf("%s serves the redirect path and mentions add-ons; the prefix is "+
				"application-tree only", hit)
		}
	}

	spec, err := os.ReadFile(filepath.Join(repoRoot(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if lower := strings.ToLower(string(spec)); strings.Contains(lower, "addon") ||
		strings.Contains(lower, "add-on") {
		t.Error("api/openapi.yaml mentions add-ons; M64 deliberately adds no API " +
			"surface — whether third-party surfaces are bound by *every UI feature " +
			"has API support* is M69's question, with a real case in front of it")
	}
}
