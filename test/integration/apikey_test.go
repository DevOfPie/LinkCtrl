//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
)

// testPepper is the HMAC pepper for the test key service. Length matters: the
// service refuses anything below the configuration floor.
var testPepper = []byte("integration-test-pepper-32-bytes-min")

// doWith issues a request with an explicit client and optional bearer token.
func (f *apiFixture) doWith(client *http.Client, token, method, path string, body any) *http.Response {
	f.t.Helper()

	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(f.t.Context(), method, f.server.URL+path, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// doWithKey authenticates with an API key on a client that has no cookie jar,
// so the key is provably the only credential in play.
func (f *apiFixture) doWithKey(token, method, path string, body any) *http.Response {
	f.t.Helper()
	return f.doWith(&http.Client{}, token, method, path, body)
}

type createdKey struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Prefix string   `json:"prefix"`
	Scopes []string `json:"scopes"`
	Key    string   `json:"key"`
}

// createKey mints a key through the API as the signed-in user.
func (f *apiFixture) createKey(name string, scopes ...string) createdKey {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": name, "scopes": scopes,
	})
	if resp.StatusCode != http.StatusCreated {
		body := map[string]any{}
		f.decode(resp, &body)
		f.t.Fatalf("create key returned %d: %v", resp.StatusCode, body)
	}
	var out createdKey
	f.decode(resp, &out)
	return out
}

func TestAPIKeyLifecycle(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("ci-deploy", "links.read", "links.create")

	if key.Key == "" {
		t.Fatal("create returned no key; the token is only available once and this was it")
	}
	if len(key.Key) <= auth.APIKeyPrefixLength || key.Key[:auth.APIKeyPrefixLength] != key.Prefix {
		t.Errorf("key %q does not begin with the returned prefix %q", key.Key, key.Prefix)
	}

	// The secret must not be recoverable afterwards, from the list or from the
	// database. Only the HMAC is stored.
	resp := f.do(http.MethodGet, "/api/v1/api-keys", nil)
	var list struct {
		Items []struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Prefix string   `json:"prefix"`
			Scopes []string `json:"scopes"`
			Key    string   `json:"key"`
		} `json:"items"`
	}
	f.decode(resp, &list)

	if len(list.Items) != 1 {
		t.Fatalf("list returned %d keys, want 1", len(list.Items))
	}
	if list.Items[0].Key != "" {
		t.Error("the key list disclosed a token; only the hash is stored, so this cannot be right")
	}
	if list.Items[0].ID != key.ID || list.Items[0].Prefix != key.Prefix {
		t.Errorf("listed key = %+v, want id %s prefix %s", list.Items[0], key.ID, key.Prefix)
	}

	// What is stored is the HMAC and nothing else. The row is compared as text
	// so the check covers every column, not just the one that is supposed to
	// hold the credential.
	secret := key.Key[auth.APIKeyPrefixLength+1:]
	var stored []byte
	var rowText string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT key_hash, api_keys::text FROM api_keys WHERE prefix = $1`,
		key.Prefix).Scan(&stored, &rowText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rowText, secret) {
		t.Error("the stored row contains the secret; a database dump would hand over live keys")
	}
	if !bytes.Equal(stored, auth.APIKeyHash(testPepper, key.Prefix, secret)) {
		t.Error("key_hash is not HMAC(pepper, prefix, secret)")
	}

	// The key authenticates, and its scopes are what it can do.
	resp = f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("listing links with a links.read key returned %d, want 200", resp.StatusCode)
	}

	resp2 := f.doWithKey(key.Key, http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://example.com/from-a-key"})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("creating a link with a links.create key returned %d, want 201", resp2.StatusCode)
	}

	// Revocation takes effect on the next request; nothing about a key is
	// cached, which is the reason the check lives in the verification query.
	resp3 := f.do(http.MethodDelete, "/api/v1/api-keys/"+key.ID, nil)
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke returned %d, want 204", resp3.StatusCode)
	}

	resp4 := f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
	defer func() { _ = resp4.Body.Close() }()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked key returned %d, want 401", resp4.StatusCode)
	}
}

func TestAPIKeyIsLimitedToItsScopes(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Read-only, minted by an owner who can do everything. The key must not
	// inherit the owner's power.
	key := f.createKey("read-only", "links.read")

	resp := f.doWithKey(key.Key, http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://example.com/denied"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("creating a link with a read-only key returned %d, want 403", resp.StatusCode)
	}

	// /me reports the effective permissions, which for a key are its scopes.
	resp2 := f.doWithKey(key.Key, http.MethodGet, "/api/v1/me", nil)
	var me struct {
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	f.decode(resp2, &me)

	if len(me.Permissions) != 1 || me.Permissions[0] != "links.read" {
		t.Errorf("permissions = %v, want exactly [links.read]", me.Permissions)
	}
	if me.Role != "owner" {
		t.Errorf("role = %q; the key still acts as its owner, only with fewer permissions", me.Role)
	}
}

func TestAPIKeyLosesPermissionsWithItsOwner(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("was-powerful", "links.read", "links.create")

	// Demote the owner to viewer. The key's scopes are unchanged, but the
	// intersection with the role is recomputed on every request, so the key
	// weakens immediately rather than surviving the demotion.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE memberships SET role_id = (SELECT id FROM roles WHERE slug='viewer' AND organization_id IS NULL)`,
	); err != nil {
		t.Fatal(err)
	}

	resp := f.doWithKey(key.Key, http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://example.com/nope"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("creating a link after the owner was demoted returned %d, want 403", resp.StatusCode)
	}

	// links.read survives, because a viewer still holds it.
	resp2 := f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("listing links returned %d, want 200: a viewer still holds links.read", resp2.StatusCode)
	}
}

