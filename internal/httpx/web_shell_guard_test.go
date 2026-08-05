package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// The shell guard's own branches, exercised against structs that have the
// shapes it was written for.
//
// TestNoPageDataStructShadowsTheShell reads the real package, and every page
// struct in it embeds exactly `shell` and nothing else. So the branch F39 added
// — recording an embedded field as *declared* — and the walk through non-shell
// promotions both run against zero inputs: deleting them leaves the package
// green (F130). The guard is correct; nothing was holding it correct.
//
// This is F39's own finding one level up, which is why it gets synthetic inputs
// rather than a bigger scan. A guard that only ever sees compliant code has not
// been shown to reject anything, and the three shapes below are exactly the ones
// its comments claim to defend against.
func TestTheShellGuardCatchesEachShapeItClaimsTo(t *testing.T) {
	const shellSrc = `package p
type shell struct {
	Identity  string
	Workspace string
	Title     string
}
`

	for _, tc := range []struct {
		name string
		src  string
		// want is the declared name the guard must report for "page".
		want string
		why  string
	}{
		{
			name: "a named field shadowing the shell's",
			src: `package p
type page struct {
	shell
	Workspace string
}
`,
			want: "Workspace",
			why:  "the plain case, and the only one the real package can currently produce",
		},
		{
			name: "an embedded type whose name is a shell field",
			src: `package p
type Workspace struct{ ID string }
type page struct {
	shell
	Workspace
}
`,
			want: "Workspace",
			why: "Go names an embedded field after its type, so this declares Workspace at " +
				"depth 0 and beats the shell's at depth 1 — silently, because there is no " +
				"field name to read",
		},
		{
			name: "an embedded pointer to a qualified type",
			src: `package p
type page struct {
	shell
	*auth.Identity
}
`,
			want: "Identity",
			why: "the leading * and the package qualifier are dropped, which is what " +
				"embeddedName exists for and what F39 found unrecorded",
		},
		{
			name: "a field promoted through a struct that does not reach shell",
			src: `package p
type widget struct{ Title string }
type page struct {
	shell
	widget
}
`,
			want: "Title",
			why: "widget sits at the same depth as shell, so its Title does not shadow the " +
				"shell's — it makes the selector ambiguous, and a template resolving an " +
				"ambiguous selector fails at render exactly the way F20 did",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			structs := parseSyntheticStructs(t, shellSrc, tc.src)

			if !embedsShell(structs, "page") {
				t.Fatal("the fixture's page does not embed shell, so the guard would skip it")
			}
			var got []string
			for _, f := range declaredNames(structs, "page") {
				got = append(got, f.name)
			}
			if !slices.Contains(got, tc.want) {
				t.Errorf("declaredNames reported %v and not %q, so the guard would not "+
					"flag this — %s", got, tc.want, tc.why)
			}
			if _, clash := structs["shell"].named[tc.want]; !clash {
				t.Fatalf("the fixture's shell does not declare %q, so this case proves "+
					"nothing", tc.want)
			}
		})
	}

	// And the arrangement the guard must NOT flag: a page reaching shell through
	// an intermediate struct that embeds it. Inheriting the shell's fields is
	// what pages are for, and reporting them would make the guard unusable.
	t.Run("a page reaching shell through an intermediate", func(t *testing.T) {
		structs := parseSyntheticStructs(t, shellSrc, `package p
type middle struct {
	shell
}
type page struct {
	middle
}
`)
		for _, f := range declaredNames(structs, "page") {
			if _, clash := structs["shell"].named[f.name]; clash {
				t.Errorf("declaredNames reported %q, which the page inherits from shell "+
					"through middle rather than competing with — flagging it would make "+
					"the guard reject the arrangement it exists to allow", f.name)
			}
		}
	})
}

// parseSyntheticStructs builds the same map parsePackageStructs produces, from
// source written here instead of from the package on disk.
func parseSyntheticStructs(t *testing.T, sources ...string) map[string]*structFields {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*structFields{}
	for i, src := range sources {
		file, err := parser.ParseFile(fset, "synthetic.go", strings.NewReader(src),
			parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture %d: %v", i, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			out[spec.Name.Name] = readStruct(fset, st)
			return true
		})
	}
	return out
}
