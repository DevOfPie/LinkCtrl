package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// This file is the redirect tree's half of M66, asserted where it can be
// asserted without a database. What a *visitor* gets from a veto, a rewrite and a
// killed module is an integration test — test/integration/addon_redirect_test.go
// — because every one of those answers depends on a link that exists, a budget
// that is or is not spent, and a real wasm module. What is here is the two things
// that are properties of this package alone: the guard the hot path pays on every
// redirect, and the query merge M66 moved.

// The handler's contract with the add-on host, asserted at compile time. The
// interface exists so this package's tests need no wasm runtime; the assertion
// exists so the interface cannot drift away from what the host actually offers,
// which is the failure a hand-written stub would hide.
var _ AddonRedirector = (*addon.Host)(nil)

// stubRedirector is an add-on host that answers the guard and nothing else. What
// an inline module *does* is driven against a real one in internal/addon and end
// to end in test/integration; this is only here so the guard below can be asked
// with a host present.
type stubRedirector struct{ has bool }

func (s stubRedirector) HasInline() bool { return s.has }

func (s stubRedirector) Inline(_ context.Context, d addon.RedirectDecision) addon.InlineResult {
	return addon.InlineResult{Destination: d.Destination}
}

// requestWithQuery is a GET carrying the raw query given, which is all
// [withForwardedQuery] reads off a request.
func requestWithQuery(t *testing.T, raw string) *http.Request {
	t.Helper()
	target := "/x"
	if raw != "" {
		target += "?" + raw
	}
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// The guard the redirect path pays on every request, and the three states it has
// to get right.
//
// It matters because it runs on the hot path of an instance that installed no
// add-on at all, which is every instance by default: a nil interface and an
// interface holding a nil host are different things in Go, and the second is what
// wiring produces when somebody assigns an optional dependency without checking
// it. Both have to answer *no*, without a panic and without asking the host.
func TestTheRedirectPathAsksNothingWhenNoAddonIsInline(t *testing.T) {
	var noHost *addon.Host
	for _, tc := range []struct {
		name string
		h    *RedirectHandler
		want bool
	}{
		{"no add-on host at all", &RedirectHandler{}, false},
		{"an interface holding a nil host", &RedirectHandler{Addons: noHost}, false},
		{"a host with nothing inline", &RedirectHandler{Addons: stubRedirector{}}, false},
		{"a host with something inline", &RedirectHandler{Addons: stubRedirector{has: true}}, true},
	} {
		if got := tc.h.inline(); got != tc.want {
			t.Errorf("%s: inline() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The merge M66 moved, and the reason it moved.
//
// It used to be the last thing done to the URL, after the gates and after the
// metric. The extension point has to be handed the URL the visitor would actually
// receive, so it moved up — and the case that forces it is the one D317 gave the
// rewrite grant for: on a link with query forwarding on, `fbclid` arrives from the
// *visitor's* request, so a module shown the destination before this merge would
// strip a parameter that was not there yet and the merge would then add it.
//
// Every row below is behaviour that predates M66 and must not have changed. The
// last is the one that says why the function exists at all.
func TestTheForwardedQueryIsMergedBeforeAnAddonSeesTheDestination(t *testing.T) {
	for _, tc := range []struct {
		name     string
		forward  bool
		incoming string
		target   string
		want     string
	}{
		{
			name:   "forwarding off leaves the destination alone",
			target: "https://shop.example/p", incoming: "utm_source=mail",
			want: "https://shop.example/p",
		},
		{
			name: "forwarding on with nothing to forward", forward: true,
			target: "https://shop.example/p", want: "https://shop.example/p",
		},
		{
			name: "forwarding on merges", forward: true,
			target: "https://shop.example/p", incoming: "utm_source=mail",
			want: "https://shop.example/p?utm_source=mail",
		},
		{
			// The signature parameters are addressed to this server and stop here
			// (M35): forwarding one would hand whoever runs the destination a URL they
			// can replay against the link until it expires.
			name: "the signature parameters stop here", forward: true,
			target: "https://shop.example/p", incoming: "sig=abc&exp=123",
			want: "https://shop.example/p",
		},
		{
			// The whole reason the merge had to move: this is what an inline module
			// must be shown, because stripping a tracking parameter from the string
			// above it would have stripped nothing.
			name:    "the visitor's tracking parameter is present before the module runs",
			forward: true, target: "https://shop.example/p?id=7",
			incoming: "fbclid=abc",
			want:     "https://shop.example/p?fbclid=abc&id=7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := requestWithQuery(t, tc.incoming)
			snap := &redirect.Snapshot{ForwardQuery: tc.forward}
			if got := withForwardedQuery(tc.target, r, snap); got != tc.want {
				t.Errorf("withForwardedQuery(%q, %q) = %q, want %q",
					tc.target, tc.incoming, got, tc.want)
			}
		})
	}
}
