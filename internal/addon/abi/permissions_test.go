package abi

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestThePermissionVocabularyIsExactlyThis is what makes the vocabulary *closed*
// rather than merely small.
//
// m62.md requires that adding an entry be "a code change with a test asserting
// the set, the same discipline `?src=` uses for its closed vocabulary". This is
// that test, and the literal below is the whole of it: a seventh permission, a
// renamed one, or one that quietly became grantable fails here and has to be
// argued for in the diff that changes it.
//
// Grantability is asserted beside the name because it is the half that decides
// behaviour. `redirect.inline` exists so M66 can enforce against an
// already-enforced permission, and until M66 lands **nothing may hold it** — that
// is the requirement, and flipping the flag without editing this line is exactly
// the accident it guards against.
func TestThePermissionVocabularyIsExactlyThis(t *testing.T) {
	want := []struct {
		name      string
		grantable bool
	}{
		{"config.read", true},
		{"storage.own_schema", true},
		{"routes.own_prefix", true},
		// The seventh, added at M64 (D258). It is here rather than folded into
		// routes.own_prefix because a manifest reader would not expect a page-serving
		// grant to also hand over who is signed in.
		{"session.context", true},
		{"session.mint", true},
		{"redirect.observe", true},
		{"redirect.inline", false},
	}

	if len(Permissions) != len(want) {
		t.Fatalf("the vocabulary has %d entries and this test names %d: %v",
			len(Permissions), len(want), PermissionNames())
	}
	for i, w := range want {
		got := Permissions[i]
		if got.Name != w.name {
			t.Errorf("Permissions[%d] is %q, want %q", i, got.Name, w.name)
			continue
		}
		if got.Grantable != w.grantable {
			t.Errorf("%s is grantable=%v, want %v — a change to this is a change to what "+
				"an add-on may hold, and it belongs in the diff rather than in a flag",
				got.Name, got.Grantable, w.grantable)
		}
	}
}

// The vocabulary is spelled the way this product's own permissions are, and every
// entry is documented, because both are consumed outside this repository: the
// spelling ends up in a manifest beside API-key scopes on the manager's page, and
// the documentation is generated into the published table a publisher reads before
// writing that manifest.
func TestEveryPermissionIsWellFormed(t *testing.T) {
	// This product's spelling. internal/addon's permissionRe is the manifest-side
	// twin and a test there holds the two together, which is the direction that
	// matters — a token this host publishes and a manifest may not declare would be
	// a vocabulary nobody can use.
	shape := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	milestone := regexp.MustCompile(`^M\d+(\.\d+)?$`)

	seen := map[string]bool{}
	for _, p := range Permissions {
		if !shape.MatchString(p.Name) {
			t.Errorf("%q is not spelled like a permission; expected dotted lowercase, "+
				"as in links.read", p.Name)
		}
		if seen[p.Name] {
			t.Errorf("%q is in the vocabulary twice", p.Name)
		}
		seen[p.Name] = true
		if !milestone.MatchString(p.BackedBy) {
			// The same field Function.BackedBy carries and for the same reason: it is
			// what lets the mid-phase review read this set against what was built.
			t.Errorf("%q does not name a milestone that backs it, got %q", p.Name, p.BackedBy)
		}
		if strings.TrimSpace(p.Doc) == "" {
			t.Errorf("%q has no documentation, and the published permission table is "+
				"generated from it", p.Name)
		}
	}
}

// Every permission a function costs is one the vocabulary carries, and every
// permission the vocabulary carries either gates a function or is a placement
// class named here literally.
//
// Both directions, because each catches a different mistake. A function requiring
// a token outside the vocabulary is a capability no manifest can ever declare —
// it would be refused for want of a grant that cannot be asked for. A vocabulary
// entry no function requires is a grant an operator can read on the manager's
// page and that buys the module nothing, which is a lie in a manifest unless it
// is the deliberate case.
func TestPermissionsAndFunctionsAgreeInBothDirections(t *testing.T) {
	required := map[string]bool{}
	for _, f := range Functions {
		if f.Requires == "" {
			continue
		}
		p, known := PermissionByName(f.Requires)
		if !known {
			t.Errorf("%s requires %q, which is not in the vocabulary, so no manifest can "+
				"declare it and every call is refused", f.Name, f.Requires)
			continue
		}
		if !p.Grantable {
			// The one shape that would make a function permanently unreachable while
			// looking like ordinary enforcement.
			t.Errorf("%s requires %q, which no host grants, so the function is refused for "+
				"every module however its manifest is written", f.Name, f.Requires)
		}
		required[f.Requires] = true
	}

	// The permissions that gate nothing, named rather than counted. `redirect.inline`
	// is a *placement* class — it decides where a module runs, not which function it
	// may call — so it gates no function by construction, and it is the only one.
	gatesNothing := []string{"redirect.inline"}
	for _, p := range Permissions {
		if required[p.Name] == slices.Contains(gatesNothing, p.Name) {
			t.Errorf("%q gates a function: %v, and this test says otherwise — either the "+
				"permission stopped being a placement class or a function acquired it",
				p.Name, required[p.Name])
		}
	}
}

