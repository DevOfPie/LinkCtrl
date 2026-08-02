package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// No page data struct may redeclare a field the shell already provides.
//
// This is F20 made unrepeatable. Every dashboard page embeds shell, and the
// layout renders the chrome — the switcher, the bell, the theme — from the same
// dot the page renders its content from. Go's template resolution prefers the
// outer struct's field, so a page declaring `Workspaces` does not merely add
// one: it replaces the shell's for the whole template, including markup the
// page's author never opened. /workspaces and /members shipped doing exactly
// that, with a type the switcher's markup cannot read, and answered 500 on a
// plain GET for any organization with more than one workspace.
//
// Written against the source rather than against a list of types, because a
// list is a thing to forget. A page added tomorrow is covered by having been
// written, which is the only form of coverage that survives nobody remembering
// this happened. It is also the mechanism the milestone asked for instead of a
// comment: a comment asks the next author to remember, and the next author is
// exactly who will not.
//
// **Embedded fields count**, because under Go's spec an embedded field is a
// field declaration: `struct{ shell; *auth.Identity }` declares a field named
// Identity at depth 0, which beats the shell's at depth 1 and shadows it
// exactly as a named field would. The first version of this test read only
// named fields and therefore passed against three shapes that reproduce F20 —
// an embedded local type named `Workspaces`, an embedded `*auth.Identity`, and
// a shared mixin declaring `Path` alongside the shell, whose promoted field
// makes the selector ambiguous instead. That mixin is the ordinary refactor
// this package is one tidy-up away from, since a dozen page structs repeat
// Notice, FieldErrors and Path. Recorded as F39, closed under M28.
//
// One limit, stated rather than hidden: it reads fields, not methods — a method
// whose name matches a shell field would shadow it the same way, and nothing in
// the tree does that today. It reads this package only, which is not a limit
// but a fact about the subject: shell is unexported, so a struct that embeds it
// cannot be declared anywhere else.
func TestNoPageDataStructShadowsTheShell(t *testing.T) {
	structs := parsePackageStructs(t)

	shellFields, ok := structs["shell"]
	if !ok {
		t.Fatal("no `shell` struct in package httpx; this test has lost its subject")
	}
	if len(shellFields.named) == 0 {
		t.Fatal("`shell` declares no fields; this test would pass vacuously")
	}

	pages := 0
	for _, name := range sortedNames(structs) {
		if name == "shell" || !embedsShell(structs, name) {
			continue
		}
		pages++
		for _, field := range declaredNames(structs, name) {
			if _, clash := shellFields.named[field.name]; !clash {
				continue
			}
			t.Errorf("%s: %s declares %s, which shell already provides.\n"+
				"A page field shadows the shell's for the whole template, the layout's "+
				"chrome included — give it a name of its own (OrgWorkspaces, not Workspaces).",
				field.pos, name, field.name)
		}
	}
	// A refactor that renamed or moved shell would otherwise leave this test
	// green while checking nothing at all.
	if pages == 0 {
		t.Fatal("no struct in package httpx embeds shell; this test found nothing to check")
	}
}

// structFields is one struct declaration: what it names, what it embeds, and
// where each name was written.
//
// fieldOrder holds every field this struct *declares*, embedded ones included
// under the name Go gives them — the type's own identifier, with any pointer
// and package qualifier dropped. embedded holds only the bare local type names,
// because those are the only ones that can reach shell.
type structFields struct {
	named      map[string]struct{}
	fieldOrder []string
	embedded   []string
	pos        map[string]string
}

// declaredField is one name a struct puts at a template's disposal, and where.
type declaredField struct {
	name string
	pos  string
}

// declaredNames is every name a page struct resolves ahead of the shell's.
//
// Its own fields, embedded ones included; plus the fields promoted from
// anything it embeds that is *not* on the path to shell. That last exclusion is
// what keeps the shell's own promoted fields from being reported as clashing
// with themselves — a page reaching shell through an intermediate struct is the
// arrangement this test exists to allow, not one to flag. Everything else that
// gets embedded sits at the same depth as shell, so its fields do not shadow
// the shell's, they make the selector ambiguous, and a template resolving an
// ambiguous selector fails at render exactly the way F20 did.
func declaredNames(structs map[string]*structFields, name string) []declaredField {
	var out []declaredField
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		s, ok := structs[n]
		if !ok {
			return
		}
		for _, field := range s.fieldOrder {
			out = append(out, declaredField{name: field, pos: s.pos[field]})
		}
		for _, embedded := range s.embedded {
			// Skip shell and anything that reaches it: those are the fields the
			// page is meant to inherit, not ones competing with them. A struct
			// on that path is itself checked by the loop above, because it
			// embeds shell too.
			if embedded == "shell" || embedsShell(structs, embedded) {
				continue
			}
			walk(embedded)
		}
	}
	walk(name)
	return out
}

// parsePackageStructs reads every non-test file in this package and returns the
// struct types it declares, by name.
//
// os.ReadDir plus ParseFile rather than parser.ParseDir, which is deprecated
// along with ast.Package.
func parsePackageStructs(t *testing.T) map[string]*structFields {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	out := map[string]*structFields{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
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
	if len(out) == 0 {
		t.Fatal("no struct declarations found; the test is looking in the wrong directory")
	}
	return out
}

func readStruct(fset *token.FileSet, st *ast.StructType) *structFields {
	s := &structFields{named: map[string]struct{}{}, pos: map[string]string{}}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			// Embedded, and therefore *declared*: Go names the field after the
			// type, dropping any pointer and package qualifier, so
			// `*auth.Identity` declares Identity at depth 0 and beats the
			// shell's Identity at depth 1. Recording only the local bare names
			// as embedded — those are the only ones that can reach shell — but
			// recording every one of them as a field, which is what F39 found
			// missing.
			if name := embeddedName(field.Type); name != "" {
				s.named[name] = struct{}{}
				s.fieldOrder = append(s.fieldOrder, name)
				s.pos[name] = fset.Position(field.Type.Pos()).String()
			}
			if ident, ok := field.Type.(*ast.Ident); ok {
				s.embedded = append(s.embedded, ident.Name)
			}
			continue
		}
		for _, name := range field.Names {
			s.named[name.Name] = struct{}{}
			s.fieldOrder = append(s.fieldOrder, name.Name)
			s.pos[name.Name] = fset.Position(name.Pos()).String()
		}
	}
	return s
}

// embeddedName is the field name Go gives an embedded type: the type's own
// identifier, with a leading `*` and any package qualifier dropped. Empty for
// anything that cannot be embedded, which keeps this honest about what it
// understands rather than guessing at a name.
func embeddedName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr: // a generic instantiation, Type[Arg]
		return embeddedName(t.X)
	case *ast.IndexListExpr:
		return embeddedName(t.X)
	}
	return ""
}

// embedsShell reports whether a struct reaches shell by embedding, directly or
// through another struct in this package.
func embedsShell(structs map[string]*structFields, name string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if seen[n] {
			return false
		}
		seen[n] = true
		s, ok := structs[n]
		if !ok {
			return false
		}
		for _, embedded := range s.embedded {
			if embedded == "shell" || walk(embedded) {
				return true
			}
		}
		return false
	}
	return walk(name)
}

func sortedNames(structs map[string]*structFields) []string {
	out := make([]string, 0, len(structs))
	for name := range structs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
