package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// M69.5. The sign-in page is the one page every visitor with an account meets,
// and this milestone is the first time an add-on's own data reaches a page drawn
// before anybody has authenticated. Both halves are asserted here: what a stock
// instance renders, byte for byte, and what an add-on may and may not put on it.

// signInLink is the shape internal/httpx hands the template. Written out rather
// than imported, because this package depends on nothing but the standard library
// and a template's contract is the field names it reads.
type signInLink struct {
	Addon string
	Label string
	Href  string
}

// builtStylesheetURL matches the one URL on this page whose fingerprint hashes a
// file that is not in the tree.
//
// F349, M69.5. `internal/ui/static/css/app.css` is built by `make css` and
// gitignored (.gitignore:25), so the `?v=` token the layout emits for it is a
// property of a build rather than of this template: the golden was captured
// against one machine's stylesheet and CI, building its own, disagreed on a page
// that was byte-identical in every other respect. This one token is normalized
// out of the comparison below and nothing else is — every other `?v=` on the
// page hashes a committed file, so a change to any of them still fails here.
var builtStylesheetURL = regexp.MustCompile(`/static/css/app\.css\?v=[A-Za-z0-9_-]+`)

// normalizedStylesheetURL is what the golden carries in that position, so a
// reader of testdata/login_stock.html can see the substitution rather than
// having to infer it from a hash that looks real and is not compared.
const normalizedStylesheetURL = "/static/css/app.css?v=FINGERPRINT"

// withoutStylesheetFingerprint normalizes that token, and fails unless there was
// exactly one to normalize.
//
// The count is the guard on the loosening. A comparison that skips part of a page
// is only as good as its certainty about which part: if the layout stopped
// emitting a fingerprinted stylesheet, or this pattern stopped matching the one it
// emits, the substitution would become a no-op on both sides and the test would go
// on passing while asserting something narrower than it claims. Counting turns
// that into a failure.
func withoutStylesheetFingerprint(t *testing.T, page string) string {
	t.Helper()
	if n := len(builtStylesheetURL.FindAllString(page, -1)); n != 1 {
		t.Fatalf("the page carries %d fingerprinted stylesheet URLs, want exactly 1 "+
			"— the normalization below would assert less than it says", n)
	}
	return builtStylesheetURL.ReplaceAllString(page, normalizedStylesheetURL)
}

// TestAStockSignInPageIsUnchanged is the milestone's no-op claim, held against
// the bytes.
//
// The golden was captured from this template as it stood before M69.5 touched it.
// An instance with no add-ons directory, or with no installed module holding
// `session.mint`, hands this page an empty list — and the cost of getting this
// page wrong is the whole product's front door, so the assertion is byte equality
// rather than a search for what should be absent.
//
// Byte equality with one stated exception: the built stylesheet's fingerprint,
// for the reason on builtStylesheetURL above. Everything an add-on could put on
// this page is still compared, because everything an add-on could put on it is a
// byte the template writes.
func TestAStockSignInPageIsUnchanged(t *testing.T) {
	raw, err := os.ReadFile("testdata/login_stock.html")
	if err != nil {
		t.Fatal(err)
	}
	want := withoutStylesheetFingerprint(t, string(raw))
	for _, tc := range []struct {
		name string
		data map[string]any
	}{
		{"no add-on host at all", nil},
		{"a host that offers nothing", map[string]any{"SignInLinks": []signInLink{}}},
		{"a nil slice", map[string]any{"SignInLinks": []signInLink(nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withoutStylesheetFingerprint(t, renderPage(t, "login", tc.data))
			if got != want {
				t.Errorf("the sign-in page changed for an instance that runs no "+
					"add-ons\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestAnOfferedSignInLinkIsDrawn(t *testing.T) {
	out := renderPage(t, "login", map[string]any{"SignInLinks": []signInLink{
		{Addon: "oidc", Label: "Sign in with Contoso", Href: "/addons/oidc/start"},
		{Addon: "saml", Label: "Staff single sign-on", Href: "/addons/saml/go"},
	}})
	for _, want := range []string{
		`href="/addons/oidc/start"`, "Sign in with Contoso",
		`href="/addons/saml/go"`, "Staff single sign-on",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the sign-in page does not carry %q", want)
		}
	}
	// The order it was given, which is the host's. The template sorts nothing and
	// must not: internal/addon decides the order, and a second opinion here would
	// be a place for one to drift.
	if strings.Index(out, "Sign in with Contoso") > strings.Index(out, "Staff single sign-on") {
		t.Error("the template reordered the links it was given")
	}
}

// A label is an add-on author's string on an unauthenticated page. It is hostile
// input and is escaped like every other value in this package.
func TestASignInLabelIsEscaped(t *testing.T) {
	out := renderPage(t, "login", map[string]any{"SignInLinks": []signInLink{
		{Addon: "evil", Label: `<script>alert(1)</script>`, Href: "/addons/evil/start"},
	}})
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Fatal("an add-on's label reached the page as markup")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("the label was neither escaped nor rendered:\n%s", out)
	}
}

// TestTheLocalSignInFormDoesNotMove is the promise an operator's instance rests
// on: whatever an add-on offers, password sign-in stays where it is, with the
// same fields in the same order. An instance whose add-on is broken must still
// let its operator in.
func TestTheLocalSignInFormDoesNotMove(t *testing.T) {
	fields := regexp.MustCompile(`<input[^>]*\bname="([a-z_]+)"`)
	names := func(page string) []string {
		form, _, ok := strings.Cut(page, "</form>")
		if !ok {
			t.Fatal("the sign-in page draws no form")
		}
		var out []string
		for _, m := range fields.FindAllStringSubmatch(form, -1) {
			out = append(out, m[1])
		}
		return out
	}

	stock := names(renderPage(t, "login", nil))
	if len(stock) == 0 {
		t.Fatal("the sign-in form carries no fields at all")
	}
	withAddon := names(renderPage(t, "login", map[string]any{"SignInLinks": []signInLink{
		{Addon: "oidc", Label: "Sign in with Contoso", Href: "/addons/oidc/start"},
	}}))
	if strings.Join(stock, ",") != strings.Join(withAddon, ",") {
		t.Errorf("the local form's fields changed when an add-on offered a link: "+
			"%v became %v", stock, withAddon)
	}
	// And the offer is below the form rather than in front of it: an add-on's
	// links are additive, and the page's own way in is the one that leads.
	page := renderPage(t, "login", map[string]any{"SignInLinks": []signInLink{
		{Addon: "oidc", Label: "Sign in with Contoso", Href: "/addons/oidc/start"},
	}})
	offer := strings.Index(page, "/addons/oidc/start")
	if offer < 0 {
		t.Fatal("the offer is not on the page at all, so its position says nothing")
	}
	if strings.Index(page, `name="password"`) > offer {
		t.Error("an add-on's link is drawn before the local form")
	}
}