func TestAPIKeyCannotManageAPIKeys(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// The scope cannot even be requested: a key able to mint keys would make
	// revoking the original pointless.
	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "self-replicating", "scopes": []string{"links.read", "apikeys.write"},
	})
	var problem struct {
		Status int `json:"status"`
		Errors []struct {
			Field, Code string
		} `json:"errors"`
	}
	f.decode(resp, &problem)

	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Code != "not_delegable" {
		t.Errorf("errors = %+v, want one not_delegable on scopes", problem.Errors)
	}

	// And the endpoints refuse a key regardless, including the password change
	// that would otherwise let a leaked key lock its owner out.
	key := f.createKey("ordinary", "links.read")
	for _, e := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/api-keys", nil},
		{http.MethodPost, "/api/v1/api-keys", map[string]any{
			"name": "second", "scopes": []string{"links.read"},
		}},
		{http.MethodDelete, "/api/v1/api-keys/" + key.ID, nil},
		{http.MethodPost, "/api/v1/auth/password", map[string]any{
			"current_password": "a-sufficiently-long-password",
			"new_password":     "another-sufficiently-long-password",
		}},
	} {
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			resp := f.doWithKey(key.Key, e.method, e.path, e.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 for an API key", resp.StatusCode)
			}
		})
	}
}

func TestAPIKeyScopeMustBeHeldByItsCreator(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Take links.delete away from the owner role, then ask for it as a scope.
	// Minting a key must not be a way to grant yourself a permission your role
	// does not include.
	if _, err := f.pool.Exec(t.Context(), `
		DELETE FROM role_permissions
		 WHERE role_id = (SELECT id FROM roles WHERE slug='owner' AND organization_id IS NULL)
		   AND permission_id = (SELECT id FROM permissions WHERE slug='links.delete')`,
	); err != nil {
		t.Fatal(err)
	}

	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "escalation", "scopes": []string{"links.delete"},
	})
	var problem struct {
		Status int `json:"status"`
		Errors []struct {
			Field, Code string
		} `json:"errors"`
	}
	f.decode(resp, &problem)

	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Code != "not_held" {
		t.Errorf("errors = %+v, want one not_held on scopes", problem.Errors)
	}
}

func TestAPIKeyRejectsUnknownScopeAndEmptyScopes(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	cases := map[string]struct {
		scopes []string
		code   string
	}{
		// A typo must fail loudly: a key silently authorizing nothing looks
		// like a broken server rather than a bad request.
		"typo":  {scopes: []string{"links.reed"}, code: "unknown"},
		"empty": {scopes: []string{}, code: "required"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
				"name": "bad-scopes", "scopes": tc.scopes,
			})
			var problem struct {
				Status int `json:"status"`
				Errors []struct {
					Field, Code string
				} `json:"errors"`
			}
			f.decode(resp, &problem)

			if problem.Status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", problem.Status)
			}
			if len(problem.Errors) != 1 || problem.Errors[0].Code != tc.code {
				t.Errorf("errors = %+v, want one %s on scopes", problem.Errors, tc.code)
			}
		})
	}
}

