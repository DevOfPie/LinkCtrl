// Package update asks, once a day, whether a newer LinkCtrl has been published.
//
// # What leaves this instance, enumerated rather than summarized
//
// One `GET` to a compile-time constant URL. The request carries:
//
//   - this instance's source address, which is a property of opening a socket
//     and not something this package chooses;
//   - the running version, in the `User-Agent`, so a maintainer reading their
//     own logs can tell a 0.2 instance from a 0.3 one.
//
// **Nothing else.** No instance identifier, no deployment size, no link counts,
// no configuration, no account. There is no request body, no query string, no
// cookie and no credential. TestTheRequestCarriesNothingButAVersion compares
// the outgoing request against an exact expected form — every header, the
// method, the URL and the body — so a field added here fails the build rather
// than shipping quietly. That test is the disclosure's enforcement, and it is
// deliberately written to be annoying to change.
//
// The response is read for a version and discarded. Nothing from it is stored
// beyond the version string, which travels in the notification that reports it.
//
// # The rules it inherits, and the one it does not
//
// Both of this product's existing operator-facing clients refuse redirects —
// internal/feed and internal/webhook — and this one does too, on the same
// reasoning: a host that answers 302 is pointing this process at a URL nobody
// configured, and the destination being a constant is exactly what makes a
// redirect away from it worth refusing rather than following.
//
// It does **not** get internal/webhook's dial-time address check. That guard
// exists because a webhook URL is a *user's* choice and can resolve anywhere,
// including at a cloud metadata endpoint. This destination is a constant in this
// file: there is no input, no registration path and nobody to rebind on behalf
// of. Adding the check would suggest there is something here to defend that
// there is not, and the difference is stated rather than left to be noticed.
//
// # Why it is its own job family rather than a step in `housekeeping`
//
// See cmd/linkctrl/jobs.go. In short: this is the only scheduled work in the
// product that opens a socket outwards, and burying it inside a family called
// *maintenance* is how egress stops being auditable in one place.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Endpoint is where the check asks, and it is a constant on purpose.
//
// Not configurable. An operator who wants this instance to ask somewhere else
// wants a different product's release feed reported as this product's, and the
// setting that allowed it would be the one that turned a disclosed, auditable
// daily GET into "wherever this variable points". Turning the check *off* is the
// control that exists (`LINKCTRL_UPDATE_CHECK`), and it is the only one.
//
// `/releases/latest` rather than `/releases`: GitHub's definition of *latest*
// already excludes drafts and pre-releases, so the milestone's *pre-releases and
// drafts are ignored* is true of the endpoint and not only of the code. The
// payload's own flags are checked anyway, below, because a claim that rests on
// somebody else's API semantics is a claim with nothing testing it here.
const Endpoint = "https://api.github.com/repos/DevOfPie/LinkCtrl/releases/latest"

// DefaultTimeout bounds one check end to end: connect, write, read.
//
// Ten seconds, matching the webhook client. Generous for one small GET, and
// bounded for the same reason every other outbound call in this product is: a
// host that accepts a connection and then says nothing must not hold a job
// goroutine open indefinitely. There is no retry — see Service.Run.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes bounds what is read back.
//
// A release payload is a few kilobytes. The bound exists because the body is
// somebody else's and a host that streams forever would otherwise fill memory
// for the whole timeout. 256 KiB is far above any real answer and far below
// anything that matters.
const maxResponseBytes = 256 << 10

// Release is the whole of what this package reads out of the response.
//
// Three fields from a payload that has around forty. Everything else — the
// body, the assets, the author, the URLs — is decoded into nothing and
// discarded, which is what makes *the response is read for a version* a
// property of this struct rather than a promise in a comment.
type Release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Client fetches the latest published release.
type Client struct {
	http     *http.Client
	endpoint string
	// userAgent is the one thing about this instance the request carries.
	userAgent string
}

