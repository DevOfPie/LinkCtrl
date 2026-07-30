//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/api"
)

// contract replays real requests against the running fixture and validates
// each request and response against the embedded OpenAPI document. This is
// what keeps the spec honest: a field added to a response, a status code
// changed, an endpoint renamed — any of them fails here, not in a client.
type contract struct {
	t      *testing.T
	f      *apiFixture
	doc    *openapi3.T
	router routers.Router
	hit    map[string]bool
}

func newContract(t *testing.T) *contract {
	t.Helper()
	f := newAPI(t)

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(api.SpecYAML())
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("build spec router: %v", err)
	}
	return &contract{t: t, f: f, doc: doc, router: router, hit: map[string]bool{}}
}

// call performs a live request and validates both directions against the spec.
//
// checkRequest is false for calls that are deliberately malformed at the
// schema level — the point of those is the server's 422, and the spec-side
// request validator would fail first and prove nothing.
func (c *contract) call(method, path string, body any, wantStatus int, checkRequest bool) []byte {
	c.t.Helper()

	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}

	newReq := func() *http.Request {
		req, err := http.NewRequestWithContext(c.t.Context(), method,
			c.f.server.URL+path, bytes.NewReader(payload))
		if err != nil {
			c.t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req
	}

	resp, err := c.f.client.Do(newReq())
	if err != nil {
		c.t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s = %d, want %d\n%s", method, path, resp.StatusCode, wantStatus, respBody)
	}

	// Spec-side: the operation must exist, the request must satisfy it, and
	// the response must match what it promises for this status.
	vreq := newReq()
	route, pathParams, err := c.router.FindRoute(vreq)
	if err != nil {
		c.t.Fatalf("%s %s is not in openapi.yaml: %v", method, path, err)
	}
	opts := &openapi3filter.Options{
		IncludeResponseStatus: true,
		AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
	}
	rvi := &openapi3filter.RequestValidationInput{
		Request: vreq, PathParams: pathParams, Route: route, Options: opts,
	}
	if checkRequest {
		if err := openapi3filter.ValidateRequest(c.t.Context(), rvi); err != nil {
			c.t.Errorf("request %s %s violates the spec: %v", method, path, err)
		}
	}
	if err := openapi3filter.ValidateResponse(c.t.Context(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: rvi,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Body:                   io.NopCloser(bytes.NewReader(respBody)),
		Options:                opts,
	}); err != nil {
		c.t.Errorf("response %d from %s %s violates the spec: %v\n%s",
			resp.StatusCode, method, path, err, respBody)
	}

	c.hit[route.Operation.OperationID] = true
	return respBody
}

func (c *contract) do(method, path string, body any, wantStatus int) []byte {
	c.t.Helper()
	return c.call(method, path, body, wantStatus, true)
}

func (c *contract) doBadRequest(method, path string, body any, wantStatus int) []byte {
	c.t.Helper()
	return c.call(method, path, body, wantStatus, false)
}

func field(t *testing.T, raw []byte, name string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v", err)
	}
	v, _ := m[name].(string)
	if v == "" {
		t.Fatalf("response has no string field %q: %s", name, raw)
	}
	return v
}

