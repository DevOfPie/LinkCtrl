package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// TestTheRequestCarriesNothingButAVersion.
//
// **m55.md's central assertion, and the test the milestone describes rather than
// implies**: *asserted by a test that captures the outgoing request and compares
// it against an exact expected form — a test that fails when a field is added,
// which is the point.*
//
// So this is deliberately brittle. Every header the server observes is compared
// against a complete expected set, not checked for the presence of the ones
// somebody thought of; the method, path, query, body and cookies are each
// asserted; and adding anything at all to the request fails here first. If this
// test becomes annoying, that is the mechanism working — the thing to do is
// argue about the field in a review, not widen the assertion.
//
// `Accept-Encoding: gzip` is in the expected set because Go's transport adds it
// and it is genuinely on the wire. Listing it is the honest form: an expectation
// that quietly excluded the headers the runtime adds would be an enumeration of
// what this package writes rather than of what leaves the machine.
func TestTheRequestCarriesNothingButAVersion(t *testing.T) {
	type captured struct {
		method  string
		path    string
		query   string
		body    string
		cookies int
		headers map[string][]string
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1)
		n, _ := r.Body.Read(body)
		got = captured{
			method:  r.Method,
			path:    r.URL.Path,
			query:   r.URL.RawQuery,
			body:    string(body[:n]),
			cookies: len(r.Cookies()),
			headers: map[string][]string{},
		}
		for k, v := range r.Header {
			got.headers[k] = v
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL+"/repos/DevOfPie/LinkCtrl/releases/latest", srv.Client().Transport)
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}

	if got.method != http.MethodGet {
		t.Errorf("method = %q, want GET. Anything else has a body or a side effect.", got.method)
	}
	if got.path != "/repos/DevOfPie/LinkCtrl/releases/latest" {
		t.Errorf("path = %q, want the releases path and nothing appended to it", got.path)
	}
	if got.query != "" {
		t.Errorf("query = %q, want empty. A query string is a place to put a field "+
			"nobody reviewed, which is exactly what this test exists to catch.", got.query)
	}
	if got.body != "" {
		t.Errorf("body = %q, want empty. A GET with a body is a GET carrying something.", got.body)
	}
	if got.cookies != 0 {
		t.Errorf("the request carried %d cookies, want none", got.cookies)
	}

	// The complete expected header set. Values are compared too: User-Agent is
	// the one field that says anything at all about this instance, and its exact
	// shape is what docs/SECURITY.md and docs/configuration.md promise.
	want := map[string]string{
		"Accept":          "application/vnd.github+json",
		"User-Agent":      "LinkCtrl/0.3.0",
		"Accept-Encoding": "gzip",
	}
	for name, wantValue := range want {
		values, ok := got.headers[name]
		if !ok {
			t.Errorf("the request did not carry %s at all", name)
			continue
		}
		if len(values) != 1 || values[0] != wantValue {
			t.Errorf("%s = %v, want [%q]", name, values, wantValue)
		}
	}
	var unexpected []string
	for name := range got.headers {
		if _, ok := want[name]; !ok {
			unexpected = append(unexpected, name+": "+strings.Join(got.headers[name], ","))
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("the request carried headers nothing disclosed: %v\n\n"+
			"docs/SECURITY.md's egress row, docs/configuration.md and the setup "+
			"page all enumerate what this request contains: the source address and "+
			"the version, and nothing else. A header added here is a field added to "+
			"that enumeration, so it is a documentation change and a decision, not "+
			"an implementation detail. Widening this test is the wrong fix.",
			unexpected)
	}
}

// TestNoAuthorizationOrIdentityIsSent is the same claim from the other side.
//
// The set comparison above already fails on any of these, and this exists so the
// failure names the specific thing that would be worst: a credential, or
// anything identifying the instance beyond its version.
func TestNoAuthorizationOrIdentityIsSent(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}

	for _, name := range []string{
		"Authorization", "Cookie", "X-Api-Key", "X-Instance-Id", "X-Linkctrl-Instance",
		"From", "Referer",
	} {
		if v := seen.Get(name); v != "" {
			t.Errorf("the update check sent %s: %q. Nothing about this instance beyond "+
				"its version may leave with this request.", name, v)
		}
	}
	if ua := seen.Get("User-Agent"); ua != "LinkCtrl/0.3.0" {
		t.Errorf("User-Agent = %q, want %q — the version alone, with no platform, "+
			"Go version or hostname beside it", ua, "LinkCtrl/0.3.0")
	}
}

