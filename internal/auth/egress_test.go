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
// **The mechanism is a new scan, not an existing one, and that is the point.** An
// earlier draft of the milestone cited
// `TestThisPackageOpensNoSocketOfItsOwn` in internal/link, which reads
// `os.ReadDir(".")` and therefore only ever sees internal/link. M53's code lands
// here and in internal/httpx, which that test cannot reach, so it would have
// passed while the thing it was cited for went unchecked. Corrected 2026-08-07,
// from review: the template requires enforcement named as a mechanism — a test
// that fails, not review vigilance — and the named test could not fail.
var authenticationPackages = []string{".", filepath.Join("..", "httpx")}

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
func TestTheSecondFactorOpensNoSocket(t *testing.T) {
	// Files exempt from the scan, by path relative to the package being walked.
	// Empty today, and kept as a declared empty set rather than as an absent
	// concept: an exemption added later is then a visible diff with a name beside
	// it, instead of a `continue` somebody slipped into the loop.
	exempt := map[string]bool{}

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
					t.Errorf("%s:%d uses %s. Nothing on an authentication path in this "+
						"product reaches the network: TOTP is arithmetic over a clock and "+
						"a shared secret, and a second factor that asked somebody would be "+
						"a channel docs/SECURITY.md's egress row does not name.",
						rel, i+1, m)
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no files; the walk is broken rather than the packages clean")
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