// NewClient builds the checker's HTTP client.
//
// `endpoint` empty means Endpoint, which is what production always passes;
// a test passes its own httptest server. There is no configuration path that
// reaches this argument.
func NewClient(version, endpoint string, rt http.RoundTripper) *Client {
	if endpoint == "" {
		endpoint = Endpoint
	}
	if rt == nil {
		rt = &http.Transport{
			// One idle connection to one host, once a day. Anything larger would
			// be a pool for a call that is never concurrent with itself.
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     30 * time.Second,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: DefaultTimeout,
			// No proxy, ever, and for internal/webhook's reason rather than a
			// different one: reading the environment here would let HTTP_PROXY
			// send this request somewhere the disclosure does not name, which is
			// the whole thing the constant endpoint above is for.
			Proxy: nil,
		}
	}
	return &Client{
		endpoint:  endpoint,
		userAgent: UserAgent(version),
		http: &http.Client{
			Transport: rt,
			Timeout:   DefaultTimeout,
			// No redirect is followed. internal/feed and internal/webhook say the
			// same thing for the same reason, and here it is sharper: the
			// destination is a compile-time constant, so a 302 is by definition
			// somewhere nobody chose.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// UserAgent is the one identifying string the request carries.
//
// `LinkCtrl/<version>`, and nothing else — no platform, no Go version, no
// hostname. build.Info carries all three and putting them here would be a
// deployment fingerprint offered for nothing: the version is what makes an
// aggregate answerable ("how many instances are still on 0.2"), and the rest
// only narrows it toward one instance.
func UserAgent(version string) string {
	if version == "" {
		version = "unknown"
	}
	return "LinkCtrl/" + version
}

// ErrNoRelease is what a repository with no published release answers with.
//
// Its own error because it is the ordinary state of a fresh fork rather than a
// fault: GitHub answers 404 at `/releases/latest` when every release is a draft
// or a pre-release, and logging that at the same level as a refused connection
// would make a working instance look broken.
var ErrNoRelease = errors.New("update: the repository has published no release")

// Latest fetches the newest published release.
//
// A draft or pre-release answers ErrNoRelease rather than a Release, so a caller
// cannot forget to check the flags. The endpoint should never return one — that
// is what `/releases/latest` means — and the check is here because the
// milestone's promise is about this product's behaviour and not about GitHub's.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return Release{}, fmt.Errorf("update: build request: %w", err)
	}
	// Two headers, and both are here rather than defaulted so that the exact-form
	// test has something to be exact about. Accept names the media type; it says
	// nothing about this instance.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Release{}, ErrNoRelease
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The rate-limit case lands here. It is per source address on the
		// unauthenticated API, so a deployment behind a large NAT can be
		// throttled; the failure mode is a check that does nothing, which is what
		// the milestone promises and what docs/deployment.md tells the operator.
		return Release{}, fmt.Errorf("update: %s answered %s", c.endpoint, resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("update: decode response: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		return Release{}, ErrNoRelease
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return Release{}, ErrNoRelease
	}
	return rel, nil
}

// Version is a release version reduced to the three numbers that order it.
type Version struct {
	Major, Minor, Patch int
}

// Compare orders two versions. Negative when v is older than w.
func (v Version) Compare(w Version) int {
	switch {
	case v.Major != w.Major:
		return v.Major - w.Major
	case v.Minor != w.Minor:
		return v.Minor - w.Minor
	default:
		return v.Patch - w.Patch
	}
}

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// ParseVersion reads the leading `vX.Y.Z` of a version string.
//
// **Everything after the patch number is dropped, and that is the decision this
// function exists to make.** internal/build reports what the linker stamped,
// and what the Makefile stamps is `git describe`: a build 39 commits past the
// v0.2.0 tag reports `v0.2.0-39-g888dbcd`, and one from a dirty tree gets
// `-dirty` on the end. Read as semver those sort *below* `0.2.0`, because a
// pre-release suffix precedes its release — so an instance running code newer
// than 0.2.0 would be told that 0.2.0 was available. Read the way the string is
// actually produced, the suffix means "past this tag", and the honest
// comparison is against the tag itself with a strict `>` on the other side.
//
// A leading `v` is optional, because a Git tag has one and a plain version
// string does not.
//
// `false` for anything that does not start with three numbers, which is how
// **a build reporting "dev" never notifies**: `dev` and `dev-dirty` have no
// numbers to compare and there is nothing to be newer than. A development
// binary telling its operator to upgrade is noise, and this is where that is
// enforced rather than by a special case on the word.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")

	var v Version
	for i, out := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if i > 0 {
			rest, ok := strings.CutPrefix(s, ".")
			if !ok {
				return Version{}, false
			}
			s = rest
		}
		digits := 0
		for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return Version{}, false
		}
		n, err := strconv.Atoi(s[:digits])
		if err != nil {
			// Unreachable for digit runs this product produces, and a refusal
			// rather than a panic for the one that overflows an int.
			return Version{}, false
		}
		*out = n
		s = s[digits:]
	}
	// What is left is a suffix — `-39-g888dbcd`, `-dirty`, `-rc1` — and it is
	// dropped. A suffix that is not separated at all (`0.1.2beta`) is refused,
	// because that is not a shape this product or a Git tag produces and
	// guessing at it would be inventing a version scheme.
	if s != "" && s[0] != '-' && s[0] != '+' {
		return Version{}, false
	}
	return v, true
}

// IsNewer reports whether `remote` names a release newer than `local`.
//
// False whenever either side does not parse, which covers three of the
// milestone's four rules at once: a `dev` build never notifies, an unparseable
// remote version is a no-op rather than an error surface, and an equal or older
// remote is silence. The fourth — drafts and pre-releases — is handled where the
// response is read, because that is a fact about the release rather than about
// its number.
func IsNewer(local, remote string) (Version, bool) {
	lv, ok := ParseVersion(local)
	if !ok {
		return Version{}, false
	}
	rv, ok := ParseVersion(remote)
	if !ok {
		return Version{}, false
	}
	if rv.Compare(lv) <= 0 {
		return Version{}, false
	}
	return rv, true
}