func TestInvalidAPIKeysAreRejected(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// A well-formed key whose prefix exists nowhere, so the shape check passes
	// and the lookup is what fails. Built by altering one character of a real
	// prefix, keeping the alphabet valid.
	issued := f.createKey("real", "links.read")
	replacement := "z"
	if issued.Prefix[len(issued.Prefix)-1] == 'z' {
		replacement = "a"
	}
	forged := issued.Prefix[:len(issued.Prefix)-1] + replacement +
		issued.Key[auth.APIKeyPrefixLength:]

	cases := map[string]string{
		"garbage":        "not-a-key-at-all",
		"unknown prefix": forged,
		"right prefix, wrong secret": issued.Prefix + "_" +
			"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}

	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			resp := f.doWithKey(token, http.MethodGet, "/api/v1/links", nil)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got == "" {
				t.Error("no WWW-Authenticate header on a 401 from a bearer token")
			}

			var problem struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			f.decode(resp, &problem)
			// One response for every reason, so a leaked key cannot be probed
			// for whether it is merely revoked.
			if problem.Type != "https://linkctrl.dev/problems/invalid-api-key" {
				t.Errorf("problem type = %q, want invalid-api-key", problem.Type)
			}
		})
	}
}

func TestExpiredAPIKeyIsRejected(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("short-lived", "links.read")

	// Expiry in the past is refused at creation, so the row is backdated
	// instead. What is under test is enforcement, not validation.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE prefix = $1`,
		key.Prefix); err != nil {
		t.Fatal(err)
	}

	resp := f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired key returned %d, want 401", resp.StatusCode)
	}
}

func TestAPIKeyCreationRejectsPastExpiry(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "already-dead", "scopes": []string{"links.read"}, "expires_at": past,
	})
	var problem struct {
		Status int `json:"status"`
		Errors []struct {
			Field, Code string
		} `json:"errors"`
	}
	f.decode(resp, &problem)

	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "expires_at" {
		t.Errorf("errors = %+v, want one on expires_at", problem.Errors)
	}
}

// A bearer token beats a cookie on the same request. Otherwise a browser with
// a live session could not be used to test a deliberately weak key, and worse,
// a cookie could silently upgrade a key's permissions.
func TestBearerTokenWinsOverASessionCookie(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("read-only", "links.read")

	// f.client carries the owner's session cookie.
	resp := f.doWith(f.client, key.Key, http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://example.com/should-not-exist"})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403; the session cookie overrode the key's scopes",
			resp.StatusCode)
	}
}

func TestAPIKeyUsageIsRecordedAsynchronously(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("tracked", "links.read")

	// Nothing is written on the authentication path itself.
	var beforeFlush *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT last_used_at FROM api_keys WHERE prefix = $1`, key.Prefix).Scan(&beforeFlush); err != nil {
		t.Fatal(err)
	}
	if beforeFlush != nil {
		t.Fatalf("last_used_at was %s before the key was ever used", beforeFlush)
	}

	for range 3 {
		resp := f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
		_ = resp.Body.Close()
	}

	var duringUse *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT last_used_at FROM api_keys WHERE prefix = $1`, key.Prefix).Scan(&duringUse); err != nil {
		t.Fatal(err)
	}
	if duringUse != nil {
		t.Error("last_used_at was written on the request path; it is supposed to be buffered")
	}

	if err := f.keys.FlushUsage(t.Context()); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}

	var afterFlush *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT last_used_at FROM api_keys WHERE prefix = $1`, key.Prefix).Scan(&afterFlush); err != nil {
		t.Fatal(err)
	}
	if afterFlush == nil {
		t.Fatal("last_used_at is still null after a flush")
	}
	if time.Since(*afterFlush) > time.Minute {
		t.Errorf("last_used_at = %s, which is not the timestamp of a request made just now", afterFlush)
	}
}

func TestRevokingAnotherUsersKeyIsNotFound(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	key := f.createKey("owners-key", "links.read")

	// A second account, whose own role also permits key management. Made through
	// the service rather than through POST /auth/register: since M29 that
	// endpoint mails a verification link and creates nothing, and what this test
	// needs is a second account, not a second registration.
	if _, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "other@example.com", Name: "Other", Password: "a-sufficiently-long-password",
	}); err != nil {
		t.Fatalf("register the second account: %v", err)
	}
	resp := f.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "other@example.com", "password": "a-sufficiently-long-password",
	})
	_ = resp.Body.Close()

	// Not found rather than forbidden: an id must not be probeable for
	// existence by someone who does not own it.
	resp = f.do(http.MethodDelete, "/api/v1/api-keys/"+key.ID, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when revoking someone else's key", resp.StatusCode)
	}

	// The key still works, which is the thing that would actually be broken.
	resp2 := f.doWithKey(key.Key, http.MethodGet, "/api/v1/links", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("the owner's key returned %d after a stranger tried to revoke it, want 200",
			resp2.StatusCode)
	}
}
