//go:build integration

package integration

import (
	"net/http"
	"net/url"
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
