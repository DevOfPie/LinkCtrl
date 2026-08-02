//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Query forwarding, end to end: the write surface sets it, the redirect honours
// it, and the default is off. The read path existed unexercised for six
// milestones — the column could only be set by hand-written SQL — so this test
// covers the whole loop, not just the new field.
func TestQueryForwarding(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	// Default off: the destination's own URL is the whole answer.
	plain := f.createLink(map[string]any{"url": "https://example.com/plain"})
	resp := f.get("/" + plain + "?utm_source=news&x=1")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://example.com/plain" {
		t.Errorf("Location = %q; with forwarding off the visitor's query must be dropped", loc)
	}
	_ = resp.Body.Close()

	// On at creation.
	fwd := f.createLink(map[string]any{
		"url": "https://example.com/land?keep=yes", "alias": "fwdon", "forward_query": true,
	})
	resp = f.get("/" + fwd + "?utm_source=news&keep=no")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("utm_source") != "news" {
		t.Errorf("Location %q lost the forwarded parameter", loc)
	}
	// The destination's own parameters win on conflict — that is the documented
	// contract, and the piece a naive merge gets backwards.
	if q.Get("keep") != "yes" {
		t.Errorf("Location %q let the visitor override the destination's own parameter", loc)
	}
	_ = resp.Body.Close()

	// Toggled off again via PATCH, and the change reaches the redirect (the
	// update invalidates the cached snapshot).
	var created struct{ ID string }
	r2 := f.do(http.MethodGet, "/api/v1/links?search=fwdon", nil)
	var page struct {
		Items []struct {
			ID           string `json:"id"`
			ForwardQuery bool   `json:"forward_query"`
		} `json:"items"`
	}
	f.decode(r2, &page)
	if len(page.Items) != 1 {
		t.Fatalf("found %d links for fwdon, want 1", len(page.Items))
	}
	if !page.Items[0].ForwardQuery {
		t.Error("the list response does not carry forward_query=true")
	}
	created.ID = page.Items[0].ID

	r3 := f.do(http.MethodPatch, "/api/v1/links/"+created.ID, map[string]any{
		"forward_query": false,
	})
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("patch returned %d", r3.StatusCode)
	}
	var updated struct {
		ForwardQuery bool `json:"forward_query"`
	}
	f.decode(r3, &updated)
	if updated.ForwardQuery {
		t.Error("PATCH forward_query=false did not stick in the response")
	}

	resp = f.get("/fwdon?utm_source=news")
	if loc := resp.Header.Get("Location"); loc != "https://example.com/land?keep=yes" {
		t.Errorf("Location = %q after toggling forwarding off; the cached snapshot "+
			"must have been invalidated", loc)
	}
	_ = resp.Body.Close()
}

