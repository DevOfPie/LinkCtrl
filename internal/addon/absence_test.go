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
// **These tests are meant to fail, at M64 and M63 respectively**, when an add-on
// first reaches the page and first owns a schema. That is not drift — it is the
// point. When it happens, the assertion is narrowed deliberately and in writing,
// exactly as scripts/single-instance-check.sh and demoCoverage() require of
// themselves, and never deleted.
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

// No route. M60 reads the directory at boot and that is the whole lifecycle; the
// manager is M68's and an add-on's own routes are M64's.
func TestNoHTTPSurfaceMentionsAddOns(t *testing.T) {
	var hits []string
	hits = append(hits, mentionsAddOns(t, "internal/httpx", ".go")...)
	hits = append(hits, mentionsAddOns(t, "internal/ui", ".go", ".html")...)
	if len(hits) > 0 {
		t.Errorf("the HTTP surface mentions add-ons: %v\n"+
			"M60 mounts no route and renders no page. If this is M64 or later, "+
			"narrow this test deliberately rather than deleting it", hits)
	}

	spec, err := os.ReadFile(filepath.Join(repoRoot(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if lower := strings.ToLower(string(spec)); strings.Contains(lower, "addon") ||
		strings.Contains(lower, "add-on") {
		t.Error("api/openapi.yaml mentions add-ons; M60 adds no API surface")
	}
}
