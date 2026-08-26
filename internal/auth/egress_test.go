package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// outboundFromAnAuthenticationPath matches a symbol that would let one of these
// packages reach the network on its own.
//
// The same pattern internal/link's TestThisPackageOpensNoSocketOfItsOwn uses, and
// blunt in the same way on purpose: a false positive is a line somebody justifies
// in a comment, and a false negative is a credential path that dials.
var outboundFromAnAuthenticationPath = regexp.MustCompile(
	`\b(?:` +
		`http\.(?:Get|Head|Post|PostForm|Do|NewRequest|NewRequestWithContext|Client|DefaultClient|` +
		`DefaultTransport|Transport)` +
		`|httputil\.` +
		`|net\.(?:Dial|DialTimeout|DialIP|DialTCP|DialUDP|Dialer|Resolver|LookupHost|LookupIP|LookupAddr|LookupCNAME)` +
		`|exec\.(?:Command|CommandContext)` +
		`|smtp\.|websocket\.` +
		`)`)

// authenticationPackages are the directories this scan covers, relative to this
// one.
//
// **internal/auth and internal/httpx, because those are the packages M53 touches**
// — m53.md says so, and the list is what makes the milestone's *no outbound
// connection* bullet enforceable rather than reviewed.
//
// **internal/addon joined at M65**, and it had to: `session.mint` makes that
// package a place where somebody is signed in, so the mechanism M53 built to make
// *nothing on an authentication path reaches the network* enforceable had stopped
// covering all of it. The package was clean when it was added, which is the
// cheapest moment to add one — a list extended only when it goes red is a list
// that documents defects rather than preventing them. Note what this does and does
// not cover: it is about **this host's own Go source**, not about what a guest can
// reach. A module gets no socket because it gets no import that opens one, which
// is the ABI's claim and is asserted elsewhere; this is the claim that the code
// standing between an assertion and a session does not dial either.
//
// **The mechanism is a new scan, not an existing one, and that is the point.** An
// earlier draft of the milestone cited
// `TestThisPackageOpensNoSocketOfItsOwn` in internal/link, which reads
// `os.ReadDir(".")` and therefore only ever sees internal/link. M53's code lands
// here and in internal/httpx, which that test cannot reach, so it would have
// passed while the thing it was cited for went unchecked. Corrected 2026-08-07,
// from review: the template requires enforcement named as a mechanism — a test
// that fails, not review vigilance — and the named test could not fail.
var authenticationPackages = []string{
	".",
	filepath.Join("..", "httpx"),
	filepath.Join("..", "addon"),
}

