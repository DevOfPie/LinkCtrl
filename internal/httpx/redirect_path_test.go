package httpx

import (
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

// The worked examples. What deep-link forwarding is supposed to do, before the
// property test below establishes what it is not allowed to do.
func TestAppendPathJoinsOntoTheDestination(t *testing.T) {
	tests := []struct {
		name   string
		target string
		rest   string
		want   string
	}{
		{
			name:   "destination with a path",
			target: "https://docs.example/product",
			rest:   "api/quickstart",
			want:   "https://docs.example/product/api/quickstart",
		},
		{
			name:   "destination with no path",
			target: "https://docs.example",
			rest:   "guide",
			want:   "https://docs.example/guide",
		},
		{
			name:   "destination's trailing slash is not doubled",
			target: "https://docs.example/product/",
			rest:   "guide",
			want:   "https://docs.example/product/guide",
		},
		{
			// The visitor's own trailing slash is theirs to keep: /a/b/ and
			// /a/b are different resources to plenty of servers.
			name:   "the visitor's trailing slash survives",
			target: "https://docs.example/product",
			rest:   "guide/",
			want:   "https://docs.example/product/guide/",
		},
		{
			name:   "the destination's query is untouched",
			target: "https://docs.example/product?lang=en",
			rest:   "guide",
			want:   "https://docs.example/product/guide?lang=en",
		},
		{
			name:   "the destination's fragment is untouched",
			target: "https://docs.example/product#top",
			rest:   "guide",
			want:   "https://docs.example/product/guide#top",
		},
		{
			// The mangling this whole design is arranged around. ServeMux hands
			// PathValue back unescaped, so a naive handler would append a real
			// '?' here and hand the visitor a query the destination never had.
			name:   "an encoded question mark stays a path byte",
			target: "https://docs.example/product",
			rest:   "a%3Fb=1",
			want:   "https://docs.example/product/a%3Fb=1",
		},
		{
			name:   "an encoded slash stays inside one segment",
			target: "https://docs.example/product",
			rest:   "a%2Fb",
			want:   "https://docs.example/product/a%2Fb",
		},
		{
			name:   "an encoded hash stays a path byte",
			target: "https://docs.example/product",
			rest:   "a%23b",
			want:   "https://docs.example/product/a%23b",
		},
		{
			// Not re-encoded on the way past, for the same reason appendRaw
			// leaves an unparseable query alone: %C3%A9 and é are the same
			// resource, and rewriting one into the other is a change the owner
			// did not ask for.
			name:   "escaped UTF-8 is passed through verbatim",
			target: "https://docs.example/product",
			rest:   "caf%C3%A9",
			want:   "https://docs.example/product/caf%C3%A9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := appendPath(tt.target, tt.rest)
			if !ok {
				t.Fatalf("appendPath(%q, %q) refused to join", tt.target, tt.rest)
			}
			if got != tt.want {
				t.Errorf("appendPath(%q, %q) = %q, want %q", tt.target, tt.rest, got, tt.want)
			}
		})
	}
}

// Dot segments are refused, in every spelling that reaches a handler.
//
// The literal ones never arrive — ServeMux cleans the escaped path and
// redirects first — so these are the encoded forms, which the URL standard
// counts as dots and a browser therefore resolves.
func TestAppendPathRefusesDotSegments(t *testing.T) {
	for _, rest := range []string{
		"%2e%2e", "%2E%2E", ".%2e", "%2e.", "a/%2e%2e/b",
		"%2e%2e/%2e%2e/admin", "%2e", "a/%2E/b",
	} {
		t.Run(rest, func(t *testing.T) {
			if got, ok := appendPath("https://docs.example/product/v2", rest); ok {
				t.Errorf("appendPath accepted %q and produced %q; a browser resolves "+
					"that back out of the subtree the owner pointed at", rest, got)
			}
		})
	}
}

// pathRemainder reads the bytes as they arrived, not as the router unescaped
// them, and it does not care how the alias segment was spelled.
func TestPathRemainderIsTakenFromTheEscapedPath(t *testing.T) {
	tests := []struct {
		escaped, want string
		wantDeep      bool
	}{
		// The pair that has to stay distinguishable: neither has a remainder,
		// and only the second is asking for something under the alias.
		{"/abc", "", false},
		{"/abc/", "", true},

		{"/abc/x/y", "x/y", true},
		{"/abc/x%2Fy", "x%2Fy", true},
		{"/a%62c/deep", "deep", true},
		{"/abc/a%3Fb", "a%3Fb", true},
	}
	for _, tt := range tests {
		got, deep := pathRemainder(tt.escaped)
		if got != tt.want || deep != tt.wantDeep {
			t.Errorf("pathRemainder(%q) = (%q, %v), want (%q, %v)",
				tt.escaped, got, deep, tt.want, tt.wantDeep)
		}
	}
}