// Deep-link path forwarding (M33), end to end, and the half that is easy to get
// wrong is the *off* half: with forwarding off, everything under an alias must
// be as blank as an alias that was never created.
func TestDeepLinkPathForwarding(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	// --- forwarding off, which is every link that already exists -------------

	off := f.createLink(map[string]any{"url": "https://example.com/plainpath", "alias": "pathoff"})

	resp := f.get("/" + off)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound ||
		loc != "https://example.com/plainpath" {
		t.Errorf("the bare alias answered %d %q; registering the multi-segment "+
			"pattern must not disturb the single-segment one", resp.StatusCode, loc)
	}
	_ = resp.Body.Close()

	// The claim: a miss, indistinguishable from an alias nobody made. Not a
	// redirect to the bare destination, which would make one alias answer every
	// URL beneath itself for every link on the instance.
	resp = f.get("/" + off + "/deep/segments")
	assertCustomNotFound(t, resp, "a multi-segment request to a link with path forwarding off")

	// And an unknown alias with a path is the same answer, byte for byte in
	// shape — so the 404 reveals nothing about which aliases exist.
	resp = f.get("/nosuchaliashere/deep/segments")
	assertCustomNotFound(t, resp, "a multi-segment request to an alias that does not exist")

	// A trailing slash has answered 404 since the redirect tree existed, and
	// this milestone does not move it. It is now handled by a pattern that did
	// not exist before, which is exactly why it is asserted here as well as in
	// TestRedirectMatrix: the answer has to survive the route changing beneath
	// it.
	resp = f.get("/" + off + "/")
	assertCustomNotFound(t, resp, "a trailing slash on a link with path forwarding off")

	// --- forwarding on -------------------------------------------------------

	on := f.createLink(map[string]any{
		"url": "https://example.com/docs", "alias": "pathon", "forward_path": true,
	})

	for _, tc := range []struct{ req, want string }{
		{"/" + on + "/api/quickstart", "https://example.com/docs/api/quickstart"},
		{"/" + on + "/api/", "https://example.com/docs/api/"},
		// With forwarding on the trailing slash is the top of the forwarded
		// subtree rather than a special case — the empty remainder joins to the
		// destination's own root.
		{"/" + on + "/", "https://example.com/docs/"},
		// Not re-encoded on the way past.
		{"/" + on + "/caf%C3%A9", "https://example.com/docs/caf%C3%A9"},
		// An encoded '?' is a path byte, not the start of a query.
		{"/" + on + "/a%3Fb", "https://example.com/docs/a%3Fb"},
		// Nothing after the alias, so nothing to append.
		{"/" + on, "https://example.com/docs"},
	} {
		r := f.get(tc.req)
		if r.StatusCode != http.StatusFound {
			t.Errorf("GET %s = %d, want 302", tc.req, r.StatusCode)
		}
		if loc := r.Header.Get("Location"); loc != tc.want {
			t.Errorf("GET %s → %q, want %q", tc.req, loc, tc.want)
		}
		_ = r.Body.Close()
	}

	// Traversal is refused rather than resolved. ServeMux cleans the literal
	// spelling away before the handler sees it, so the encoded one is the only
	// one that arrives — and a browser resolves it, which is why it cannot be
	// forwarded.
	resp = f.get("/" + on + "/%2e%2e/%2e%2e/admin")
	assertCustomNotFound(t, resp, "an encoded dot-segment traversal")

	// Both halves of the URL at once, applied in the order a reader would
	// expect: the path is joined, then the query is merged onto the result.
	both := f.createLink(map[string]any{
		"url": "https://example.com/shop?ref=house", "alias": "pathboth",
		"forward_path": true, "forward_query": true,
	})
	resp = f.get("/" + both + "/boots/42?utm_source=news&ref=visitor")
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/shop/boots/42" {
		t.Errorf("Location path = %q, want /shop/boots/42", loc.Path)
	}
	if loc.Query().Get("utm_source") != "news" {
		t.Errorf("Location %q lost the forwarded query", loc)
	}
	if loc.Query().Get("ref") != "house" {
		t.Errorf("Location %q let the visitor override the destination's own parameter", loc)
	}
	_ = resp.Body.Close()

	// --- toggled off again, and the cached snapshot must not outlive it ------

	var page struct {
		Items []struct {
			ID          string `json:"id"`
			ForwardPath bool   `json:"forward_path"`
		} `json:"items"`
	}
	f.decode(f.do(http.MethodGet, "/api/v1/links?search=pathon", nil), &page)
	if len(page.Items) != 1 {
		t.Fatalf("found %d links for pathon, want 1", len(page.Items))
	}
	if !page.Items[0].ForwardPath {
		t.Error("the list response does not carry forward_path=true")
	}

	r3 := f.do(http.MethodPatch, "/api/v1/links/"+page.Items[0].ID, map[string]any{
		"forward_path": false,
	})
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("patch returned %d", r3.StatusCode)
	}
	var updated struct {
		ForwardPath bool `json:"forward_path"`
	}
	f.decode(r3, &updated)
	if updated.ForwardPath {
		t.Error("PATCH forward_path=false did not stick in the response")
	}

	resp = f.get("/" + on + "/api/quickstart")
	assertCustomNotFound(t, resp, "a deep link after path forwarding was switched off")
}

// assertCustomNotFound checks that a refusal is the product's own 404 rather
// than ServeMux's default one — which is what an unregistered pattern produces,
// and which is how "the miss path applies" quietly stops being true.
func assertCustomNotFound(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("%s returned %d, want 404", what, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("%s returned Content-Type %q, want the custom HTML 404 page", what, ct)
	}
	if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("%s returned X-Robots-Tag %q, want noindex", what, got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<html") {
		t.Errorf("%s returned a body that is not the custom page: %q", what, string(body))
	}
}