// TestTheSecondFactorOpensNoSocket.
//
// m53.md's third refusal: *no outbound connection*. `docs/SECURITY.md`'s egress
// row stays at whatever count M55 leaves it at, and this milestone adds nothing to
// it — TOTP is arithmetic over a clock and a shared secret, so there is nobody to
// ask. A second factor that phoned a verification service would be the fifth thing
// leaving this product, arriving without a row in the disclosure the operator
// reads.
//
// # Two exemptions, both named rather than pattern-matched
//
// internal/httpx is an HTTP *server* package. `http.Client`, `http.Transport` and
// the dialling verbs are what a client uses and are absent from it; the pattern
// above is written not to match `http.Handler`, `http.Request` or
// `http.ResponseWriter`, which is why a server package can be scanned by a rule
// designed for a judge. Two files there are exempt by name below, each for a
// reason that is about the test rather than about the traffic:
//
//   - `redirect_hosts.go` — the redirect tree's host cache. Not M53's, older than
//     it, and what it does is read a table.
//
// If the scan grows a third exemption, that is worth arguing about rather than
// adding.
//
// # The exemption M68.5 argued for, and what the claim narrowed to
//
// `internal/addon/fetch.go` dials, on purpose, and it is the first file in any of
// these three packages that does. That makes this test's sentence — *nothing on
// an authentication path in this product reaches the network* — false as written,
// and the honest response is to say what it is now instead of quietly widening a
// pattern.
//
// **What it asserted, and still does:** none of this product's own authentication
// code reaches the network of its own accord. TOTP is arithmetic, a session is a
// row, and a password is a hash — there is nobody to ask, and a verification
// service arriving with a feature is exactly what this scan is for.
//
// **What is now true beside it:** an add-on may reach outward, because an operator
// declared a permission and named an origin (D364). That is not this product
// dialling on an authentication path; it is a capability an operator granted to
// somebody else's code, and it will run on an authentication path when the OIDC
// add-on lands. The two claims are different and the second does not weaken the
// first — but only because the egress is confined to **one file**, which is what
// this exemption asserts. A second file in internal/addon that dials would be a
// second door with its own bounds, and it would fail here rather than joining a
// list.
//
// `docs/SECURITY.md`'s egress row carries the disclosure, and it is the sixth
// connection counted there.
func TestTheSecondFactorOpensNoSocket(t *testing.T) {
	// Files exempt from the scan, by path relative to the package being walked.
	// One, and it is the diff-with-a-name-beside-it this set was kept empty for:
	// M68.5's outbound fetch is a declared capability an operator configured, and
	// it is confined to this file so that "this product's own authentication code
	// does not dial" stays checkable. See the second half of the comment above.
	exempt := map[string]bool{
		"../addon/fetch.go": true,
	}

	var scanned int
	for _, dir := range authenticationPackages {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(dir, name))
			if exempt[rel] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: names come from a fixed list of this repository's own directories.
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			scanned++
			for i, line := range strings.Split(string(b), "\n") {
				if m := outboundFromAnAuthenticationPath.FindString(line); m != "" {
					t.Errorf("%s:%d uses %s. This product's own authentication code does not "+
						"reach the network: TOTP is arithmetic over a clock and a shared "+
						"secret, and a second factor that asked somebody would be a channel "+
						"docs/SECURITY.md's egress row does not name. An add-on's outbound "+
						"request is the one exception and it lives in addon/fetch.go, which "+
						"is exempt by name — a second file that dials is a second door and "+
						"belongs in that row before it belongs here.",
						rel, i+1, m)
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no files; the walk is broken rather than the packages clean")
	}
	// The exemption is a claim about the tree and is checked as one: a name that
	// stops matching a file is an exemption that has quietly stopped exempting
	// anything, and the scan would then be passing because the door moved rather
	// than because it is shut.
	for rel := range exempt {
		if _, err := os.Stat(rel); err != nil {
			t.Errorf("%s is exempt from this scan and is not there: %v", rel, err)
		}
	}

	// A floor rather than an exact count, because both packages grow. Low enough
	// that it never needs revising and high enough that a walk which found one
	// file fails here instead of passing.
	if scanned < 40 {
		t.Fatalf("scanned only %d files across %v; internal/httpx alone has more than "+
			"that, so the walk is reading the wrong directories", scanned, authenticationPackages)
	}
}

// TestTheScanWouldActuallyFail.
//
// The scan above is the milestone's enforcement mechanism, and a scan that cannot
// fail enforces nothing — which is exactly the defect the milestone's own
// correction records about the test it used to cite. This runs the pattern against
// a line that should trip it, so the regexp is shown to work rather than assumed
// to.
func TestTheScanWouldActuallyFail(t *testing.T) {
	for _, line := range []string{
		`resp, err := http.Get("https://otp.example.com/verify")`,
		`c := &http.Client{Timeout: time.Second}`,
		`addrs, _ := net.LookupHost(host)`,
		`conn, _ := net.Dial("tcp", "10.0.0.1:25")`,
	} {
		if !outboundFromAnAuthenticationPath.MatchString(line) {
			t.Errorf("the scan does not match %q, so it would not have caught it", line)
		}
	}
	// And the server verbs a handler package is full of are not matched, or the
	// scan would be unusable on internal/httpx and would be silenced rather than
	// obeyed.
	for _, line := range []string{
		`func (h *Web) MFAPage(w http.ResponseWriter, r *http.Request) {`,
		`app.Handle("POST /login/code", guard(http.HandlerFunc(web.MFAChallengeSubmit)))`,
		`http.SetCookie(w, NewSessionCookie(res.Token, h.Config.SecureCookies, maxAge))`,
	} {
		if m := outboundFromAnAuthenticationPath.FindString(line); m != "" {
			t.Errorf("the scan matches %q on %q, which is an HTTP server verb. A scan "+
				"that fires on ordinary handler code gets exempted rather than obeyed", m, line)
		}
	}
}
