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
// Two limits, stated rather than hidden. It reads fields, not methods — a
// method whose name matches a shell field would shadow it the same way, and
// nothing in the tree does that today. And it reads this package only, which is
// where every page data struct lives: shell is unexported, so a struct that
// embeds it cannot be declared anywhere else.
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
		s := structs[name]
		for _, field := range s.fieldOrder {
			if _, clash := shellFields.named[field]; !clash {
				continue
			}
			t.Errorf("%s: %s declares %s, which shell already provides.\n"+
				"A page field shadows the shell's for the whole template, the layout's "+
				"chrome included — give it a name of its own (OrgWorkspaces, not Workspaces).",
				s.pos[field], name, field)
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
type structFields struct {
	named      map[string]struct{}
	fieldOrder []string
	embedded   []string
	pos        map[string]string
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
			// Embedded. Only a bare local type name can reach shell; a qualified
			// or pointer type is from another package and cannot embed it.
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
