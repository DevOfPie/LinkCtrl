package httpx

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/DevOfPie/LinkCtrl/api"
)

// loadSpec parses and validates the embedded OpenAPI document.
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(api.SpecYAML())
	if err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI 3 document: %v", err)
	}
	return doc
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	doc := loadSpec(t)
	if doc.Info.Title == "" {
		t.Error("spec has no title")
	}
	if _, err := api.SpecJSON(); err != nil {
		t.Fatalf("the JSON form does not derive: %v", err)
	}
}

// routePattern matches API route registrations in router.go, e.g.
//
//	"GET " + APIPrefix + "/links/{id}"
//
// Scanning the source is blunt, but ServeMux does not expose its patterns and
// the alternative — maintaining a second route list for the test to read — is
// the drift this test exists to prevent.
var routePattern = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) " ?\+ ?APIPrefix ?\+ ?"([^"]+)"`)

// TestOpenAPICoversEveryRoute asserts the spec and the router describe the
// same API: every registered /api/v1 route appears in the document, and the
// document promises nothing the router does not serve.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}

	registered := map[string]bool{}
	for _, m := range routePattern.FindAllStringSubmatch(string(src), -1) {
		registered[m[1]+" "+m[2]] = true
	}
	if len(registered) < 10 {
		t.Fatalf("found only %d API routes in router.go; the scanning regex has rotted", len(registered))
	}

	documented := map[string]bool{}
	doc := loadSpec(t)
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	for r := range registered {
		if !documented[r] {
			t.Errorf("route %q is served but absent from openapi.yaml", r)
		}
	}
	for d := range documented {
		if !registered[d] {
			t.Errorf("openapi.yaml documents %q but the router does not serve it", d)
		}
	}
}

// TestOpenAPIOperationsAreComplete enforces the document's own hygiene: every
// operation has an id (tooling keys on them) and a tag, and every operationId
// is unique.
func TestOpenAPIOperationsAreComplete(t *testing.T) {
	doc := loadSpec(t)
	seen := map[string]string{}
	var ids []string

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			where := method + " " + path
			if op.OperationID == "" {
				t.Errorf("%s has no operationId", where)
				continue
			}
			if prev, dup := seen[op.OperationID]; dup {
				t.Errorf("operationId %q used by both %s and %s", op.OperationID, prev, where)
			}
			seen[op.OperationID] = where
			ids = append(ids, op.OperationID)
			if len(op.Tags) == 0 {
				t.Errorf("%s has no tag; it would render in Swagger UI's default bucket", where)
			}
			if op.Responses == nil || op.Responses.Len() == 0 {
				t.Errorf("%s documents no responses", where)
			}
		}
	}

	sort.Strings(ids)
	if len(ids) == 0 {
		t.Fatal("no operations found")
	}
}

// TestDocsCSPOnlyRelaxesStyles pins the shape of the /docs waiver: inline
// styles allowed there and only there, inline scripts allowed nowhere.
func TestDocsCSPOnlyRelaxesStyles(t *testing.T) {
	if !strings.Contains(docsCSP, "style-src 'self' 'unsafe-inline'") {
		t.Error("docs CSP no longer permits inline styles; Swagger UI renders unstyled without them")
	}
	if strings.Contains(docsCSP, "script-src 'self' 'unsafe") || strings.Contains(docsCSP, "unsafe-eval") {
		t.Error("docs CSP waives script restrictions; the style waiver must not creep")
	}
	if strings.Contains(csp, "unsafe") {
		t.Error("the app-wide CSP contains an unsafe- waiver")
	}
}
