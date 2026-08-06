//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The override, end to end: a form post, a cookie, and the next response
// already in the chosen theme.
//
// "Already" is the point. There is no correcting script anywhere in the
// dashboard, so if the server did not render the attribute the visitor would
// get the system theme first and a flash to the chosen one — the defect this
// design makes unrepresentable rather than suppressing.
func TestThemeOverrideRoundTrips(t *testing.T) {
	f := newWeb(t)
	f.claim()

	// Nothing chosen: no attribute, so prefers-color-scheme decides.
	if body := f.body(f.get("/dashboard", nil)); strings.Contains(body, "data-theme") {
		t.Error("a fresh visitor's page carries a data-theme attribute; with no " +
			"preference stored the system setting must decide")
	}

	for _, theme := range []string{"dark", "light"} {
		f.wantRedirect(f.postForm("/theme", url.Values{
			"theme": {theme}, "next": {"/dashboard"},
		}, nil), "/dashboard")

		body := f.body(f.get("/dashboard", nil))
		if !strings.Contains(body, `data-theme="`+theme+`"`) {
			t.Errorf("after choosing %s the page does not render data-theme=%q", theme, theme)
		}
	}

	// And back to following the system, which has to be reachable: a fresh
	// visitor is on it, so a control that could not return there would make the
	// default unreachable the moment anyone touched it.
	f.wantRedirect(f.postForm("/theme", url.Values{
		"theme": {"system"}, "next": {"/dashboard"},
	}, nil), "/dashboard")
	if body := f.body(f.get("/dashboard", nil)); strings.Contains(body, "data-theme") {
		t.Error("choosing System left an attribute behind; system means the page " +
			"carries none at all")
	}
}

// The preference is per-browser, not per account, so it has to work with no
// session at all — the login page is the first page anybody sees.
func TestThemeWorksSignedOut(t *testing.T) {
	f := newWeb(t)
	// Claimed and then signed out, rather than left fresh: on an instance with
	// no users at all, /login redirects to /setup, so a fresh fixture would be
	// asserting against an empty redirect body rather than the login page.
	f.claim()
	f.body(f.postForm("/logout", url.Values{}, nil))

	resp := f.postForm("/theme", url.Values{"theme": {"dark"}, "next": {"/login"}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setting a theme while signed out returned %d, want 303", resp.StatusCode)
	}
	f.body(resp)

	body := f.body(f.get("/login", nil))
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("the login page does not honour the theme cookie; the preference " +
			"is per-browser and there is no account to hang it on")
	}
	if !strings.Contains(body, `action="/theme"`) {
		t.Error("the login page offers no way to change the appearance")
	}
}

// A cookie is attacker-supplied input like any other. An unrecognised value has
// to degrade to the system default rather than reach the attribute.
func TestUnknownThemeCookieIsIgnored(t *testing.T) {
	f := newWeb(t)
	f.claim()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/dashboard", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "linkctrl_theme", Value: `midnight" onload="alert(1)`})
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := f.body(resp)

	if strings.Contains(body, "data-theme") {
		t.Error("an unrecognised theme cookie reached the attribute; anything not " +
			"light or dark must be treated as no preference")
	}
	if strings.Contains(body, "onload=") {
		t.Fatal("a theme cookie's value was rendered into the page unescaped")
	}
}

// Posting a theme must not become an open redirect. The form carries the page
// it came from, and that value is caller-supplied.
func TestThemeRedirectStaysOnThisSite(t *testing.T) {
	f := newWeb(t)
	f.claim()

	for _, next := range []string{"//evil.example.com", "https://evil.example.com", "/\\evil"} {
		resp := f.postForm("/theme", url.Values{"theme": {"dark"}, "next": {next}}, nil)
		loc := resp.Header.Get("Location")
		f.body(resp)
		if strings.HasPrefix(loc, "//") || strings.Contains(loc, "evil.example.com") {
			t.Errorf("next=%q redirected to %q, off this site", next, loc)
		}
	}
}