// TestARedirectIsNotFollowed.
//
// The rule internal/feed and internal/webhook already follow, and sharper here:
// the destination is a compile-time constant, so a 302 is by definition
// somewhere nobody chose. The second server is where a followed redirect would
// land, and it fails the test by being reached at all rather than by returning
// something wrong.
func TestARedirectIsNotFollowed(t *testing.T) {
	var elsewhereReached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereReached = true
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
	_, err := c.Latest(context.Background())
	if elsewhereReached {
		t.Fatal("the check followed a redirect. The destination is a constant in the " +
			"source precisely so that what this process connects to cannot be " +
			"changed by what it connects to.")
	}
	if err == nil {
		t.Fatal("a 302 produced no error; the response was treated as an answer")
	}
}

// TestADraftOrPrereleaseIsIgnored.
//
// m55.md: *pre-releases and drafts are ignored*. `/releases/latest` already
// excludes both by GitHub's definition, so this asserts the product's own
// behaviour rather than the endpoint's semantics — the claim is about what this
// instance does with such a payload, and a claim that rests on somebody else's
// API is one nothing here tests.
func TestADraftOrPrereleaseIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"draft", `{"tag_name":"v9.9.9","draft":true}`},
		{"prerelease", `{"tag_name":"v9.9.9","prerelease":true}`},
		{"both", `{"tag_name":"v9.9.9","draft":true,"prerelease":true}`},
		{"no tag at all", `{"tag_name":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
			_, err := c.Latest(context.Background())
			if !errors.Is(err, ErrNoRelease) {
				t.Errorf("err = %v, want ErrNoRelease. A %s must not be reported as a "+
					"release somebody should upgrade to.", err, tc.name)
			}
		})
	}
}

// TestAMissingReleaseIsNotAFault.
//
// 404 is what `/releases/latest` answers for a repository whose every release is
// a draft or a pre-release, which is the ordinary state of a fresh fork. Its own
// error so the caller can log it at the level of "nothing to compare against"
// rather than the level of "the network is broken".
func TestAMissingReleaseIsNotAFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
	if _, err := c.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Errorf("err = %v, want ErrNoRelease", err)
	}
}

// TestARateLimitedCheckIsAnError.
//
// 403 with a rate-limit body is what an address behind a large NAT gets, and it
// has to be distinguishable from *no newer release* — not to the operator, who
// sees nothing either way, but to the caller, which must not treat a throttled
// answer as evidence of being up to date. The milestone's own risk note turns on
// this: *no notification is not evidence of up to date*.
func TestARateLimitedCheckIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
	_, err := c.Latest(context.Background())
	if err == nil {
		t.Fatal("a throttled check reported success, so a caller could read it as up to date")
	}
	if errors.Is(err, ErrNoRelease) {
		t.Fatal("a throttled check is indistinguishable from a repository with no " +
			"releases, which is the one confusion this error split exists to prevent")
	}
}

// TestTheResponseBodyIsBounded.
//
// The body is somebody else's. A host that streams forever must not be able to
// fill memory for the length of the timeout, so the decoder reads through a
// limit.
//
// **The oversized document is deliberately valid JSON.** An earlier version of
// this test truncated the body mid-string, and it passed with the bound removed:
// the decode failed on the malformed JSON rather than on the limit, so the test
// asserted nothing about the limit at all. A complete document larger than
// maxResponseBytes fails only because it is cut short by the reader, which is
// the property being claimed — and the second half below proves the same
// document decodes when it fits, so the failure is the bound and not the size.
func TestTheResponseBodyIsBounded(t *testing.T) {
	oversized := `{"tag_name":"v9.9.9","body":"` +
		strings.Repeat("a", maxResponseBytes*2) + `"}`
	if !json.Valid([]byte(oversized)) {
		t.Fatal("the oversized document is not valid JSON, so a decode failure " +
			"would prove nothing about the read bound")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	defer srv.Close()

	c := NewClient("0.3.0", srv.URL, srv.Client().Transport)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatalf("a %d-byte body decoded successfully, so the read is not bounded "+
			"at %d and a host that streams forever can fill memory for the whole "+
			"timeout", len(oversized), maxResponseBytes)
	}

	// The control: the same shape, inside the bound, decodes. Without this the
	// test above would still pass if Latest had simply stopped working.
	small := `{"tag_name":"v9.9.9","body":"` + strings.Repeat("a", 1024) + `"}`
	inBounds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(small))
	}))
	defer inBounds.Close()

	c = NewClient("0.3.0", inBounds.URL, inBounds.Client().Transport)
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("a %d-byte body failed to decode: %v", len(small), err)
	}
	if rel.TagName != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", rel.TagName)
	}
}

