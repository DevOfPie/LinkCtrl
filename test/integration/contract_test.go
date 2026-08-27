//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/api"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
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
	// bearer, while set, sends the call as an API key instead of as the signed-in
	// session. Set only by doAsKey, and restored after it.
	bearer string
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
		if c.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		return req
	}

	// A bearer token replaces the session, on a client with no cookie jar, so a
	// call made as an API key is provably made as one. Only rotation needs it —
	// it is the one operation a session cannot reach at all.
	client := c.f.client
	if c.bearer != "" {
		client = &http.Client{}
	}

	resp, err := client.Do(newReq())
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

// doAsKey replays an operation as an API key rather than as the session.
func (c *contract) doAsKey(token, method, path string, body any, wantStatus int) []byte {
	c.t.Helper()
	c.bearer = token
	defer func() { c.bearer = "" }()
	return c.call(method, path, body, wantStatus, true)
}

func (c *contract) doBadRequest(method, path string, body any, wantStatus int) []byte {
	c.t.Helper()
	return c.call(method, path, body, wantStatus, false)
}

// upload replays a multipart operation (M50.5).
//
// **The contract test had no multipart shape before this milestone**, and
// teaching it one was named as part of M50.5 rather than discovered inside it.
// Everything above sends `application/json`: `call` marshals whatever it is
// given and sets one header. A file is a different body entirely — a boundary,
// per-part headers, and a schema whose property is `format: binary` — so it
// needs its own builder rather than a flag on that one.
//
// What it keeps is everything that makes `call` worth having: the request is
// validated against the document before it is trusted, the response is validated
// against what the document promises for that status, and the operation is
// recorded as exercised. The body is rebuilt for the validation pass because the
// first send consumed it.
//
// `filename` and `declared` are deliberately parameters. The server reads
// neither — that is the milestone's claim, asserted in internal/httpx by source
// scan and in qr_test.go by behaviour — so this replay sends values that
// disagree with the content, and the operation is expected to succeed anyway.
func (c *contract) upload(
	method, path, field, filename, declared string, body []byte,
	wantStatus int, checkRequest bool,
) []byte {
	c.t.Helper()

	build := func() (string, []byte) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {
				`form-data; name="` + field + `"; filename="` + filename + `"`,
			},
			"Content-Type": {declared},
		})
		if err != nil {
			c.t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			c.t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			c.t.Fatal(err)
		}
		return w.FormDataContentType(), buf.Bytes()
	}

	newReq := func() *http.Request {
		contentType, payload := build()
		req, err := http.NewRequestWithContext(c.t.Context(), method,
			c.f.server.URL+path, bytes.NewReader(payload))
		if err != nil {
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		if c.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		return req
	}

	// The same substitution `call` makes: a bearer replaces the session on a
	// client with no cookie jar, so a call made as an API key is provably made
	// as one.
	client := c.f.client
	if c.bearer != "" {
		client = &http.Client{}
	}

	resp, err := client.Do(newReq())
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

// uploadParts replays a multipart operation carrying more than one file part
// (M67).
//
// [contract.upload] above sends one part and is written around that: it takes a
// field, a filename and a declared type, because M50.5's claim is that the server
// reads none of them. An install carries two files and no text fields, so what
// this needs is a map and nothing else — and keeping it separate leaves that
// milestone's assertion about a single part exactly where it was.
func (c *contract) uploadParts(
	method, path string, parts map[string][]byte, wantStatus int, checkRequest bool,
) []byte {
	c.t.Helper()

	build := func() (string, []byte) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		// Sorted, so a body this test sends is the same body every run. Map order
		// is not a thing to make a request out of.
		names := make([]string, 0, len(parts))
		for name := range parts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			part, err := w.CreateFormFile(name, name+".bin")
			if err != nil {
				c.t.Fatal(err)
			}
			if _, err := part.Write(parts[name]); err != nil {
				c.t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			c.t.Fatal(err)
		}
		return w.FormDataContentType(), buf.Bytes()
	}

	newReq := func() *http.Request {
		contentType, payload := build()
		req, err := http.NewRequestWithContext(c.t.Context(), method,
			c.f.server.URL+path, bytes.NewReader(payload))
		if err != nil {
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		if c.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		return req
	}

	client := c.f.client
	if c.bearer != "" {
		client = &http.Client{}
	}

	resp, err := client.Do(newReq())
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

// uploadAsKey replays an upload as an API key rather than as the session.
func (c *contract) uploadAsKey(
	token, method, path, field, filename, declared string, body []byte, wantStatus int,
) []byte {
	c.t.Helper()
	c.bearer = token
	defer func() { c.bearer = "" }()
	return c.upload(method, path, field, filename, declared, body, wantStatus, true)
}

// contractLogo is a real PNG built here rather than committed as a fixture: a
// binary file in the tree is one nobody reviews, and this one has to be small
// enough to be obviously inert.
func contractLogo(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 8), G: 0x30, B: uint8(y * 8), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// contractOversizedLogo is inside the side bound and past the storage target, so
// the server stores a smaller copy of it and says so.
func contractOversizedLogo(t *testing.T) []byte {
	t.Helper()
	const side = 800
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: 0x30, B: uint8(y), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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

	// 202, not 201. Nothing exists yet — the account is created when the
	// emailed link is followed, which is what verifying the address before the
	// account is usable means (D1). The fixture's LINKCTRL_SIGNUP_MODE is
	// `open` and it configures a mailer, which is the only combination in which
	// this answers anything but 403.
	c.do("POST", p+"/auth/register", map[string]string{
		"email": "second@example.com", "password": password,
	}, http.StatusAccepted)
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

	// The gates are settable (M35). This replayed the documented 422 while they
	// were stubs; the spec now documents them as accepted, and a contract test
	// still asserting the refusal would be asserting the stub.
	gated := c.do("POST", p+"/links", map[string]any{
		"url": "https://example.com/x", "one_time": true, "max_clicks": 3,
		"require_signature": true,
	}, http.StatusCreated)
	c.do("POST", p+"/links/"+field(t, gated, "id")+"/sign",
		map[string]any{"ttl_seconds": 3600}, http.StatusCreated)

	c.do("GET", p+"/links?search=contract&include_total=true&sort=newest", nil, http.StatusOK)
	c.do("GET", p+"/links/"+linkID, nil, http.StatusOK)
	c.do("GET", p+"/links/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("PATCH", p+"/links/"+linkID, map[string]any{
		"title": "Contract v2", "tags": []string{"spec", "verified"},
	}, http.StatusOK)
	c.do("POST", p+"/links/"+linkID+"/archive", nil, http.StatusOK)
	c.do("POST", p+"/links/"+linkID+"/restore", nil, http.StatusOK)

	// --- routing rules (M34) ------------------------------------------------
	//
	// Every response here is validated against the spec's schemas by c.do, which
	// is what makes the RuleConditions schema more than documentation: a
	// condition the server emits under a name the document does not carry fails
	// the request rather than the reader.
	rule := c.do("POST", p+"/links/"+linkID+"/rules", map[string]any{
		"url":      "https://example.com/contract-gb",
		"priority": 50,
		"conditions": map[string]any{
			"country": []string{"GB"},
			"device":  []string{"mobile"},
			"utm":     map[string][]string{"source": {"newsletter"}},
			"time": map[string]any{
				"days": []string{"mon", "tue"}, "from": "09:00", "to": "17:00",
				"tz": "Europe/London",
			},
			"returning": true,
		},
	}, http.StatusCreated)
	ruleID := field(t, rule, "id")

	// The refusal that is a product decision rather than a typo, at the surface
	// a client actually meets it.
	//
	// doBadRequest, because the *request* is deliberately outside the schema —
	// RuleConditions declares `additionalProperties: false` and lists twelve
	// names, so a cookies condition is refused by the document as well as by the
	// server. That double refusal is the point rather than an obstacle: the spec
	// says these are the conditions, and the server says why this one is not
	// among them.
	c.doBadRequest("POST", p+"/links/"+linkID+"/rules", map[string]any{
		"url":        "https://example.com/x",
		"conditions": map[string]any{"cookies": []string{"session"}},
	}, http.StatusUnprocessableEntity)
	// A rule matching everybody would short-circuit every rule beneath it.
	c.do("POST", p+"/links/"+linkID+"/rules", map[string]any{
		"url": "https://example.com/x", "conditions": map[string]any{},
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/links/"+linkID+"/rules", nil, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID+"/rules/"+ruleID, map[string]any{
		"enabled": false, "priority": 10,
	}, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID+"/rules/"+uuid.NewString(), map[string]any{
		"enabled": false,
	}, http.StatusNotFound)
	c.do("DELETE", p+"/links/"+linkID+"/rules/"+ruleID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/links/"+linkID+"/rules/"+ruleID, nil, http.StatusNotFound)

	// --- split testing (M36) ------------------------------------------------
	//
	// The arms are created, listed, edited and removed against the same
	// schema-validating client, which is what makes `share` more than
	// documentation: it is computed rather than stored, so a change to how the
	// denominator is chosen shows up here as a response the document refuses.
	arm := c.do("POST", p+"/links/"+linkID+"/split", map[string]any{
		"kind": "weighted", "url": "https://example.com/contract-a", "weight": 60,
	}, http.StatusCreated)
	armID := field(t, arm, "id")
	c.do("POST", p+"/links/"+linkID+"/split", map[string]any{
		"kind": "weighted", "url": "https://example.com/contract-b", "weight": 40,
	}, http.StatusCreated)
	// A link's arms are all one kind, so this is refused rather than stored.
	c.do("POST", p+"/links/"+linkID+"/split", map[string]any{
		"kind": "sequential", "url": "https://example.com/contract-c",
	}, http.StatusUnprocessableEntity)
	// A fallback is a third thing and sits beside them.
	c.do("POST", p+"/links/"+linkID+"/split", map[string]any{
		"kind": "fallback", "url": "https://example.com/contract-catch",
	}, http.StatusCreated)

	c.do("GET", p+"/links/"+linkID+"/split", nil, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID+"/split/"+armID, map[string]any{
		"enabled": false, "weight": 10,
	}, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID+"/split/"+uuid.NewString(), map[string]any{
		"enabled": false,
	}, http.StatusNotFound)
	c.do("DELETE", p+"/links/"+linkID+"/split/"+armID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/links/"+linkID+"/split/"+armID, nil, http.StatusNotFound)

	// --- folders (M38) ------------------------------------------------------
	//
	// The tree, the two refusals the document names by code, and the filing of a
	// link into a folder in both directions. The cycle refusal is replayed here
	// rather than only in folders_test.go because the *document* claims it: an
	// implementation that quietly allowed it would leave openapi.yaml describing
	// a rule nothing enforces.
	parentFolder := c.do("POST", p+"/folders", map[string]any{
		"name": "Campaigns",
	}, http.StatusCreated)
	parentFolderID := field(t, parentFolder, "id")
	childFolder := c.do("POST", p+"/folders", map[string]any{
		"name": "Summer", "parent_id": parentFolderID,
	}, http.StatusCreated)
	childFolderID := field(t, childFolder, "id")
	// Siblings may not share a name, case-insensitively.
	c.do("POST", p+"/folders", map[string]any{
		"name": "campaigns",
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/folders", nil, http.StatusOK)
	c.do("PATCH", p+"/folders/"+childFolderID, map[string]any{
		"name": "Summer sale",
	}, http.StatusOK)
	c.do("PATCH", p+"/folders/"+uuid.NewString(), map[string]any{
		"name": "Nowhere",
	}, http.StatusNotFound)

	// A folder can never become its own descendant.
	c.do("POST", p+"/folders/"+parentFolderID+"/move", map[string]any{
		"parent_id": childFolderID,
	}, http.StatusUnprocessableEntity)
	// Moving out to the top level: the one destination `parent_id: null` names.
	c.do("POST", p+"/folders/"+childFolderID+"/move", map[string]any{
		"parent_id": nil,
	}, http.StatusOK)

	// Filing a link, and taking it out again with the empty-string sentinel.
	c.do("PATCH", p+"/links/"+linkID, map[string]any{
		"folder_id": parentFolderID,
	}, http.StatusOK)
	c.do("GET", p+"/links?folder="+parentFolderID, nil, http.StatusOK)
	c.do("GET", p+"/links?folder=none", nil, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID, map[string]any{"folder_id": ""}, http.StatusOK)

	c.do("DELETE", p+"/folders/"+childFolderID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/folders/"+childFolderID, nil, http.StatusNotFound)
	c.do("DELETE", p+"/folders/"+parentFolderID, nil, http.StatusNoContent)

	// --- campaigns (M41) ----------------------------------------------------
	//
	// The lifecycle, the slug rule the document names, and the labelling of a
	// link in both directions. The slug conflict is replayed here rather than
	// only in campaigns_test.go because the *document* claims it.
	campaign := c.do("POST", p+"/campaigns", map[string]any{
		"name": "Summer 2026", "description": "The June push",
	}, http.StatusCreated)
	campaignID := field(t, campaign, "id")
	// Two campaigns in a workspace may not share a slug, case-insensitively.
	c.do("POST", p+"/campaigns", map[string]any{
		"name": "Another", "slug": "SUMMER-2026",
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/campaigns", nil, http.StatusOK)
	c.do("GET", p+"/campaigns/"+campaignID, nil, http.StatusOK)
	c.do("GET", p+"/campaigns/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("PATCH", p+"/campaigns/"+campaignID, map[string]any{
		"name": "Summer 2026 (paid)", "clear_ends_at": true,
	}, http.StatusOK)
	c.do("PATCH", p+"/campaigns/"+uuid.NewString(), map[string]any{
		"name": "Nowhere",
	}, http.StatusNotFound)

	// Labelling a link, and taking it out again with the empty-string sentinel
	// `folder_id` above uses.
	c.do("PATCH", p+"/links/"+linkID, map[string]any{
		"campaign_id": campaignID,
	}, http.StatusOK)
	c.do("GET", p+"/links?campaign="+campaignID, nil, http.StatusOK)
	c.do("GET", p+"/links?campaign=none", nil, http.StatusOK)
	c.do("PATCH", p+"/links/"+linkID, map[string]any{"campaign_id": ""}, http.StatusOK)

	c.do("DELETE", p+"/campaigns/"+campaignID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/campaigns/"+campaignID, nil, http.StatusNotFound)

	// --- webhooks (M42) -----------------------------------------------------
	//
	// The whole lifecycle, because every operation on this collection is one the
	// document makes a claim about: that the secret appears on exactly two
	// responses, that rotation produces a different one, that an event outside
	// the vocabulary is refused, and that the delivery log answers even when it
	// is empty.
	//
	// The URL is a `.example` name, which RFC 2606 reserves and which therefore
	// resolves for nobody — this test registers a webhook and never drains the
	// queue, but a contract test that left a *resolvable* endpoint behind would
	// be one whose failure mode is somebody else's server getting traffic.
	hook := c.do("POST", p+"/webhooks", map[string]any{
		"url":         "https://hooks.linkctrl.example/contract",
		"events":      []string{"link.created", "link.updated"},
		"description": "contract",
	}, http.StatusCreated)
	webhookID := field(t, hook, "id")
	if secret := field(t, hook, "secret"); len(secret) != 64 {
		t.Errorf("the created webhook carried a %d-character secret; the document "+
			"says 64 hex characters and says this is the only place it appears",
			len(secret))
	}

	// A URL the destination tiers refuse, which is this collection's whole
	// security claim.
	//
	// The other refusal worth replaying — an event outside the vocabulary — is
	// *not* here, and cannot be: this test validates the request against the
	// document before sending it, and the document's enum rejects the body
	// itself. That the server also refuses one is asserted in webhook_test.go,
	// against the service. Two mechanisms, and the enum is the stronger of them.
	c.do("POST", p+"/webhooks", map[string]any{
		"url": "http://169.254.169.254/", "events": []string{"link.created"},
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/webhooks", nil, http.StatusOK)
	c.do("GET", p+"/webhooks/"+webhookID, nil, http.StatusOK)
	c.do("GET", p+"/webhooks/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("PATCH", p+"/webhooks/"+webhookID, map[string]any{
		"events": []string{"destination.blocked"}, "enabled": false,
	}, http.StatusOK)
	c.do("PATCH", p+"/webhooks/"+uuid.NewString(), map[string]any{
		"enabled": false,
	}, http.StatusNotFound)

	rotated := c.do("POST", p+"/webhooks/"+webhookID+"/rotate", nil, http.StatusOK)
	if field(t, rotated, "secret") == field(t, hook, "secret") {
		t.Error("rotating returned the same secret; the document says the previous " +
			"one stops verifying immediately")
	}
	c.do("POST", p+"/webhooks/"+uuid.NewString()+"/rotate", nil, http.StatusNotFound)

	// The delivery log, empty here and still a valid document.
	c.do("GET", p+"/webhooks/"+webhookID+"/deliveries", nil, http.StatusOK)
	c.do("GET", p+"/webhooks/"+webhookID+"/deliveries?limit=5", nil, http.StatusOK)
	c.do("GET", p+"/webhooks/"+uuid.NewString()+"/deliveries", nil, http.StatusNotFound)

	c.do("DELETE", p+"/webhooks/"+webhookID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/webhooks/"+webhookID, nil, http.StatusNotFound)

	// --- automation rules (M43) ---------------------------------------------
	//
	// The whole lifecycle, because every operation is one the document makes a
	// claim about: that a rule is armed at creation rather than left with a null
	// watermark, that the list carries both vocabularies and the evaluation
	// bounds, and that `archive_link` is refused on a trigger with no link.
	//
	// **Nothing here evaluates anything**, and that is the point rather than an
	// omission — there is no endpoint that could. Evaluation is the scheduler's,
	// and test/integration/automation_test.go is where it is driven.
	autoRule := c.do("POST", p+"/automation", map[string]any{
		"name": "contract", "trigger": "link.expired",
		"actions": []string{"notify", "webhook"},
	}, http.StatusCreated)
	autoID := field(t, autoRule, "id")
	if field(t, autoRule, "last_fired_at") == "" {
		t.Error("the created rule carried a null watermark; the document says a rule " +
			"is armed at creation, and a null one means it fires for the whole " +
			"history of the workspace on its first run")
	}

	// `archive_link` on a trigger with no link subject. Refused at write time,
	// which the document says and which is the difference between a rule that
	// will not save and one that saves and silently does nothing.
	//
	// Reachable through the enum, unlike the webhook vocabulary refusal above:
	// the action name is legal, and what makes it invalid is the trigger it is
	// paired with — which no schema can express and only the server can decide.
	c.do("POST", p+"/automation", map[string]any{
		"name": "no link to archive", "trigger": "destination.blocked",
		"actions": []string{"archive_link"},
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/automation", nil, http.StatusOK)
	c.do("GET", p+"/automation/"+autoID, nil, http.StatusOK)
	c.do("GET", p+"/automation/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("PATCH", p+"/automation/"+autoID, map[string]any{
		"trigger_config": map[string]any{"min_count": 3}, "enabled": false,
	}, http.StatusOK)
	c.do("PATCH", p+"/automation/"+uuid.NewString(), map[string]any{
		"enabled": false,
	}, http.StatusNotFound)

	c.do("DELETE", p+"/automation/"+autoID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/automation/"+autoID, nil, http.StatusNotFound)

	// --- QR codes (M41) -----------------------------------------------------
	//
	// The JSON half is replayed like everything else. The picture is not: it is
	// the second non-JSON response this API has, and kin-openapi has no SVG body
	// decoder any more than it has a YAML one — so it is checked by hand, below,
	// exactly as the spec document is.
	c.do("GET", p+"/links/"+linkID+"/qr", nil, http.StatusOK)
	c.do("PUT", p+"/links/"+linkID+"/qr", map[string]any{
		"style": map[string]any{"foreground": "#102030", "level": "H"},
	}, http.StatusOK)
	// A colour that is not a colour. Refused rather than escaped: a stored style
	// becomes the attributes of an SVG this server generates.
	//
	// doBadRequest, because the point is the server's 422 and the body is
	// deliberately outside the schema — the document's own pattern rejects it,
	// which is the request-side check being skipped here rather than a gap.
	c.doBadRequest("PUT", p+"/links/"+linkID+"/qr", map[string]any{
		"style": map[string]any{"foreground": "red"},
	}, http.StatusUnprocessableEntity)
	c.do("PUT", p+"/links/"+uuid.NewString()+"/qr", map[string]any{
		"style": map[string]any{},
	}, http.StatusNotFound)
	c.do("DELETE", p+"/links/"+linkID+"/qr", nil, http.StatusNoContent)
	c.svgEndpoint(linkID)
	c.pngEndpoint(linkID)

	// --- more than one code per link (M50) ----------------------------------
	//
	// **The five above are unchanged and still answer for the link's default
	// code**, which is the choice m50.md required be made and recorded: they are
	// the shorthand rather than growing an identifier, so a client written
	// against the previous release keeps getting the same code. That claim is
	// exactly what this replay holds — the calls above ran before this block and
	// asked for nothing new.
	c.do("GET", p+"/links/"+linkID+"/qr/codes", nil, http.StatusOK)
	var madeCode struct {
		Code struct {
			Slug string `json:"slug"`
		} `json:"code"`
	}
	if err := json.Unmarshal(c.do("POST", p+"/links/"+linkID+"/qr/codes", map[string]any{
		"label": "Autumn poster",
	}, http.StatusCreated), &madeCode); err != nil {
		c.t.Fatalf("decode created qr code: %v", err)
	}
	slug := madeCode.Code.Slug
	if slug == "" {
		c.t.Fatal("a created QR code came back with no slug; the slug is what its " +
			"payload prints and what the redirect resolves")
	}
	c.do("GET", p+"/links/"+linkID+"/qr/codes/"+slug, nil, http.StatusOK)
	c.do("PUT", p+"/links/"+linkID+"/qr/codes/"+slug, map[string]any{
		"label": "Autumn poster, second run",
		"style": map[string]any{"foreground": "#102030"},
	}, http.StatusOK)
	// A slug this link never issued. 404 rather than a default, because a code
	// somebody removed must stop answering.
	c.do("GET", p+"/links/"+linkID+"/qr/codes/zzzzzzzz", nil, http.StatusNotFound)
	c.do("DELETE", p+"/links/"+linkID+"/qr/codes/zzzzzzzz", nil, http.StatusNotFound)
	// Which code an untagged scan resolves through, moved and moved back (D183).
	// **Moved back deliberately**: everything below addresses the link through
	// both the `/qr` shorthand and this code's own slug, and those are two names
	// for one code while the flag sits on it — `svgCodeEndpoint` asserts the two
	// pictures differ, which is a claim about slugs and not about roles.
	var defaultCodeSlug struct {
		QR struct {
			Slug string `json:"slug"`
		} `json:"qr"`
	}
	if err := json.Unmarshal(c.do("GET", p+"/links/"+linkID+"/qr", nil, http.StatusOK),
		&defaultCodeSlug); err != nil {
		c.t.Fatalf("decode the default qr code: %v", err)
	}
	if defaultCodeSlug.QR.Slug == "" {
		c.t.Fatal("the default code has no slug on a link carrying two codes; a code " +
			"gains one when it stops being alone (D183), and without one it cannot " +
			"be addressed, removed or told apart")
	}
	c.do("PUT", p+"/links/"+linkID+"/qr/codes/"+slug+"/default", nil, http.StatusOK)
	c.do("PUT", p+"/links/"+uuid.NewString()+"/qr/codes/"+slug+"/default", nil,
		http.StatusNotFound)
	c.do("PUT", p+"/links/"+linkID+"/qr/codes/"+defaultCodeSlug.QR.Slug+"/default", nil,
		http.StatusOK)
	c.svgCodeEndpoint(linkID, slug)
	c.pngCodeEndpoint(linkID, slug)

	// --- the first file this API accepts (M50.5) ----------------------------
	//
	// The upload, and then the three claims about it that the *document* makes
	// and that a client would otherwise have to discover: that `has_logo` turns
	// over, that an SVG is refused, and that clearing is idempotent.
	logoPath := p + "/links/" + linkID + "/qr/codes/" + slug + "/logo"
	var uploaded struct {
		Code struct {
			HasLogo bool `json:"has_logo"`
		} `json:"code"`
	}
	// The filename is a path escape and the declared type is the one format
	// this product refuses, and neither is read — so the PNG in the body is
	// what decides, and the call succeeds. That is the milestone's sniffing
	// claim replayed at the surface a client meets it.
	if err := json.Unmarshal(c.upload("PUT", logoPath, "logo",
		"../../../etc/passwd", "image/svg+xml", contractLogo(t),
		http.StatusOK, true), &uploaded); err != nil {
		t.Fatalf("decode logo upload: %v", err)
	}
	if !uploaded.Code.HasLogo {
		t.Error("a code came back with has_logo false after an upload; the document " +
			"says that field is how a client knows the upload landed")
	}

	// An SVG. Refused by the server rather than by the schema — the document's
	// request body is `format: binary` and cannot express which bytes are
	// acceptable — so this is a real 422 from the decoder's own refusal, and the
	// request half is checked because the request *is* valid against the spec.
	c.upload("PUT", logoPath, "logo", "logo.png", "image/png",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x()</script></svg>`),
		http.StatusUnprocessableEntity, true)

	// **And once as an API key**, which is m50.5.md's D87 bullet stated
	// positively: this operation is not session-gated, because its subject is
	// the link rather than the person. A key holding `links.update` may
	// restyle a code today, and putting an image on one is the same authority.
	// Replayed here rather than asserted by reading the handler, because what
	// would be wrong is the middleware around it.
	uploader := c.do("POST", p+"/api-keys", map[string]any{
		"name": "logo uploader", "scopes": []string{"links.read", "links.update"},
	}, http.StatusCreated)
	c.uploadAsKey(field(t, uploader, "key"), "PUT", logoPath, "logo",
		"logo.png", "image/png", contractLogo(t), http.StatusOK)

	// **An image past the storage target, which the document says is resized
	// rather than refused** (F214, 2026-08-12). Replayed because the response
	// grows a key for it: `resampled` is absent on every upload above and
	// present on this one, and a schema that forbade it would fail here rather
	// than in a client's parser.
	oversized := c.upload("PUT", logoPath, "logo", "logo.png", "image/png",
		contractOversizedLogo(t), http.StatusOK, true)
	var resized struct {
		Resampled *struct {
			FromWidth int `json:"from_width"`
			Width     int `json:"width"`
		} `json:"resampled"`
	}
	if err := json.Unmarshal(oversized, &resized); err != nil {
		t.Fatalf("decode a resized logo upload: %v", err)
	}
	if resized.Resampled == nil || resized.Resampled.FromWidth <= resized.Resampled.Width {
		t.Errorf("an oversized upload answered %s; the document promises both sizes "+
			"when it had to shrink one", oversized)
	}

	c.do("DELETE", logoPath, nil, http.StatusNoContent)
	// Idempotent: a code with no logo answers the same 204, because "this code
	// has no logo" is already true.
	c.do("DELETE", logoPath, nil, http.StatusNoContent)
	var cleared struct {
		Code struct {
			HasLogo bool `json:"has_logo"`
		} `json:"code"`
	}
	if err := json.Unmarshal(
		c.do("GET", p+"/links/"+linkID+"/qr/codes/"+slug, nil, http.StatusOK), &cleared,
	); err != nil {
		t.Fatalf("decode qr code after clearing its logo: %v", err)
	}
	if cleared.Code.HasLogo {
		t.Error("has_logo is still true after the logo was cleared")
	}
	// A slug this link never issued, on both operations, so neither can be used
	// to ask whether a code exists.
	c.upload("PUT", p+"/links/"+linkID+"/qr/codes/zzzzzzzz/logo", "logo",
		"logo.png", "image/png", contractLogo(t), http.StatusNotFound, true)
	c.do("DELETE", p+"/links/"+linkID+"/qr/codes/zzzzzzzz/logo", nil, http.StatusNotFound)

	// --- and the same capability at the shorthand (M50.5, D136 overruled) ---
	//
	// The owner ruled on 2026-08-07 that the link's *default* code carries a
	// logo too, addressed the way `qr.svg` and `qr.png` address it: without
	// naming a code. Two routes, one capability — which is the same relationship
	// `GET …/qr.png` and `GET …/qr/codes/{slug}/image.png` already have.
	//
	// **The shorthand names a role rather than a slug** (D183), and that is what
	// this block holds. `POST /qr/codes` ran above, so this link carries two
	// codes and its default has a slug of its own; the upload has to land on
	// *that row* rather than inserting a second one against the empty slug the
	// request carried. A client cannot see the difference except through what
	// comes back, which is why the slug is asserted rather than the row count.
	shorthandLogo := p + "/links/" + linkID + "/qr/logo"
	var defaultCode struct {
		QR struct {
			Slug    string `json:"slug"`
			Stored  bool   `json:"stored"`
			HasLogo bool   `json:"has_logo"`
		} `json:"qr"`
	}
	if err := json.Unmarshal(c.upload("PUT", shorthandLogo, "logo",
		"../../brand.svg", "image/svg+xml", contractLogo(t),
		http.StatusOK, true), &defaultCode); err != nil {
		t.Fatalf("decode default-code logo upload: %v", err)
	}
	// Keyed `qr` and not `code`, like every other `/links/{id}/qr` operation, and
	// answering for the code that holds the default flag — which on this link has
	// a slug, and is the one the collection addresses by it.
	if defaultCode.QR.Slug != defaultCodeSlug.QR.Slug {
		t.Errorf("the shorthand answered for the code %q; the link's default is %q. "+
			"An empty answer here means the upload inserted a row against the empty "+
			"slug the request carried instead of landing on the default's own row",
			defaultCode.QR.Slug, defaultCodeSlug.QR.Slug)
	}
	if !defaultCode.QR.HasLogo || !defaultCode.QR.Stored {
		t.Errorf("the default code came back has_logo=%v stored=%v after an upload; "+
			"both are how a client knows the row now exists and carries an image",
			defaultCode.QR.HasLogo, defaultCode.QR.Stored)
	}
	c.do("DELETE", shorthandLogo, nil, http.StatusNoContent)
	// Idempotent here for the same reason it is on a named code, and for one
	// more: a default code with no stored row has no logo either.
	c.do("DELETE", shorthandLogo, nil, http.StatusNoContent)
	// A link this workspace cannot see, on both, so neither is a way to learn
	// that one exists.
	missing := p + "/links/" + uuid.NewString() + "/qr/logo"
	c.upload("PUT", missing, "logo", "logo.png", "image/png",
		contractLogo(t), http.StatusNotFound, true)
	c.do("DELETE", missing, nil, http.StatusNotFound)

	c.do("DELETE", p+"/links/"+linkID+"/qr/codes/"+slug, nil, http.StatusNoContent)
	c.do("GET", p+"/links/"+linkID+"/qr/codes/"+slug, nil, http.StatusNotFound)

	// --- registered domains (M39) -------------------------------------------
	//
	// The lifecycle, plus the two refusals the document names by code. Both are
	// replayed here rather than only in domains_test.go because the *document*
	// claims them: one hostname belongs to one workspace, and the instance
	// default is not administered through this collection. An implementation
	// that quietly allowed either would leave openapi.yaml describing rules
	// nothing enforces.
	registered := c.do("POST", p+"/domains", map[string]any{
		"hostname": "go.contract.example",
	}, http.StatusCreated)
	registeredID := field(t, registered, "id")
	// A hostname is one alias namespace, so a second registration of the same
	// name is refused rather than shared — whoever asks.
	c.do("POST", p+"/domains", map[string]any{
		"hostname": "GO.contract.example",
	}, http.StatusUnprocessableEntity)
	// Not a hostname: the shape people type instead of one.
	c.do("POST", p+"/domains", map[string]any{
		"hostname": "https://go.contract.example/path",
	}, http.StatusUnprocessableEntity)

	domainList := c.do("GET", p+"/domains", nil, http.StatusOK)
	// The instance default is listed, and it is not administered here.
	var listed struct {
		Domains []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"is_default"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(domainList, &listed); err != nil {
		t.Fatalf("domain list is not the documented shape: %v", err)
	}
	var defaultDomainID string
	for _, d := range listed.Domains {
		if d.IsDefault {
			defaultDomainID = d.ID
		}
	}
	if defaultDomainID == "" {
		t.Fatal("the instance default is missing from GET /domains")
	}
	c.do("PATCH", p+"/domains/"+defaultDomainID, map[string]any{
		"hostname": "renamed.contract.example",
	}, http.StatusUnprocessableEntity)

	c.do("PATCH", p+"/domains/"+registeredID, map[string]any{
		"hostname": "links.contract.example",
	}, http.StatusOK)
	c.do("PATCH", p+"/domains/"+uuid.NewString(), map[string]any{
		"hostname": "nowhere.contract.example",
	}, http.StatusNotFound)
	// The gate (M40), replayed here as the document describes it. The contract
	// fixture has no DNS resolver, so verification cannot pass — and the refusal
	// is the half worth replaying anyway: it is what a caller meets when the
	// record is not published, and it is what stops a hostname being served.
	//
	// 503 rather than 422 because this process cannot resolve DNS at all, which
	// is a different answer from "the record is not there" and the document says
	// so. Either way the domain is not verified, which is what the next call
	// asserts.
	c.do("POST", p+"/domains/"+registeredID+"/verify", nil, http.StatusServiceUnavailable)
	// The root redirect is refused on an unverified hostname: nothing is served
	// there, so its root has nowhere to send anybody.
	c.do("PUT", p+"/domains/"+registeredID+"/root-redirect", map[string]any{
		"root_redirect_url": "https://example.com/home",
	}, http.StatusUnprocessableEntity)
	// And a hostname that does not exist is 404 on both, whoever asks.
	c.do("POST", p+"/domains/"+uuid.NewString()+"/verify", nil, http.StatusNotFound)
	c.do("PUT", p+"/domains/"+uuid.NewString()+"/root-redirect", map[string]any{
		"root_redirect_url": "",
	}, http.StatusNotFound)

	c.do("DELETE", p+"/domains/"+registeredID, nil, http.StatusNoContent)
	c.do("DELETE", p+"/domains/"+registeredID, nil, http.StatusNotFound)

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

	// --- domain -------------------------------------------------------------
	c.do("GET", p+"/domain", nil, http.StatusOK)
	// This fixture is single-host, where the root belongs to the dashboard, so
	// setting a root redirect is refused. The 422 is the documented answer and
	// exercises the operation against its schemas either way.
	c.do("PATCH", p+"/domain", map[string]any{
		"root_redirect_url": "https://example.com/home",
	}, http.StatusUnprocessableEntity)

	// --- api keys -----------------------------------------------------------
	key := c.do("POST", p+"/api-keys", map[string]any{
		"name": "contract", "scopes": []string{"links.read"},
	}, http.StatusCreated)
	keyID := field(t, key, "id")
	c.do("POST", p+"/api-keys", map[string]any{
		"name": "escalation", "scopes": []string{"apikeys.write"},
	}, http.StatusUnprocessableEntity)
	c.do("GET", p+"/api-keys", nil, http.StatusOK)

	// Rotation, which is the one operation in the document a session cannot
	// reach: it replaces the credential that made the request, so it is replayed
	// with the key's own token. The session is refused first, because "403 for a
	// session" is part of what the document promises.
	c.do("POST", p+"/api-keys/rotate", map[string]any{}, http.StatusForbidden)
	rotatable := c.do("POST", p+"/api-keys", map[string]any{
		"name": "rotatable", "scopes": []string{"links.read"},
	}, http.StatusCreated)
	c.doAsKey(field(t, rotatable, "key"), "POST", p+"/api-keys/rotate",
		map[string]any{"grace_seconds": 300}, http.StatusCreated)

	// The reach axis (M54). Both the optional organization on creation and the
	// reach on rotation are replayed, because both are request schemas the
	// document now promises and neither is exercised by the calls above.
	//
	// The organization comes off the workspace-bound key minted first: such a key
	// is pinned by construction, so its organization_id is this session's own —
	// and reading it here rather than adding a route to ask for it keeps the
	// replay to operations the document describes.
	sessionOrg := field(t, key, "organization_id")
	accountWide := c.do("POST", p+"/api-keys", map[string]any{
		"name": "account-wide", "scopes": []string{"links.read"}, "org_wide": true,
	}, http.StatusCreated)
	c.do("POST", p+"/api-keys", map[string]any{
		"name": "pinned", "scopes": []string{"links.read"},
		"org_wide": true, "organization_id": sessionOrg,
	}, http.StatusCreated)
	// Another organization is refused on the field rather than accepted and
	// quietly reinterpreted, which is the 422 the document promises.
	c.do("POST", p+"/api-keys", map[string]any{
		"name": "elsewhere", "scopes": []string{"links.read"},
		"org_wide": true, "organization_id": "00000000-0000-4000-8000-0000000000ff",
	}, http.StatusUnprocessableEntity)
	// Narrowing through rotation, sent as the key because rotation always is.
	c.doAsKey(field(t, accountWide, "key"), "POST", p+"/api-keys/rotate",
		map[string]any{"reach": "organization"}, http.StatusCreated)

	c.do("DELETE", p+"/api-keys/"+keyID, nil, http.StatusNoContent)

	// --- audit --------------------------------------------------------------
	// Still signed in with a session, which is the only credential that can
	// reach this: audit.read is non-delegable, so the key minted above could
	// not have been given it (D18).
	c.do("GET", p+"/audit", nil, http.StatusOK)
	c.do("GET", p+"/audit?limit=10", nil, http.StatusOK)
	// A cursor from a different scheme is refused rather than reinterpreted as
	// a position it does not describe.
	c.do("GET", p+"/audit?cursor=not-a-cursor", nil, http.StatusUnprocessableEntity)

	// --- disputes -----------------------------------------------------------
	// Also session-only, and for the escalating reason rather than the
	// disclosing one: a key holding destinations.review could allow a
	// destination and then point links at it (D18).
	//
	// A row has to exist for anything to be refused, so one is inserted the way
	// an operator would. The seeded shorteners would have served, but naming the
	// host here keeps the test readable when somebody edits that seed.
	if _, err := c.f.pool.Exec(t.Context(),
		`INSERT INTO blocked_destinations (host, source, reason)
		 VALUES ('contract-blocked.example', 'review', 'contract test')`); err != nil {
		t.Fatalf("seed a blocked destination: %v", err)
	}

	filed := c.do("POST", p+"/disputes", map[string]any{
		"url": "https://contract-blocked.example/x",
	}, http.StatusCreated)
	disputeID := field(t, filed, "id")
	// One open dispute per host: the second is the documented 409.
	c.do("POST", p+"/disputes", map[string]any{
		"url": "https://contract-blocked.example/y",
	}, http.StatusConflict)
	// The two tiers with no appeal path, as the documented 422. A private
	// address and a host on the compiled list both answer `not_disputable`,
	// which is the whole of the milestone's first bullet seen from outside.
	c.doBadRequest("POST", p+"/disputes", map[string]any{
		"url": "http://169.254.169.254/latest/meta-data/",
	}, http.StatusUnprocessableEntity)
	c.doBadRequest("POST", p+"/disputes", map[string]any{
		"url": "https://metadata.google.internal/computeMetadata/",
	}, http.StatusUnprocessableEntity)

	c.do("GET", p+"/disputes", nil, http.StatusOK)
	c.do("GET", p+"/disputes?open=true&limit=10", nil, http.StatusOK)
	c.do("GET", p+"/disputes?cursor=not-a-cursor", nil, http.StatusUnprocessableEntity)

	// Upheld first, then a second dispute allowed, so both decisions are
	// exercised and both 409s stay reachable: deciding twice, and an id from
	// nowhere.
	c.do("POST", p+"/disputes/"+disputeID+"/uphold", nil, http.StatusOK)
	c.do("POST", p+"/disputes/"+disputeID+"/uphold", nil, http.StatusConflict)
	c.do("POST", p+"/disputes/"+uuid.NewString()+"/allow", nil, http.StatusNotFound)

	second := c.do("POST", p+"/disputes", map[string]any{
		"url": "https://contract-blocked.example/z",
	}, http.StatusCreated)
	c.do("POST", p+"/disputes/"+field(t, second, "id")+"/allow", nil, http.StatusOK)

	// --- the instance-level principal ---------------------------------------
	// This fixture's account claimed the instance through /auth/setup, so it is
	// the principal (D98) — which is also why the two decisions above succeeded:
	// destinations.decide is held instance-wide and by no organization role.
	//
	// The roster is exercised in full because the delegation bound is a
	// documented behaviour rather than an implementation detail: the response
	// carries no scope list, the request takes no scope field, and there is no
	// operation anywhere in this spec that confers instance.admin.
	c.do("GET", p+"/instance/reviewers", nil, http.StatusOK)
	// An address with no account is the documented 422 rather than a silent
	// no-op: appointing nobody and believing you had is the mistake this
	// endpoint's reader would actually make.
	c.doBadRequest("POST", p+"/instance/reviewers", map[string]any{
		"email": "nobody@contract.example",
	}, http.StatusUnprocessableEntity)

	// A second account to appoint, made the way this instance makes one.
	reviewerEmail := "reviewer@contract.example"
	c.f.registerAccount(reviewerEmail)
	appointed := c.do("POST", p+"/instance/reviewers", map[string]any{
		"email": reviewerEmail,
	}, http.StatusOK)
	// Idempotent, and 200 both times: the second call creates nothing, so a 201
	// would describe something that did not happen.
	c.do("POST", p+"/instance/reviewers", map[string]any{
		"email": reviewerEmail,
	}, http.StatusOK)
	c.do("DELETE", p+"/instance/reviewers/"+field(t, appointed, "user_id"), nil,
		http.StatusNoContent)
	// Withdrawing from somebody who holds nothing is a 404 rather than a 204,
	// because "there was nothing to withdraw" and "withdrawn" are different
	// answers to an administrator.
	c.do("DELETE", p+"/instance/reviewers/"+uuid.NewString(), nil, http.StatusNotFound)

	// The instance-wide audit surface (F36). It has rows by now: the two dispute
	// decisions above and the appointment belong to no organization, which is
	// the whole of what this endpoint exists to make readable.
	c.do("GET", p+"/instance/audit", nil, http.StatusOK)
	c.do("GET", p+"/instance/audit?limit=10", nil, http.StatusOK)
	c.do("GET", p+"/instance/audit?cursor=not-a-cursor", nil, http.StatusUnprocessableEntity)

	// --- add-ons (M67) ------------------------------------------------------
	//
	// The whole lifecycle, against a real WebAssembly module, because every claim
	// the document makes here is about what the host did rather than about a
	// row: that the digest is checked, that the answer names the permissions the
	// add-on *holds*, and that removing one answers with what it left behind.
	//
	// The session is the account that claimed the instance, so it is the
	// principal and holds `addons.manage`. That the scope cannot reach an API key
	// is asserted in addon_lifecycle_test.go, where a key can be minted to try.
	module := addonFixture(t, "minimal")
	manifest := contractAddonManifest(t, "contract", module)
	installed := c.uploadParts("POST", p+"/addons", map[string][]byte{
		"manifest": manifest, "module": module,
	}, http.StatusCreated, true)
	if got := field(t, installed, "name"); got != "contract" {
		t.Errorf("the install answered for %q", got)
	}
	// A module whose bytes are not the bytes the manifest describes, refused
	// before anything is written — the one refusal on this endpoint that is about
	// the upload rather than about the manifest.
	c.uploadParts("POST", p+"/addons", map[string][]byte{
		"manifest": manifest, "module": append([]byte{0x00}, module...),
	}, http.StatusUnprocessableEntity, true)
	// One name, one add-on: replacing is a removal and an install.
	c.uploadParts("POST", p+"/addons", map[string][]byte{
		"manifest": manifest, "module": module,
	}, http.StatusConflict, true)
	// --- and the second shape at the same address (M68.6) --------------------
	//
	// What is replayed here is the *document*: that the endpoint takes a `url` and
	// a `sha256` instead of the two file parts, that the two shapes are exclusive
	// in the schema rather than in prose, and that a refused fetch is a 422 with
	// field errors the document describes. What is **not** replayed is a fetch that
	// succeeds — this instance dials globally-routable addresses only, so a test
	// server on loopback is refused by the address policy at dial time, which is
	// the policy working rather than a gap. The fetch itself is driven against a
	// real origin in internal/addon, where the address check is a field a test can
	// relax without relaxing the policy.
	//
	// So the address below is a documentation range: it parses, it resolves to
	// itself, and it is carved out — which makes this one call an assertion about
	// the schema *and* about the bound.
	c.uploadParts("POST", p+"/addons", map[string][]byte{
		"url":    []byte("https://192.0.2.1/contract.tar"),
		"sha256": []byte(strings.Repeat("ab", 32)),
	}, http.StatusUnprocessableEntity, true)
	// Both shapes at once is refused, and `checkRequest` is false because that is
	// exactly what the document's `oneOf` refuses: validating the request against
	// the spec would fail before the server ever answered, which proves the schema
	// and not the handler. Both are worth having and this is the one that is about
	// the server.
	c.uploadParts("POST", p+"/addons", map[string][]byte{
		"manifest": manifest, "module": module,
		"url":    []byte("https://192.0.2.1/contract.tar"),
		"sha256": []byte(strings.Repeat("ab", 32)),
	}, http.StatusUnprocessableEntity, false)
	// The manager's reads and its two writes (M68), against the module that is
	// installed right now. Every one of them is replayed against the document, which
	// is what the inherited *every UI feature has API support* rule buys here: the
	// page drives exactly these.
	c.do("GET", p+"/addons", nil, http.StatusOK)
	detail := c.do("GET", p+"/addons/contract", nil, http.StatusOK)
	if got := field(t, detail, "declaration"); got != "none" {
		t.Errorf("the fixture's declaration class is %q; it holds no redirect grant", got)
	}
	c.do("GET", p+"/addons/nosuch", nil, http.StatusNotFound)
	// The empty save: a write through the same transaction and the same audit
	// record, carrying nothing. It runs first so that the save below has something
	// to differ from.
	c.do("PUT", p+"/addons/contract/settings", map[string]any{
		"values": map[string]string{},
	}, http.StatusOK)
	// And a save that actually stores something, one value per declared type. Both,
	// because the empty one validates `AddonSetting` against `[]`, which is a schema
	// exercised in name only.
	saved := c.do("PUT", p+"/addons/contract/settings", map[string]any{
		"values": map[string]string{
			"endpoint": "https://contract.example/token", "client_secret": "s3cret",
			"mode": "slow", "verbose": "true",
		},
	}, http.StatusOK)
	// The one field this response must not carry. It is asserted here rather than
	// left to internal/addon's tests because the *document* says a secret's value is
	// never in an answer, and a contract test that replayed the operation without
	// looking would agree with a server that had started echoing it.
	if bytes.Contains(saved, []byte("s3cret")) {
		t.Error("the settings response echoed a stored secret back")
	}
	// And a key it does not declare is refused rather than ignored, for the reason
	// an unknown multipart part is.
	c.do("PUT", p+"/addons/contract/settings", map[string]any{
		"values": map[string]string{"nosuch": "x"},
	}, http.StatusUnprocessableEntity)
	c.do("PUT", p+"/addons/nosuch/settings", map[string]any{
		"values": map[string]string{},
	}, http.StatusNotFound)

	// The orphan list while the add-on is installed: empty, and that is an answer
	// worth replaying, because an empty array and an absent field are different
	// documents.
	if n := len(orphanRows(t, c.do("GET", p+"/addons/"+httpx.AddonOrphanPath, nil,
		http.StatusOK))); n != 0 {
		t.Errorf("the orphan list has %d rows while the only add-on is installed", n)
	}
	// Purging the data of an add-on that is installed is a conflict, and purging a
	// name that owns nothing is a 404 rather than a success nobody could tell from
	// one.
	c.do("DELETE", p+"/addons/"+httpx.AddonOrphanPath+"/contract", nil, http.StatusConflict)
	c.do("DELETE", p+"/addons/"+httpx.AddonOrphanPath+"/nosuch", nil, http.StatusNotFound)

	c.do("DELETE", p+"/addons/contract", nil, http.StatusOK)
	c.do("DELETE", p+"/addons/contract", nil, http.StatusNotFound)

	// **And now the orphan replays that need something to have been left behind.**
	// A removal deliberately keeps the schema, so this is the only point in the run
	// where `AddonOrphan` is a row rather than an empty array and where
	// `purgeAddonData` has a name it can answer `200` for. Running them before the
	// removal — which is where they used to be — validated two new schemas and a
	// success status against nothing at all.
	left := orphanRows(t, c.do("GET", p+"/addons/"+httpx.AddonOrphanPath, nil,
		http.StatusOK))
	if len(left) != 1 || left[0].Name != contractAddonName {
		t.Fatalf("after the removal the orphan list is %+v; the removed add-on owned a schema", left)
	}
	// **The count of what the purge will not take.** Four settings were saved above
	// and the removal deleted none of them — they are keyed on the add-on's *name*,
	// so whatever is installed under it next reads them. This is the number the
	// manager's confirmation puts in front of whoever presses the button.
	if left[0].StoredSettings != 4 {
		t.Errorf("the orphan reports %d stored settings; four were saved and a removal "+
			"deletes none of them", left[0].StoredSettings)
	}
	purged := c.do("DELETE", p+"/addons/"+httpx.AddonOrphanPath+"/contract", nil, http.StatusOK)
	// The purge does not take them either, which is the claim four documents make
	// and the reason the confirmation says *four things*.
	if got := orphanRow(t, purged).StoredSettings; got != 4 {
		t.Errorf("the purge answered with %d stored settings left; it drops the schema "+
			"and nothing else", got)
	}
	// Gone, and the second purge is the 404 a typo gets rather than a success.
	if n := len(orphanRows(t, c.do("GET", p+"/addons/"+httpx.AddonOrphanPath, nil,
		http.StatusOK))); n != 0 {
		t.Errorf("the purge left %d orphan rows", n)
	}
	c.do("DELETE", p+"/addons/"+httpx.AddonOrphanPath+"/contract", nil, http.StatusNotFound)

	// **`ManagedAddon.performance` is the one new schema no operation here can
	// produce, and it is validated against the document rather than left unchecked.**
	// It is present only for a module that has actually run on the redirect path,
	// which needs a `Redirect` handler, a link, and a metrics registry shared
	// between the two — none of which this fixture has, because it is built for the
	// API surface and the redirect path is `newRedirect`'s. Wiring one in to make a
	// field appear would be a second server in the fixture every other replay then
	// runs against.
	//
	// So the object comes out of the **producer** rather than out of a struct
	// literal here: a registry, three observations and two kills, read back through
	// the same `AddonPerformance()` the manager calls. A literal would assert that
	// the schema matches whatever this file typed; this asserts that it matches what
	// the code emits, which is what the replay would have bought.
	c.validateSchema("ManagedAddon", "performance", addonPerformanceSample())

	// --- reputation feeds ---------------------------------------------------
	// One operation, and the fixture has no feed configured — which is the
	// shipped default and the answer this replay validates against the schema:
	// `enabled: false` with every other field absent. There is deliberately no
	// write counterpart to exercise, because a feed is switched on in the
	// instance's configuration and nowhere else (D40).
	c.do("GET", p+"/feeds", nil, http.StatusOK)

	// --- notifications ------------------------------------------------------
	// The dispute above raised two — one to the owner when it was filed, one
	// back when it was decided — so this lists an inbox with rows in it rather
	// than the empty one it listed before M31.
	c.do("GET", p+"/notifications", nil, http.StatusOK)
	c.do("GET", p+"/notifications?unread=true&limit=10", nil, http.StatusOK)
	c.do("GET", p+"/notifications/unread", nil, http.StatusOK)
	c.do("POST", p+"/notifications/read", nil, http.StatusOK)
	// Marking an id that does not exist is a 204, not a 404 — someone else's
	// notification must be indistinguishable from one that was never there.
	c.do("POST", p+"/notifications/"+uuid.NewString()+"/read", nil, http.StatusNoContent)
	// And back to unread (M48), which is a DELETE of the read state rather than
	// a second verb: `read_at` is a column with a value or without one. Probed
	// with an id that does not exist for the same reason the line above is, and
	// answering the same 204.
	c.do("DELETE", p+"/notifications/"+uuid.NewString()+"/read", nil, http.StatusNoContent)

	// --- workspaces ---------------------------------------------------------
	// One membership on this fixture, which is the shape every instance has
	// today: the switcher lists it, switching to it succeeds, and pinning it is
	// a real preference even though it changes nothing.
	var wsList struct {
		Items []struct {
			ID             string `json:"id"`
			OrganizationID string `json:"organization_id"`
			Current        bool   `json:"current"`
		} `json:"items"`
	}
	if err := json.Unmarshal(c.do("GET", p+"/workspaces", nil, http.StatusOK), &wsList); err != nil {
		t.Fatalf("workspaces response is not the documented shape: %v", err)
	}
	if len(wsList.Items) != 1 || !wsList.Items[0].Current {
		t.Fatalf("expected exactly one current workspace, got %+v", wsList.Items)
	}
	workspaceID := wsList.Items[0].ID
	organizationID := wsList.Items[0].OrganizationID

	c.do("POST", p+"/workspaces/"+workspaceID+"/switch", nil, http.StatusNoContent)
	// A workspace this account has nothing to do with is not-found, not
	// forbidden: an id must not be probeable for existence.
	c.do("POST", p+"/workspaces/"+uuid.NewString()+"/switch", nil, http.StatusNotFound)
	c.do("PUT", p+"/workspaces/default", map[string]any{
		"workspace_id": workspaceID,
	}, http.StatusNoContent)
	// null is the documented way back to last-used, and has to survive the
	// schema as a real value rather than as an omitted field.
	c.do("PUT", p+"/workspaces/default", map[string]any{
		"workspace_id": nil,
	}, http.StatusNoContent)

	// --- invitations --------------------------------------------------------
	// The link comes back in the create response and nowhere else, so the
	// redemption below has to read it out of this body — which is also the
	// documented claim being exercised.
	invitation := c.do("POST", p+"/invitations", map[string]any{
		"email": "invited@example.com", "role": "editor",
	}, http.StatusCreated)
	inviteURL := field(t, invitation, "url")
	_, token, ok := strings.Cut(inviteURL, "/invite/")
	if !ok || token == "" {
		t.Fatalf("invitation url %q does not carry a token", inviteURL)
	}
	// One outstanding invitation per address, so revoking the visible one cannot
	// leave another live.
	c.do("POST", p+"/invitations", map[string]any{
		"email": "invited@example.com",
	}, http.StatusUnprocessableEntity)
	c.do("GET", p+"/invitations", nil, http.StatusOK)

	revocable := c.do("POST", p+"/invitations", map[string]any{
		"email": "second@example.org", "role": "viewer",
	}, http.StatusCreated)
	revocableID := field(t, revocable, "id")
	c.do("DELETE", p+"/invitations/"+revocableID, nil, http.StatusNoContent)
	// Revoking twice is not-found, the same answer an id from another
	// organization gets, so an id cannot be probed.
	c.do("DELETE", p+"/invitations/"+revocableID, nil, http.StatusNotFound)

	c.do("POST", p+"/invitations/redeem", map[string]any{
		"token": token, "email": "invited@example.com", "password": password,
	}, http.StatusOK)
	// Single-use, and the second attempt gets the same 404 every other failure
	// gets — including a wrong address, which is what keeps redemption from
	// answering whether an address is registered.
	c.do("POST", p+"/invitations/redeem", map[string]any{
		"token": token, "email": "invited@example.com", "password": password,
	}, http.StatusNotFound)

	// --- members, workspaces, organizations ---------------------------------
	// The redemption above put a second person in this organization, which is
	// what makes the member operations exercisable at all: a fixture with one
	// member can list but not manage, since nobody is below an owner except the
	// person who just joined.
	var memberList struct {
		Items []struct {
			ID         string `json:"id"`
			UserID     string `json:"user_id"`
			Email      string `json:"email"`
			Manageable bool   `json:"manageable"`
		} `json:"items"`
	}
	if err := json.Unmarshal(c.do("GET", p+"/members", nil, http.StatusOK), &memberList); err != nil {
		t.Fatalf("members response is not the documented shape: %v", err)
	}
	var invitedMembership, invitedUser string
	for _, m := range memberList.Items {
		if m.Email == "invited@example.com" {
			invitedMembership, invitedUser = m.ID, m.UserID
		}
	}
	if invitedMembership == "" {
		t.Fatalf("the redeemed invitation is not in the member list: %+v", memberList.Items)
	}

	newWorkspace := c.do("POST", p+"/workspaces", map[string]any{
		"name": "Marketing",
	}, http.StatusCreated)
	marketingID := field(t, newWorkspace, "id")
	// A duplicate name is the documented 422; the slug is what enforces it.
	c.do("POST", p+"/workspaces", map[string]any{"name": "Marketing"},
		http.StatusUnprocessableEntity)

	// A workspace-scoped membership: it adds a role there and narrows nothing.
	c.do("POST", p+"/members", map[string]any{
		"user_id": invitedUser, "workspace_id": marketingID, "role": "admin",
	}, http.StatusCreated)

	c.do("PATCH", p+"/members/"+invitedMembership, map[string]any{
		"role": "viewer",
	}, http.StatusNoContent)
	// The owner is the last one, so demoting themselves is the documented 409.
	var ownMembership string
	for _, m := range memberList.Items {
		if m.Email == "owner@example.com" {
			ownMembership = m.ID
		}
	}
	c.do("PATCH", p+"/members/"+ownMembership, map[string]any{
		"role": "admin",
	}, http.StatusConflict)
	c.do("DELETE", p+"/members/"+invitedMembership, nil, http.StatusNoContent)
	// A membership id from nowhere is not-found, so ids cannot be probed.
	c.do("DELETE", p+"/members/"+uuid.NewString(), nil, http.StatusNotFound)

	c.do("PATCH", p+"/workspaces/"+marketingID, map[string]any{
		"name": "Growth",
	}, http.StatusOK)
	c.do("DELETE", p+"/workspaces/"+marketingID, nil, http.StatusNoContent)
	// The remaining one is the organization's last, which is the other 409 the
	// delete documents.
	c.do("DELETE", p+"/workspaces/"+workspaceID, nil, http.StatusConflict)

	acme := c.do("POST", p+"/organizations", map[string]any{"name": "Acme"}, http.StatusCreated)
	// Deliberately below the schema's own floor: the point is the server's 422,
	// which a spec-side request check would pre-empt and prove nothing about.
	c.doBadRequest("POST", p+"/organizations", map[string]any{"name": ""},
		http.StatusUnprocessableEntity)

	// Deleting one. The current organization still holds the link created at the
	// top of this test, which is the documented 409 — and the second registered
	// account's personal organization is what keeps the *other* 409, the
	// instance's last organization, from being the one that fires.
	c.do("DELETE", p+"/organizations/"+organizationID, nil, http.StatusConflict)
	// An id that is not the organization being acted in is not-found, so an id
	// cannot be probed and a mistyped one deletes nothing. Switching into the new
	// organization is therefore how it gets deleted.
	c.do("DELETE", p+"/organizations/"+uuid.NewString(), nil, http.StatusNotFound)
	c.do("POST", p+"/workspaces/"+field(t, acme, "workspace_id")+"/switch", nil, http.StatusNoContent)
	c.do("DELETE", p+"/organizations/"+field(t, acme, "id"), nil, http.StatusNoContent)

	// --- link deletion, password, logout ------------------------------------
	c.do("DELETE", p+"/links/"+linkID, nil, http.StatusNoContent)
	c.do("POST", p+"/auth/password", map[string]string{
		"current_password": password, "new_password": "a-brand-new-longer-password",
	}, http.StatusNoContent)

	// --- the second factor (M53) --------------------------------------------
	//
	// The whole lifecycle, replayed in the order a person walks it: read the
	// state, take an offer, confirm it with a code the offer's own secret
	// produces, issue a fresh set of recovery codes, sign in through the prompt,
	// and turn it off again.
	//
	// **It ends with the factor off**, which is load-bearing for everything below:
	// the deletion refusal and the recovery block both drive the account, and a
	// second factor left on would put a code prompt in front of one of them.
	c.do("GET", p+"/auth/mfa", nil, http.StatusOK)

	var offer struct {
		Secret string `json:"secret"`
		QRSVG  string `json:"qr_svg"`
	}
	if err := json.Unmarshal(c.do("POST", p+"/auth/mfa/enrol", nil, http.StatusOK), &offer); err != nil {
		t.Fatalf("decode the enrolment offer: %v", err)
	}
	if offer.Secret == "" || !strings.HasPrefix(offer.QRSVG, "<svg") {
		t.Fatalf("the enrolment offer carries no secret or no drawing: %+v", offer)
	}
	// And the drawing carries nothing that only resolves inside this product.
	// `qr.Render`'s default class is two Tailwind utilities the dashboard's own
	// stylesheet defines (F184); a client reading `qr_svg` has never seen it, so
	// the class is markup naming a rule it cannot satisfy — the same reason the
	// downloaded SVG is drawn with the empty class.
	if strings.Contains(offer.QRSVG, "class=") {
		t.Errorf("qr_svg carries a class attribute: %.140s…\n\nThis body leaves the "+
			"product, and a class list means nothing outside the dashboard.",
			offer.QRSVG)
	}

	// A wrong code is the documented 401, and it must not enrol anything.
	c.doBadRequest("POST", p+"/auth/mfa/confirm", map[string]string{
		"secret": offer.Secret, "code": "000000",
	}, http.StatusUnauthorized)

	// The previous step's code, because EnableUserMFA stamps the enrolling step
	// as the replay floor — the code that completes an enrolment cannot also be
	// the one that signs somebody in. Enrolling with the step before leaves the
	// current one usable at the prompt below, which is what a person experiences
	// as their first sign-in needing the next code.
	enrolCode, err := auth.TOTPCode(offer.Secret, auth.TOTPStep(time.Now().Add(-auth.TOTPPeriod)))
	if err != nil {
		t.Fatal(err)
	}
	var enrolled struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(c.do("POST", p+"/auth/mfa/confirm", map[string]string{
		"secret": offer.Secret, "code": enrolCode,
	}, http.StatusOK), &enrolled); err != nil {
		t.Fatalf("decode the enrolment: %v", err)
	}
	if len(enrolled.RecoveryCodes) != 10 {
		t.Fatalf("enrolment returned %d recovery codes, want 10", len(enrolled.RecoveryCodes))
	}
	// Enrolling twice is the documented conflict.
	c.doBadRequest("POST", p+"/auth/mfa/enrol", nil, http.StatusConflict)

	var regenerated struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(c.do("POST", p+"/auth/mfa/recovery-codes", nil,
		http.StatusOK), &regenerated); err != nil {
		t.Fatalf("decode the regenerated codes: %v", err)
	}

	// The sign-in that stops at the prompt. 401 with an `mfa-required` problem
	// carrying the token, which is the shape a client has to handle — a 200 with
	// a flag would be read as success by everything that checks the status.
	var challenge struct {
		Type     string `json:"type"`
		MFAToken string `json:"mfa_token"`
	}
	if err := json.Unmarshal(c.doBadRequest("POST", p+"/auth/login", map[string]string{
		"email": "owner@example.com", "password": "a-brand-new-longer-password",
	}, http.StatusUnauthorized), &challenge); err != nil {
		t.Fatalf("decode the second-factor refusal: %v", err)
	}
	if !strings.HasSuffix(challenge.Type, "mfa-required") || challenge.MFAToken == "" {
		t.Fatalf("the refusal is not an mfa-required challenge: %+v", challenge)
	}
	c.doBadRequest("POST", p+"/auth/mfa/challenge", map[string]string{
		"token": challenge.MFAToken, "code": "000000",
	}, http.StatusUnauthorized)
	c.do("POST", p+"/auth/mfa/challenge", map[string]string{
		"token": challenge.MFAToken, "code": regenerated.RecoveryCodes[0],
	}, http.StatusOK)

	// And off again, on the password and a second recovery code.
	c.do("DELETE", p+"/auth/mfa", map[string]string{
		"password": "a-brand-new-longer-password",
		"code":     regenerated.RecoveryCodes[1],
	}, http.StatusNoContent)

	// --- account deletion (M52) ---------------------------------------------
	//
	// **The refusal, and only the refusal.** This account claimed the instance,
	// so it is the instance principal and `DELETE /account` answers 409 naming
	// `lctl instance principal move` — which is the documented response being
	// replayed, and is also the only one this test can reach. The success is 204
	// and it ends the session everything below depends on; it is asserted in
	// account_test.go against the rows, which is where the interesting part of
	// this operation is anyway.
	//
	// Before the recovery block rather than after it, because the reset there
	// revokes every session on the account and this would then be answering 401
	// for a reason that has nothing to do with deletion.
	c.do("DELETE", p+"/account", map[string]string{
		"password": "a-brand-new-longer-password",
	}, http.StatusConflict)

	// --- account recovery (M51) ---------------------------------------------
	//
	// Last of the authenticated flow on purpose: the reset revokes every session
	// on the account, so anything after it would be running unauthenticated. The
	// logout below is what the spec says happens either way.
	//
	// **The refusals are replayed before the success**, because a spent token is
	// one of them and the success is what spends it. 202 is the answer to an
	// address with no account as much as to one with — that equality is asserted
	// against whole bodies in recovery_test.go, and what is documented here is
	// only that the status is what the spec says.
	c.do("POST", p+"/auth/forgot", map[string]string{
		"email": "nobody-here@example.com",
	}, http.StatusAccepted)
	c.doBadRequest("POST", p+"/auth/forgot", map[string]string{
		"email": "not-an-address",
	}, http.StatusUnprocessableEntity)
	c.do("POST", p+"/auth/forgot", map[string]string{
		"email": "owner@example.com",
	}, http.StatusAccepted)

	// The token exists only in the mail body, which is the property M51 is
	// about — nothing hands the plaintext back to a caller, so the only way to
	// spend one is to read the message. The outbox is where a client cannot look
	// and a test can.
	resetToken := c.resetTokenFromOutbox()

	c.doBadRequest("POST", p+"/auth/reset", map[string]string{
		"token": resetToken, "new_password": "short",
	}, http.StatusUnprocessableEntity)
	c.do("POST", p+"/auth/reset", map[string]string{
		"token": "not-a-token-anybody-issued", "new_password": "a-recovered-longer-password",
	}, http.StatusNotFound)
	c.do("POST", p+"/auth/reset", map[string]string{
		"token": resetToken, "new_password": "a-recovered-longer-password",
	}, http.StatusNoContent)
	// Spent, and answered the same way a token that never existed is.
	c.do("POST", p+"/auth/reset", map[string]string{
		"token": resetToken, "new_password": "a-second-recovered-password",
	}, http.StatusNotFound)

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

// resetTokenFromOutbox reads the newest password-reset link out of the mail
// outbox and returns its token.
//
// It goes to the database rather than to a response because that is the whole
// point of the mechanism: the plaintext exists in exactly one place a client
// can reach, which is the mailbox, and the row stores only its SHA-256. A test
// helper that received the token from an endpoint would be exercising an API
// M51 deliberately does not have.
func (c *contract) resetTokenFromOutbox() string {
	c.t.Helper()
	var body string
	err := c.f.pool.QueryRow(c.t.Context(),
		`SELECT body FROM mail_outbox WHERE kind = $1 ORDER BY created_at DESC, id DESC LIMIT 1`,
		recovery.MailKind).Scan(&body)
	if err != nil {
		c.t.Fatalf("no %s mail in the outbox: %v", recovery.MailKind, err)
	}
	const marker = "/reset/"
	i := strings.Index(body, marker)
	if i < 0 {
		c.t.Fatalf("no reset link in the mail body:\n%s", body)
	}
	return strings.Fields(body[i+len(marker):])[0]
}

// svgEndpoint checks the other non-JSON response by hand (M41), following the
// shape yamlSpecEndpoint established below: kin-openapi has no SVG body decoder
// either, so the generic validator cannot cover it.
//
// What the validator would have checked is checked here instead — the status,
// the exact content type, and that the body is what the document says it is.
// The last one is stronger than "a string": the document says this is an SVG
// document holding a QR code, so the check is that it parses as one and that the
// drawing has modules in it. A 200 with an empty `<svg/>` would satisfy the
// schema and would be a blank square on a poster.
func (c *contract) svgEndpoint(linkID string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet,
		c.f.server.URL+"/api/v1/links/"+linkID+"/qr.svg", nil)
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
		c.t.Fatalf("qr.svg returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		c.t.Errorf("qr.svg Content-Type = %q", ct)
	}
	// Well-formed XML, which is the one property "type: string" cannot express
	// and the one a browser will refuse the response over.
	if err := xml.Unmarshal(body, new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		c.t.Errorf("qr.svg is not a well-formed SVG document: %v", err)
	}
	if !bytes.Contains(body, []byte("<rect")) {
		c.t.Error("qr.svg holds no modules; the document promises a QR code, not a blank square")
	}
	c.hit["getLinkQRSVG"] = true
}

// pngEndpoint is svgEndpoint's sibling, and the third non-JSON response this
// API has (M49). Same reason it is checked by hand: `format: binary` is as far as
// the document can describe an image, and kin-openapi has no decoder for one.
//
// What is checked beyond the status and the content type is what the document
// promises and a schema cannot: that the bytes decode as a PNG at all, that the
// picture is square, and that it carries the download disposition — a response
// the document calls a file to save, served without one, is a document that is
// wrong about the endpoint's whole purpose.
func (c *contract) pngEndpoint(linkID string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet,
		c.f.server.URL+"/api/v1/links/"+linkID+"/qr.png", nil)
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
		c.t.Fatalf("qr.png returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		c.t.Errorf("qr.png Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		c.t.Errorf("qr.png Content-Disposition = %q; the document says this is a file "+
			"to save", cd)
	}
	img, format, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("qr.png is not a decodable image: %v", err)
	}
	if format != "png" {
		c.t.Errorf("the .png path served a %s", format)
	}
	if b := img.Bounds(); b.Dx() < 1 || b.Dx() != b.Dy() {
		c.t.Errorf("qr.png is %dx%d; a QR code is square", b.Dx(), b.Dy())
	}
	c.hit["getLinkQRPNG"] = true
}

// svgCodeEndpoint and pngCodeEndpoint are the same two checks for a named code
// (M50), which are the fourth and fifth non-JSON responses this API has.
//
// **The picture has to differ from the default code's, and that is the assertion
// worth having here.** A named code exists to be told apart, and a `.svg` path
// that ignored its slug would serve two identical pictures under two identities
// — the whole feature silently absent, with every status code correct. So the
// body is checked against the default code's bytes rather than only against the
// schema.
func (c *contract) svgCodeEndpoint(linkID, slug string) {
	c.t.Helper()
	named := c.image("/api/v1/links/"+linkID+"/qr/codes/"+slug+"/image.svg", "image/svg+xml")
	if err := xml.Unmarshal(named, new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		c.t.Errorf("a named code's svg is not a well-formed SVG document: %v", err)
	}
	if !bytes.Contains(named, []byte("<rect")) {
		c.t.Error("a named code's svg holds no modules")
	}
	if bytes.Equal(named, c.image("/api/v1/links/"+linkID+"/qr.svg", "image/svg+xml")) {
		c.t.Error("a named code draws the same picture as the link's default code; the " +
			"slug is supposed to be in the payload, which is what tells the two apart")
	}
	c.hit["getLinkQRCodeSVG"] = true
}

func (c *contract) pngCodeEndpoint(linkID, slug string) {
	c.t.Helper()
	body := c.image("/api/v1/links/"+linkID+"/qr/codes/"+slug+"/image.png", "image/png")
	img, format, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("a named code's png is not a decodable image: %v", err)
	}
	if format != "png" {
		c.t.Errorf("the named code's .png path served a %s", format)
	}
	if b := img.Bounds(); b.Dx() < 1 || b.Dx() != b.Dy() {
		c.t.Errorf("a named code's png is %dx%d; a QR code is square", b.Dx(), b.Dy())
	}
	c.hit["getLinkQRCodePNG"] = true
}

// image fetches a picture endpoint and checks the two things a schema cannot
// express about one: that it answered 200 and that it said what it is.
func (c *contract) image(path, contentType string) []byte {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet, c.f.server.URL+path, nil)
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
		c.t.Fatalf("GET %s returned %d", path, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentType {
		c.t.Errorf("GET %s Content-Type = %q, want %q", path, ct, contentType)
	}
	return body
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

// contractOrphan is one row of an orphan listing, read back as a client would.
//
// Declared here rather than reusing addon.Orphan, because what these replays check
// is the *document*: a field renamed in Go and in the spec together would still
// round-trip through the producing type and would be a broken client.
type contractOrphan struct {
	Name           string `json:"name"`
	Schema         string `json:"schema"`
	StoredSettings int64  `json:"stored_settings"`
	IdentityLinks  int64  `json:"identity_links"`
}

// orphanRows reads an orphan listing, in the order the server returned it.
//
// A helper rather than `field`, because what these replays assert is how many rows
// there are and which — the two things an empty array cannot tell you apart from a
// full one, and the whole reason the listing moved after the removal.
func orphanRows(t *testing.T, raw []byte) []contractOrphan {
	t.Helper()
	var body struct {
		Orphans []contractOrphan `json:"orphans"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("orphan listing is not the documented shape: %v", err)
	}
	return body.Orphans
}

// orphanRow reads the single orphan a purge answers with.
func orphanRow(t *testing.T, raw []byte) contractOrphan {
	t.Helper()
	var o contractOrphan
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("purge answer is not the documented shape: %v", err)
	}
	return o
}

// validateSchema checks one document against a property's schema inside a named
// component.
//
// For the one new schema no operation in this replay can produce. It is the same
// validator every response above goes through — `openapi3.Schema.VisitJSON` is
// what the response validator calls — reached directly because there is no
// response to attach it to. A property rather than a component because that is how
// the document declares this one: `performance` is written inline in
// `ManagedAddon`, so this walks to it rather than making the document change shape
// to suit a test.
func (c *contract) validateSchema(component, property string, v any) {
	c.t.Helper()
	ref, ok := c.doc.Components.Schemas[component]
	if !ok {
		c.t.Fatalf("the document has no %s schema", component)
	}
	prop, ok := ref.Value.Properties[property]
	if !ok {
		c.t.Fatalf("%s declares no %s property", component, property)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal a %s.%s: %v", component, property, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		c.t.Fatalf("re-read a %s.%s: %v", component, property, err)
	}
	if err := prop.Value.VisitJSON(doc); err != nil {
		c.t.Errorf("%s.%s does not satisfy its own schema: %v\n  %s",
			component, property, err, raw)
	}
}

// addonPerformanceSample is what the manager reads for a module that has run
// inline three times and been killed twice.
//
// Through the real metrics rather than as a literal, so the `p99_seconds` and
// `sum_seconds` fields the wire carries are filled in by the function that fills
// them in production. Two classes are deliberately not exercised: `observe` has
// the same shape, and the schema is per-entry.
func addonPerformanceSample() observability.AddonPerformance {
	m := observability.NewMetrics()
	for _, d := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond} {
		m.ObserveAddonRedirect("contract", "inline", d)
	}
	m.ObserveAddonRedirectKill("contract", "instantiate")
	m.ObserveAddonRedirectKill("contract", "call")
	return m.AddonPerformance()["contract"]
}

// contractAddonManifest describes a module honestly, which is what makes the
// mismatched-module replay above a refusal about the *bytes* rather than about
// anything else in the file.
//
// # It declares storage and it declares settings, and neither is decoration
//
// A schema is what makes the orphan replays possible at all: a removal leaves the
// schema standing, so without `storage.own_schema` there is nothing for
// `GET /addons/orphaned-data` to list and no name `purgeAddonData` could answer
// `200` for — both schemas would only ever be validated against an empty array and
// a pair of refusals. The settings are the same argument one document along:
// `AddonSetting` is only reachable when a manifest declares one, and a fixture
// declaring none validated the schema against `[]`.
//
// The four types are all four the host renders, because the spec's `type` is an
// enum and one value exercised is not the enum exercised. The `select` carries no
// default deliberately — that is the state the manager draws an empty option for.
func contractAddonManifest(t *testing.T, name string, module []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(module)
	m := map[string]any{
		"schema_version": 1,
		"name":           name,
		"version":        "1.0.0",
		"abi_version":    1,
		"module":         name + ".wasm",
		"sha256":         hex.EncodeToString(sum[:]),
		"failure_class":  "degrade",
		"permissions":    []string{"storage.own_schema"},
		"settings": []map[string]any{
			{"name": "endpoint", "type": "text", "default": "https://example.test"},
			{"name": "client_secret", "type": "secret"},
			{"name": "mode", "type": "select", "options": []string{"fast", "slow"}},
			{"name": "verbose", "type": "toggle", "default": "false"},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// contractAddonName is the one add-on this test installs. Named so api_test.go's
// role cleanup and this file cannot come to disagree about it.
const contractAddonName = "contract"