// The ungated functions are exactly two, and they are named here so that a third
// one cannot arrive by somebody forgetting the field.
//
// m62.md's enforcement bullet says every host function checks the calling
// module's grants, and the vocabulary it enumerates has no entry for either of
// these — so the two facts only fit together if *ungated* is a deliberate answer
// rather than an omission. It is:
//
//   - abi_version reports a constant this host would publish to anybody; a module
//     that could not read it could not log what it is talking to.
//   - log is the capability granted on purpose. A module's stdout and stderr are
//     discarded precisely because routing them to an operator's log would be a
//     capability nobody asked for, and this function is the one that was asked
//     for. Requiring a declaration for it would put a line in every manifest and
//     buy nothing: a module refused the log still runs, and now silently.
func TestTheUngatedFunctionsAreNamed(t *testing.T) {
	ungated := []string{"abi_version", "log"}
	for _, f := range Functions {
		if f.Requires == "" && !slices.Contains(ungated, f.Name) {
			t.Errorf("%s requires no permission and is not one of the two functions this "+
				"test allows to be ungated (%v); either name the permission it costs or "+
				"argue for it here", f.Name, ungated)
		}
		if f.Requires != "" && slices.Contains(ungated, f.Name) {
			t.Errorf("%s is named ungated here and requires %q; the list is stale",
				f.Name, f.Requires)
		}
	}
}

func TestGrantableAndPermissionByNameAgreeWithTheSlice(t *testing.T) {
	for _, p := range Permissions {
		got, ok := PermissionByName(p.Name)
		if !ok || got.Name != p.Name {
			t.Errorf("PermissionByName(%q) did not find it", p.Name)
		}
		if Grantable(p.Name) != p.Grantable {
			t.Errorf("Grantable(%q) is %v and the entry says %v",
				p.Name, Grantable(p.Name), p.Grantable)
		}
	}
	if _, ok := PermissionByName("storage.own_schema.rows"); ok {
		t.Error("a token outside the vocabulary was found in it")
	}
	// A token nobody declared is not grantable, which is what stops an unknown
	// string being treated as a permission by a caller that skipped validation.
	if Grantable("links.read") {
		t.Error("links.read is one of this product's own permissions, not an add-on grant, " +
			"and Grantable said yes")
	}
	if got := PermissionNames(); len(got) != len(Permissions) {
		t.Errorf("PermissionNames returned %d of %d entries", len(got), len(Permissions))
	}
}

// The one permission with a constant, held to the slice.
//
// It exists because internal/addon's manifest validation branches on it by name,
// and a second spelling of a permission name is the drift a closed vocabulary is
// for. A constant that stopped matching an entry would make that validation
// silently stop firing.
func TestPermissionStorageNamesAnEntryInTheVocabulary(t *testing.T) {
	// The literal, spelled out. It is the one place in this repository a second
	// spelling belongs: PermissionStorage is used *as* the slice entry's Name, so a
	// lookup by the constant finds itself however the constant is misspelled — which
	// is what the first version of this test did, and it stayed green when the
	// constant was renamed.
	if PermissionStorage != "storage.own_schema" {
		t.Fatalf("PermissionStorage is %q, want storage.own_schema", PermissionStorage)
	}
	p, ok := PermissionByName(PermissionStorage)
	if !ok {
		t.Fatalf("PermissionStorage is %q, which the vocabulary does not carry: %v",
			PermissionStorage, PermissionNames())
	}
	if !p.Grantable {
		t.Errorf("%q is not grantable, and M63 implemented the functions it costs", p.Name)
	}
	if p.BackedBy != "M63" {
		t.Errorf("%q is backed by %q, want M63", p.Name, p.BackedBy)
	}
}
