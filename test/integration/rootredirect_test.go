//go:build integration

package integration

import (
	"net/http"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The root of the link host answered 404 the moment M18 gave short links a
// hostname of their own: /{alias} does not match "/", and the dashboard routes
// that used to answer there moved to the other host.
func TestLinkHostRootRedirects(t *testing.T) {
	f := newSplit(t)

	// Unset is the default and stays 404 — no placeholder page, nothing that
	// says what this instance is.
	if resp := f.get(linkHost, "/"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured root = %d, want 404", resp.StatusCode)
	}

	if _, err := f.links.SetRootRedirect(t.Context(), f.owner, "https://example.com/home"); err != nil {
		t.Fatal(err)
	}

	resp := f.get(linkHost, "/")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("configured root = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/home" {
		t.Errorf("Location = %q", got)
	}
	// An intermediary holding this defeats the point of it being changeable.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing on the redirect tree")
	}

	// The change is visible immediately. Waiting out a TTL would mean an
	// operator reloading the page they just configured and seeing the old one.
	if _, err := f.links.SetRootRedirect(t.Context(), f.owner, "https://example.com/second"); err != nil {
		t.Fatal(err)
	}
	if got := f.get(linkHost, "/").Header.Get("Location"); got != "https://example.com/second" {
		t.Errorf("after update Location = %q; the cached value was not invalidated", got)
	}

	// Clearing restores the 404.
	if _, err := f.links.SetRootRedirect(t.Context(), f.owner, ""); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(linkHost, "/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("cleared root = %d, want 404", resp.StatusCode)
	}
}

// The dashboard host keeps its own root. A root redirect must not follow the
// setting onto the host where "/" is the product.
func TestRootRedirectDoesNotAffectTheDashboardHost(t *testing.T) {
	f := newSplit(t)
	if _, err := f.links.SetRootRedirect(t.Context(), f.owner, "https://example.com/home"); err != nil {
		t.Fatal(err)
	}

	resp := f.get(appHost, "/")
	if resp.StatusCode == http.StatusFound {
		if loc := resp.Header.Get("Location"); loc == "https://example.com/home" {
			t.Fatal("the dashboard host root followed the link domain's redirect; " +
				"the dashboard would be unreachable")
		}
	}
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusFound {
		t.Errorf("dashboard root = %d; expected the dashboard, not %d", resp.StatusCode, resp.StatusCode)
	}
}

// The destination is validated exactly as a link's is. Skipping that here would
// be a cleaner SSRF than the one the validator exists to prevent: reaching it
// needs no link and no alias, only the bare hostname.
func TestRootRedirectRefusesDangerousDestinations(t *testing.T) {
	f := newSplit(t)

	for _, tc := range []struct{ name, url string }{
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"private range", "http://10.0.0.5/"},
		{"javascript scheme", "javascript:alert(1)"},
		{"file scheme", "file:///etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.links.SetRootRedirect(t.Context(), f.owner, tc.url); err == nil {
				t.Errorf("%s was accepted as a root redirect", tc.url)
			}
		})
	}
}

// It is a domains.write decision, not a links one: this is where every visitor
// who trims a short link back to its domain ends up.
func TestRootRedirectNeedsDomainsWrite(t *testing.T) {
	f := newSplit(t)

	editor := f.identityWithout(link.PermDomainsWrite)
	if _, err := f.links.SetRootRedirect(t.Context(), editor, "https://example.com/x"); err == nil {
		t.Fatal("an identity without domains.write set the root redirect")
	}

	// Reading is not gated the same way: the value is public to anyone who
	// visits the bare domain.
	if _, err := f.links.DomainSettings(t.Context(), editor); err != nil {
		t.Errorf("reading domain settings needed more than links.read: %v", err)
	}
}
