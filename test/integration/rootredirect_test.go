//go:build integration

package integration

import (
	"net/http"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
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

// The root of the instance default domain is the instance principal's, and an
// organization owner holding domains.write is refused.
//
// This test used to demote the owner to editor and assert that domains.write was
// what mattered. That is no longer the claim, and the change is F70: the
// instance default is the hostname every workspace's links are served on until
// it registers its own, and `domains.write` is a *role* permission granted to
// owner and admin — so on a multi-organization instance every organization's
// owner could repoint it, and under SIGNUP_MODE=open one registration reaches
// that. D100 moved it to `domains.write.instance`, which reaches a person only
// through instance_grants.
//
// The identity below is a full organization **owner** holding domains.write.
// That is the point: the permission it holds is the one that used to be enough.
func TestTheInstanceRootIsThePrincipalsAndNotAnOrganizationOwners(t *testing.T) {
	f := newSplit(t)

	// The fixture's owner registered first, so the setup path made them the
	// instance principal. Taking that away leaves an ordinary organization
	// owner, which is what every other account on a multi-tenant instance is.
	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM instance_grants WHERE user_id = $1`, f.owner.UserID); err != nil {
		t.Fatal(err)
	}
	owner, err := f.auth.IdentityForEmail(t.Context(), splitOwnerEmail)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Can(link.PermDomainsWrite) {
		t.Fatal("the identity lost domains.write as well; this test would prove nothing, " +
			"because being refused would no longer say anything about the instance permission")
	}
	if owner.Can(auth.PermDomainsWriteInstance) {
		t.Fatal("the identity still holds domains.write.instance after its grants were removed")
	}

	if _, err := f.links.SetRootRedirect(t.Context(), owner, "https://example.com/x"); err == nil {
		t.Error("an organization owner repointed the instance default domain's root. " +
			"That is F70: the hostname every workspace's links are served on, " +
			"administered by anybody who registered on an open instance")
	}
	if _, err := f.links.SetBotBlocking(t.Context(), owner, true, false); err == nil {
		t.Error("an organization owner changed the instance default domain's bot policy")
	}

	// Reading is not gated the same way and must not become so: the value is
	// public to anyone who visits the bare domain, and the links page shows the
	// hostname beside every link.
	if _, err := f.links.DomainSettings(t.Context(), owner); err != nil {
		t.Errorf("reading domain settings needed more than links.read: %v", err)
	}

	// And the principal, who is what this permission exists for, still can.
	grantInstanceScope(t, f.pool, f.owner.UserID, auth.PermDomainsWriteInstance)
	principal, err := f.auth.IdentityForEmail(t.Context(), splitOwnerEmail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.links.SetRootRedirect(t.Context(), principal, "https://example.com/x"); err != nil {
		t.Errorf("the holder of %s could not set the root redirect: %v",
			auth.PermDomainsWriteInstance, err)
	}
}
