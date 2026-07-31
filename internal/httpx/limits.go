package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
)

// Limiters are the request limits the server enforces. A nil member means that
// limit is off, which is the whole reason ratelimit.New returns nil for a rate
// of zero: there is no second "enabled" flag to disagree with the number.
type Limiters struct {
	// Login guards the endpoints that verify a credential.
	Login *ratelimit.Limiter
	// API guards everything under /api/v1.
	API *ratelimit.Limiter
	// NotFound throttles addresses probing for aliases that do not exist. It is
	// enforced inside the redirect handler rather than by middleware, because
	// only a miss may be charged and middleware cannot tell a miss from a hit
	// without inspecting the response it is wrapping.
	NotFound *ratelimit.Limiter
}

// NewLimiters builds the limits from configuration.
//
// One construction site for all three, so the composition root can hand the same
// values to the router, the redirect handler and the metrics collector without
// any of them re-deriving a limit from config.
func NewLimiters(cfg config.Config) Limiters {
	return Limiters{
		Login:    ratelimit.New(cfg.Auth.LoginRatePerMin, ratelimit.Options{}),
		API:      ratelimit.New(cfg.Auth.APIRatePerMin, ratelimit.Options{}),
		NotFound: ratelimit.New(cfg.Redirect.NotFoundLimit, ratelimit.Options{}),
	}
}

// Stats returns the enabled limiters for the metrics collector, keyed by the
// label they report under.
//
// Disabled limits are omitted rather than passed as nil pointers: a nil pointer
// in an interface is not a nil interface, so a disabled limiter would otherwise
// be collected as a working one reporting zeros.
func (l Limiters) Stats() map[string]observability.LimiterStats {
	out := make(map[string]observability.LimiterStats, 3)
	for name, lim := range map[string]*ratelimit.Limiter{
		"login": l.Login, "api": l.API, "redirect_404": l.NotFound,
	} {
		if lim != nil {
			out[name] = lim
		}
	}
	return out
}

// Deny answers a request that a rate limit refused. Retry-After is already set.
//
// A function rather than a fixed response, because the same limiter guards two
// surfaces: a refused API call should be a problem document, and a refused form
// post should be a page a person can read.
type Deny func(http.ResponseWriter, *http.Request)

// RateLimit throttles requests by client address.
//
// A nil limiter returns the handler untouched rather than a wrapper that always
// allows, so a disabled limit costs nothing at all — not even a context lookup
// per request.
//
// The address comes from RealIP, which trusts X-Forwarded-For only from
// configured proxies. That matters more here than anywhere else: behind a proxy
// with TRUSTED_PROXIES unset, every request carries the proxy's address, all
// traffic shares one bucket, and the limit applies to the whole world at once.
func RateLimit(l *ratelimit.Limiter, name string, metrics *observability.Metrics, deny Deny) func(http.Handler) http.Handler {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if deny == nil {
		deny = writeTooManyRequests
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retry := l.Allow(ClientIPFrom(r.Context()))
			if ok {
				next.ServeHTTP(w, r)
				return
			}
			setRetryAfter(w, retry)
			metrics.ObserveThrottled(name)
			deny(w, r)
		})
	}
}

// setRetryAfter writes the standard back-off header.
func setRetryAfter(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(retry)))
}

// writeTooManyRequests is the API's refusal.
//
// A distinct problem type from account-locked, which is also a 429: one means
// "this account is frozen for a while", the other "you are going too fast". A
// client that cannot tell them apart will retry the wrong one.
func writeTooManyRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	WriteProblem(w, r, Problem{
		Type:   problemBase + "rate-limited",
		Title:  "Too many requests",
		Status: http.StatusTooManyRequests,
		Detail: "Too many requests from your address. Retry after the interval in the Retry-After header.",
	})
}