// TestOnlyThreeFieldsAreReadOutOfTheResponse.
//
// *The response is read for a version and discarded.* A release payload has
// around forty fields; this asserts that decoding a full-shaped one produces a
// struct carrying three, so "discarded" is a property of the type rather than a
// promise in a comment.
func TestOnlyThreeFieldsAreReadOutOfTheResponse(t *testing.T) {
	full := map[string]any{
		"tag_name": "v9.9.9", "draft": false, "prerelease": false,
		"id": 12345, "html_url": "https://example.invalid/r/1",
		"body": "release notes", "name": "9.9.9",
		"author":       map[string]any{"login": "somebody", "id": 7},
		"assets":       []any{map[string]any{"name": "linkctrl", "download_count": 41}},
		"published_at": "2026-08-08T00:00:00Z",
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}

	var rel Release
	if err := json.Unmarshal(raw, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v9.9.9" || rel.Draft || rel.Prerelease {
		t.Fatalf("decoded %+v, want the tag and the two flags", rel)
	}

	// Re-encoding the struct must produce three keys. Any field added to Release
	// shows up here, which is what makes this an assertion about the type rather
	// than about this one payload.
	out, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Errorf("Release carries %d fields (%v), want 3. Anything kept from the "+
			"response beyond the version is a change to what this instance stores "+
			"about somebody else's API.", len(back), back)
	}
}

