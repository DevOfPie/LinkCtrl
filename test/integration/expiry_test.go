//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// An expired link answered 410 on the redirect path while every management
// surface still called it active, because nothing ever writes 'expired' to
// links.status. The rule is derived in two places that must agree — Go for what
// is reported, SQL for what is filtered — and this is what holds them together.
func TestExpiredLinkReportsAndFiltersAsExpired(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	// Created with a future expiry, because the API refuses a past one. Backdated
	// afterwards: "the expiry passed" is a state links reach by the clock, not
	// one a client can ask for.
	alias := f.createLink(map[string]any{
		"url":        "https://example.com/campaign",
		"alias":      "expirystatus",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE links SET expires_at = now() - interval '1 hour' WHERE alias = $1`, alias); err != nil {
		t.Fatal(err)
	}

	// The redirect path was always right about this one.
	resp := f.get("/" + alias)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("redirect for an expired link = %d, want 410", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The management surface must now agree.
	if got := f.statusOfListedLink(alias, ""); got != "expired" {
		t.Errorf("listed status = %q, want %q; an operator diagnosing the 410 is told "+
			"by operations.md to check this field", got, "expired")
	}

	// And the filter has to find it, or the UI's Expired option is a control
	// that can never match a row.
	if got := f.statusOfListedLink(alias, "expired"); got != "expired" {
		t.Errorf("?status=expired did not return the expired link (got %q)", got)
	}
	if got := f.statusOfListedLink(alias, "active"); got != "" {
		t.Errorf("?status=active still returns the expired link as %q", got)
	}

	// A link whose expiry has not passed is untouched by any of this.
	live := f.createLink(map[string]any{
		"url":        "https://example.com/live",
		"alias":      "notyetexpired",
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	if got := f.statusOfListedLink(live, ""); got != "active" {
		t.Errorf("a link expiring tomorrow reports %q, want active", got)
	}
}

// Expiry outranks an archived status, matching Snapshot.Decide. If the two
// disagreed this would be the original defect in a smaller form.
func TestExpiryOutranksArchivedEverywhere(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	alias := f.createLink(map[string]any{
		"url":        "https://example.com/both",
		"alias":      "archivedexpired",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	id := f.linkIDByAlias(alias)

	resp := f.do(http.MethodPost, "/api/v1/links/"+id+"/archive", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE links SET expires_at = now() - interval '1 hour' WHERE alias = $1`, alias); err != nil {
		t.Fatal(err)
	}

	if got := f.statusOfListedLink(alias, ""); got != "expired" {
		t.Errorf("archived and expired reports %q; the resolver calls it gone, so the "+
			"management surface must call it expired", got)
	}
	if got := f.statusOfListedLink(alias, "expired"); got != "expired" {
		t.Errorf("?status=expired did not find the archived-and-expired link (got %q)", got)
	}
}

// statusOfListedLink returns the status the list endpoint reports for an alias,
// optionally under a status filter. Empty means the filter excluded it.
func (f *apiFixture) statusOfListedLink(alias, statusFilter string) string {
	f.t.Helper()
	path := "/api/v1/links?limit=100"
	if statusFilter != "" {
		path += "&status=" + statusFilter
	}
	resp := f.do(http.MethodGet, path, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("list = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	var page struct {
		Items []struct {
			Alias  string `json:"alias"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		f.t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.Alias == alias {
			return it.Status
		}
	}
	return ""
}

func (f *apiFixture) linkIDByAlias(alias string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT id::text FROM links WHERE alias = $1`, alias).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