// The property test M33 asks for.
//
// Two invariants, and they are the two ways this could be a vulnerability
// rather than a bug. **The origin never moves**: whatever a visitor puts after
// the alias, the Location keeps the destination's scheme and host — which is
// what rules out resolving the remainder as a reference, where "//evil.example"
// becomes a different site. **The destination is never rewritten**: its path
// survives as a prefix, its query and fragment come through byte for byte, and
// nothing the visitor sends can become a '?' or a '#'.
//
// The inputs are built the way a real one is — parsed as a URL path, then
// sliced out of EscapedPath — so anything the generator invents that could not
// survive an HTTP request line is normalized away before the joiner sees it,
// exactly as it would be in production. A generator that fed appendPath strings
// no request could produce would be testing a function nobody calls.
func TestAppendPathNeverMovesTheOriginOrRewritesTheDestination(t *testing.T) {
	targets := []string{
		"https://docs.example/product",
		"https://docs.example",
		"https://docs.example/",
		"https://docs.example/product/",
		"https://docs.example/p?lang=en&v=2",
		"https://docs.example/p?q=50%&size=2", // a query Go cannot round-trip
		"https://docs.example/p#top",
		"http://other.example:8443/deep/path",
	}

	// Fixed seed: a property test that cannot be re-run on the input that broke
	// it is a flake generator.
	rng := rand.New(rand.NewPCG(0x33, 0x33))

	for i := range 4000 {
		target := targets[rng.IntN(len(targets))]
		raw := randomRemainder(rng)

		// Through the same pipeline the router puts it through.
		u, err := url.Parse("http://links.test/alias/" + raw)
		if err != nil {
			continue
		}
		rest, deep := pathRemainder(u.EscapedPath())
		if !deep || rest == "" {
			continue
		}

		got, ok := appendPath(target, rest)
		if !ok {
			continue // refused, which is always a safe answer
		}

		base, err := url.Parse(target)
		if err != nil {
			t.Fatalf("target %q does not parse", target)
		}
		out, err := url.Parse(got)
		if err != nil {
			t.Fatalf("case %d: appendPath(%q, %q) = %q, which does not parse: %v",
				i, target, rest, got, err)
		}

		if out.Scheme != base.Scheme || out.Host != base.Host {
			t.Fatalf("case %d: appendPath(%q, %q) = %q — the origin moved to %s://%s",
				i, target, rest, got, out.Scheme, out.Host)
		}
		if out.RawQuery != base.RawQuery {
			t.Fatalf("case %d: appendPath(%q, %q) = %q — the destination's query became %q",
				i, target, rest, got, out.RawQuery)
		}
		if out.Fragment != base.Fragment {
			t.Fatalf("case %d: appendPath(%q, %q) = %q — the destination's fragment became %q",
				i, target, rest, got, out.Fragment)
		}

		prefix := strings.TrimSuffix(base.EscapedPath(), "/") + "/"
		if !strings.HasPrefix(out.EscapedPath(), prefix) {
			t.Fatalf("case %d: appendPath(%q, %q) = %q — the destination's own path %q "+
				"is no longer a prefix", i, target, rest, got, prefix)
		}

		// Nothing a browser would resolve back up the tree.
		appended := strings.TrimPrefix(out.EscapedPath(), prefix)
		for seg := range strings.SplitSeq(appended, "/") {
			decoded, err := url.PathUnescape(seg)
			if err != nil {
				t.Fatalf("case %d: appendPath(%q, %q) = %q — segment %q is not valid escaping",
					i, target, rest, got, seg)
			}
			if decoded == "." || decoded == ".." {
				t.Fatalf("case %d: appendPath(%q, %q) = %q — a dot segment survived",
					i, target, rest, got)
			}
		}
	}
}

// randomRemainder builds a path remainder out of tokens chosen for being
// awkward: every character that terminates a path, both spellings of a dot
// segment, a truncated escape, and ordinary text so the generator also produces
// remainders that are supposed to work.
func randomRemainder(rng *rand.Rand) string {
	tokens := []string{
		"a", "b", "guide", "api", "v2", "caf%C3%A9", "café",
		"..", ".", "%2e", "%2E", "%2e%2e", ".%2e", "%2e.",
		"/", "//", "%2F", "%2f",
		"?", "%3F", "#", "%23", "&", "=", ";", ":", "@", "+", " ", "%20",
		"%", "%zz", "%2", "\\", "%5C", "\x00", "%00",
		"evil.example", "//evil.example", "https://evil.example",
	}
	var b strings.Builder
	for range 1 + rng.IntN(6) {
		b.WriteString(tokens[rng.IntN(len(tokens))])
		if rng.IntN(3) == 0 {
			b.WriteByte('/')
		}
	}
	return b.String()
}