// TestParseVersion covers the shapes this product and a Git tag actually
// produce, and the ones that must not parse.
func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
		ok   bool
		why  string
	}{
		{in: "0.3.0", want: Version{0, 3, 0}, ok: true, why: "a plain version"},
		{in: "v0.3.0", want: Version{0, 3, 0}, ok: true, why: "a Git tag"},
		{in: " v0.3.0 ", want: Version{0, 3, 0}, ok: true, why: "surrounding space"},
		{in: "v1.20.13", want: Version{1, 20, 13}, ok: true, why: "multi-digit parts"},
		{
			in: "v0.2.0-39-g888dbcd", want: Version{0, 2, 0}, ok: true,
			why: "git describe: 39 commits past the tag, which is what the demo runs",
		},
		{in: "v0.2.0-dirty", want: Version{0, 2, 0}, ok: true, why: "a dirty tree"},
		{in: "0.3.0+build.7", want: Version{0, 3, 0}, ok: true, why: "semver build metadata"},

		{in: "dev", why: "the fallback when nothing is stamped"},
		{in: "dev-dirty", why: "the fallback from a dirty tree"},
		{in: "", why: "empty"},
		{in: "v", why: "a bare prefix"},
		{in: "1.2", why: "two parts is not a version this product produces"},
		{in: "1.2.x", why: "a wildcard is not a version"},
		{in: "0.1.2beta", why: "an unseparated suffix is a scheme nobody here uses"},
		{in: "latest", why: "a name, not a number"},
	} {
		got, ok := ParseVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("ParseVersion(%q) ok = %v, want %v (%s)", tc.in, ok, tc.ok, tc.why)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestIsNewer is the whole of what decides whether anybody is told.
//
// The `git describe` rows are the reason D162 exists: read as semver,
// `0.2.0-39-g888dbcd` is *older* than `0.2.0`, and an instance running code past
// the tag would be told to upgrade to the tag it is already past.
func TestIsNewer(t *testing.T) {
	for _, tc := range []struct {
		local, remote string
		want          bool
		why           string
	}{
		{local: "0.2.0", remote: "v0.3.0", want: true, why: "a newer minor"},
		{local: "0.2.9", remote: "v0.3.0", want: true, why: "across a minor boundary"},
		{local: "0.3.0", remote: "v0.3.1", want: true, why: "a patch"},
		{local: "0.9.9", remote: "v1.0.0", want: true, why: "a major"},

		{local: "0.3.0", remote: "v0.3.0", why: "the same version"},
		{local: "0.3.1", remote: "v0.3.0", why: "the remote is older"},
		{local: "1.0.0", remote: "v0.9.9", why: "an older major"},

		{
			local: "v0.2.0-39-g888dbcd", remote: "v0.2.0",
			why: "a build past the tag is not behind it — D162, and the state the " +
				"demo instance is in today",
		},
		{
			local: "v0.2.0-39-g888dbcd", remote: "v0.3.0", want: true,
			why: "a build past the tag is still behind a later release",
		},

		{local: "dev", remote: "v9.9.9", why: "a development binary never notifies"},
		{local: "dev-dirty", remote: "v9.9.9", why: "and neither does a dirty one"},
		{local: "0.3.0", remote: "not-a-version", why: "an unparseable remote is a no-op"},
		{local: "0.3.0", remote: "", why: "so is an empty one"},
	} {
		got, ok := IsNewer(tc.local, tc.remote)
		if ok != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v (%s)",
				tc.local, tc.remote, ok, tc.want, tc.why)
			continue
		}
		if ok && got.String() != strings.TrimPrefix(tc.remote, "v") {
			t.Errorf("IsNewer(%q, %q) reported %s, want %s",
				tc.local, tc.remote, got, strings.TrimPrefix(tc.remote, "v"))
		}
	}
}

// TestUserAgentNeverEmpty. A build with no version still has to send something
// parseable as a User-Agent, and "LinkCtrl/" with nothing after it is a header
// some proxies drop.
func TestUserAgentNeverEmpty(t *testing.T) {
	if got := UserAgent(""); got != "LinkCtrl/unknown" {
		t.Errorf("UserAgent(%q) = %q, want %q", "", got, "LinkCtrl/unknown")
	}
}

// TestTheEndpointIsTheProductsOwnRepository.
//
// The constant is the whole of the destination policy, so it is asserted rather
// than trusted: HTTPS, GitHub's API host, this repository, and the `latest`
// path whose semantics the draft/pre-release rule leans on.
func TestTheEndpointIsTheProductsOwnRepository(t *testing.T) {
	const want = "https://api.github.com/repos/DevOfPie/LinkCtrl/releases/latest"
	if Endpoint != want {
		t.Errorf("Endpoint = %q, want %q.\n\n%s", Endpoint, want,
			"This is not a setting and must not become one: a disclosed, auditable "+
				"daily request stops being either the moment it can be redirected.")
	}
}

// TestTheClientRefusesNoProxy documents the transport's stance by construction.
//
// A `Proxy` that read the environment would let HTTP_PROXY send this request
// somewhere docs/SECURITY.md does not name, which is exactly what the constant
// endpoint is for. There is no seam to observe it through, so this asserts the
// field on the transport the constructor builds.
func TestTheClientRefusesNoProxy(t *testing.T) {
	c := NewClient("0.3.0", "", nil)
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the production transport is %T, not *http.Transport", c.http.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the update check honours a proxy from the environment. HTTP_PROXY " +
			"would then decide what this process connects to, which is the thing " +
			"the compile-time endpoint exists to prevent.")
	}
	if c.http.Timeout != DefaultTimeout {
		t.Errorf("client timeout = %s, want %s: an unbounded check holds a job "+
			"goroutine for as long as the far end stays silent", c.http.Timeout, DefaultTimeout)
	}
}
