//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
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
	c.svgCodeEndpoint(linkID, slug)
	c.pngCodeEndpoint(linkID, slug)
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