// TestAPIMatchesItsContract walks every operation in the document against the
// live server, then fails if any operation was never exercised — a spec entry
// nobody can hit is documentation fiction.
func TestAPIMatchesItsContract(t *testing.T) {
	c := newContract(t)
	const p = "/api/v1"
	const password = "a-sufficiently-long-password"

	// --- auth ---------------------------------------------------------------
	c.do("POST", p+"/auth/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password,
	}, http.StatusCreated)
	// Claimed instances answer 404, documented as such.
	c.do("POST", p+"/auth/setup", map[string]string{
		"email": "late@example.com", "password": password,
	}, http.StatusNotFound)

	c.do("POST", p+"/auth/login", map[string]string{
		"email": "owner@example.com", "password": "wrong-but-long-enough",
	}, http.StatusUnauthorized)
	c.do("POST", p+"/auth/login", map[string]string{
		"email": "owner@example.com", "password": password,
	}, http.StatusOK)

	c.do("GET", p+"/me", nil, http.StatusOK)

	c.do("POST", p+"/auth/register", map[string]string{
		"email": "second@example.com", "password": password,
	}, http.StatusCreated)
	// The 422 for a malformed email — the response that was a 500 until the
	// contract work found the unmapped error.
	c.doBadRequest("POST", p+"/auth/register", map[string]string{
		"email": "not-an-address", "password": password,
	}, http.StatusUnprocessableEntity)
	c.doBadRequest("POST", p+"/auth/register", map[string]string{
		"email": "third@example.com", "password": "short",
	}, http.StatusUnprocessableEntity)

	// --- links --------------------------------------------------------------
	created := c.do("POST", p+"/links", map[string]any{
		"url": "https://example.com/contract", "alias": "contract",
		"title": "Contract", "tags": []string{"spec"},
	}, http.StatusCreated)
	linkID := field(t, created, "id")

	// Phase 2 fields refuse loudly, per the documented 422.
	c.do("POST", p+"/links", map[string]any{
		"url": "https://example.com/x", "one_time": true,
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/links?search=contract&include_total=true&sort=newest", nil, http.StatusOK)
	c.do("GET", p+"/links/"+linkID, nil, http.StatusOK)
	c.do("GET", p+"/links/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("PATCH", p+"/links/"+linkID, map[string]any{
		"title": "Contract v2", "tags": []string{"spec", "verified"},
	}, http.StatusOK)
	c.do("POST", p+"/links/"+linkID+"/archive", nil, http.StatusOK)
	c.do("POST", p+"/links/"+linkID+"/restore", nil, http.StatusOK)

	// --- analytics ----------------------------------------------------------
	c.do("GET", p+"/links/"+linkID+"/stats", nil, http.StatusOK)
	c.do("GET", p+"/links/"+linkID+"/stats?from=2026-07-01&to=2026-07-30", nil, http.StatusOK)
	c.do("GET", p+"/links/"+linkID+"/clicks", nil, http.StatusOK)
	c.do("GET", p+"/stats/overview", nil, http.StatusOK)
	// Inverted range: schema-valid dates, semantically refused.
	c.do("GET", p+"/stats/overview?from=2026-07-30&to=2026-07-01", nil, http.StatusUnprocessableEntity)

	// --- tags ---------------------------------------------------------------
	tags := c.do("GET", p+"/tags", nil, http.StatusOK)
	var tagList struct {
		Items []struct{ ID string } `json:"items"`
	}
	if err := json.Unmarshal(tags, &tagList); err != nil || len(tagList.Items) == 0 {
		t.Fatalf("expected tags from the created link: %v / %s", err, tags)
	}
	c.do("DELETE", p+"/tags/"+tagList.Items[0].ID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/tags/"+tagList.Items[0].ID, nil, http.StatusNotFound)

	// --- api keys -----------------------------------------------------------
	key := c.do("POST", p+"/api-keys", map[string]any{
		"name": "contract", "scopes": []string{"links.read"},
	}, http.StatusCreated)
	keyID := field(t, key, "id")
	c.do("POST", p+"/api-keys", map[string]any{
		"name": "escalation", "scopes": []string{"apikeys.write"},
	}, http.StatusUnprocessableEntity)
	c.do("GET", p+"/api-keys", nil, http.StatusOK)
	c.do("DELETE", p+"/api-keys/"+keyID, nil, http.StatusNoContent)

	// --- link deletion, password, logout ------------------------------------
	c.do("DELETE", p+"/links/"+linkID, nil, http.StatusNoContent)
	c.do("POST", p+"/auth/password", map[string]string{
		"current_password": password, "new_password": "a-brand-new-longer-password",
	}, http.StatusNoContent)
	c.do("POST", p+"/auth/logout", nil, http.StatusNoContent)
	c.do("GET", p+"/me", nil, http.StatusUnauthorized)

	// --- meta ---------------------------------------------------------------
	c.do("GET", p+"/openapi.json", nil, http.StatusOK)
	c.yamlSpecEndpoint()

	// --- completeness -------------------------------------------------------
	var missed []string
	for path, item := range c.doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !c.hit[op.OperationID] {
				missed = append(missed, fmt.Sprintf("%s (%s %s)", op.OperationID, method, path))
			}
		}
	}
	if len(missed) > 0 {
		t.Errorf("spec operations never exercised by this test:\n  %s",
			strings.Join(missed, "\n  "))
	}
}

// yamlSpecEndpoint checks the one non-JSON response by hand: kin-openapi has
// no YAML body decoder, so the generic validator cannot cover it.
func (c *contract) yamlSpecEndpoint() {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet,
		c.f.server.URL+"/api/v1/openapi.yaml", nil)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.f.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("openapi.yaml returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		c.t.Errorf("openapi.yaml Content-Type = %q", ct)
	}
	if !bytes.Equal(body, api.SpecYAML()) {
		c.t.Error("served YAML differs from the embedded document")
	}
	c.hit["getOpenAPIYAML"] = true
}
