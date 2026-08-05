package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every document that enumerates the never-delegable scopes names exactly the
// map's contents.
//
// This list has now drifted three times. It was written naming three slugs; M42
// added `webhooks.write` and M43 added `automation.write` without touching the
// prose; D98 added three more. By M45 the map held nine and `docs/SECURITY.md`
// still said three, `docs/usage.md` said seven and `api/openapi.yaml` said
// three — and one of those is the file a reader consults to decide what is safe
// to put on a key (F45, F61).
//
// The lists are maintained by hand because they are prose: each one explains
// *why* its members are non-delegable, and generating that from a map would
// produce a list nobody could read. So the fix is not to generate them, it is to
// make a hand-written list that has fallen behind fail — which is D97's argument
// applied to documentation rather than to a route table.
//
// # What this checks, and what it cannot
//
// It checks membership in both directions: every slug in the map appears in each
// document, and no document names a slug the map does not hold. It does **not**
// check that the sentence beside each one is still true, and it cannot — that is
// what a reader is for. A milestone adding a scope has to write its reason;
// this only stops it from forgetting to write anything at all.
func TestDocumentedNonDelegableScopesMatchTheMap(t *testing.T) {
	root := repoRoot(t)

	// Each file, and the region of it that carries the enumeration. Scoped
	// because these slugs appear all over these documents in other roles —
	// `audit.read` is discussed at length as a permission — and a whole-file
	// search would pass on a mention rather than on the list.
	// The window is asymmetric and tuned per document because these lists sit in
	// running prose: in usage.md the slugs precede the anchor and the reasons
	// follow it, in the other two both follow. A window wide enough to be
	// generous swallows the neighbouring paragraph — which this test proved on
	// its first run by reporting `destinations.review` from a sentence that
	// correctly says it **is** grantable, one paragraph down.
	for _, doc := range []struct {
		path   string
		anchor string
		before int
		after  int
	}{
		{"docs/SECURITY.md", "are not delegable to a key at all", 40, 900},
		{"docs/usage.md", "are never grantable to a key", 260, 400},
		{"api/openapi.yaml", "Nine are never grantable to a key:", 20, 520},
	} {
		t.Run(doc.path, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			text := string(body)
			// The anchor is normalized for line wrapping: these are prose files
			// and a slug list crosses lines wherever the paragraph happens to.
			flat := strings.Join(strings.Fields(text), " ")
			anchor := strings.Join(strings.Fields(doc.anchor), " ")
			i := strings.Index(flat, anchor)
			if i < 0 {
				t.Fatalf("%s no longer contains the enumeration this test is about "+
					"(looked for %q). If the list moved, move the anchor; if it was "+
					"deleted, a reader now has no list of what may not go on a key",
					doc.path, doc.anchor)
			}
			start := max(i-doc.before, 0)
			region := flat[start:min(i+doc.after, len(flat))]

			for scope := range NonDelegableScopes {
				if !strings.Contains(region, "`"+scope+"`") {
					t.Errorf("%s does not name %q in its never-grantable list. "+
						"A permission the map refuses and the document does not "+
						"mention is one somebody will try to put on a key, and be "+
						"refused by something they were told did not apply",
						doc.path, scope)
				}
			}
			// And the other direction, so a slug removed from the map does not
			// leave a document promising a refusal that no longer happens.
			for _, candidate := range allPermissionSlugsIn(region) {
				if _, held := NonDelegableScopes[candidate]; !held {
					t.Errorf("%s lists %q as never grantable and the map does not "+
						"hold it, so the document promises a refusal that will not "+
						"happen", doc.path, candidate)
				}
			}
		})
	}
}

// allPermissionSlugsIn pulls back-ticked `noun.verb` slugs out of a region.
//
// Deliberately narrow: only tokens with a dot and no spaces, which is what a
// permission slug looks like and what the prose around them is not.
func allPermissionSlugsIn(region string) []string {
	var out []string
	for part := range strings.SplitSeq(region, "`") {
		if !strings.Contains(part, ".") || strings.ContainsAny(part, " \t\n,;:") {
			continue
		}
		if strings.HasSuffix(part, ".") || strings.HasPrefix(part, ".") {
			continue
		}
		// `apikeys.*` is a glob the prose used before the list was written out;
		// it is not a slug and the map does not hold it.
		if strings.Contains(part, "*") {
			continue
		}
		out = append(out, part)
	}
	return out
}

// repoRoot walks up from this package to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root from the test's working directory")
	return ""
}
