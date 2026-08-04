package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
)

// limited builds the chain a request really travels: RealIP resolves the address
// the limiter keys on, so a test that skipped it would be testing a limiter that
// sees no client at all.
func limited(l *ratelimit.Limiter, deny Deny) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return RealIP(nil)(RateLimit(l, "test", nil, deny)(ok))
}

func get(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// marker is a handler with a distinguishable type, so "was it wrapped?" is a
// type assertion rather than an attempt to compare function pointers.
type marker struct{}

func (marker) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// A nil limiter must return the handler untouched, not a wrapper that always
// allows: "0 disables the limit" should cost nothing at all, including the
// context lookup a wrapper would do to find the client address.
func TestRateLimitNilLimiterIsTransparent(t *testing.T) {
	if _, ok := RateLimit(nil, "test", nil, nil)(marker{}).(marker); !ok {
		t.Error("RateLimit(nil) wrapped the handler; a disabled limit should add nothing")
	}
	if got := get(limited(nil, nil), "203.0.113.9:1234").Code; got != http.StatusNoContent {
		t.Errorf("status = %d, want 204", got)
	}
}

func TestRateLimitRefusesWithProblemAndRetryAfter(t *testing.T) {
	h := limited(ratelimit.New(2, ratelimit.Options{}), nil)

	for i := range 2 {
		if got := get(h, "203.0.113.9:1234").Code; got != http.StatusNoContent {
			t.Fatalf("request %d: status = %d, want 204", i+1, got)
		}
	}

	rec := get(h, "203.0.113.9:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	// A client that honours Retry-After needs a number it can wait for, and one
	// that is at least 1: zero would invite an immediate, certain-to-fail retry.
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", rec.Header().Get("Retry-After"))
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	// The only 429 this API produces, since F92 folded account-locked into the
	// ordinary sign-in refusal. A client that cannot recognise this one cannot
	// honour Retry-After.
	if p.Type != problemBase+"rate-limited" {
		t.Errorf("problem type = %q, want %srate-limited", p.Type, problemBase)
	}
	if p.Status != http.StatusTooManyRequests {
		t.Errorf("problem status = %d, want 429", p.Status)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("refusal is served outside the security-header chain and must set nosniff itself")
	}
}

func TestRateLimitKeysOnTheClientAddress(t *testing.T) {
	h := limited(ratelimit.New(1, ratelimit.Options{}), nil)

	if got := get(h, "203.0.113.9:1111").Code; got != http.StatusNoContent {
		t.Fatalf("first address: status = %d, want 204", got)
	}
	if got := get(h, "203.0.113.9:2222").Code; got != http.StatusTooManyRequests {
		// Same address, different source port. A limiter keyed on RemoteAddr
		// verbatim would treat every connection as a new client.
		t.Errorf("second request from the same address: status = %d, want 429", got)
	}
	if got := get(h, "198.51.100.4:1111").Code; got != http.StatusNoContent {
		t.Errorf("different address: status = %d, want 204", got)
	}
}

// The dashboard needs a page, not a problem document, and the router supplies one
// for the same limiter. This is the seam that makes that possible.
func TestRateLimitUsesTheSuppliedDenier(t *testing.T) {
	called := false
	deny := func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("<html>slow down</html>"))
	}
	h := limited(ratelimit.New(1, ratelimit.Options{}), deny)

	get(h, "203.0.113.9:1234")
	rec := get(h, "203.0.113.9:1234")

	if !called {
		t.Fatal("supplied denier was not used")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set for a custom denier too")
	}
	if rec.Body.String() != "<html>slow down</html>" {
		t.Errorf("body = %q, want the denier's own output", rec.Body.String())
	}
}
