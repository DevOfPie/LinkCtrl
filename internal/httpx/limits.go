package httpx

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

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
	// LinkPassword throttles guesses at a link password (D54). Shared through
	// Redis like Login, and enforced inside the redirect handler for the same
	// reason NotFound is: only a submitted password may be charged, and the
	// handler is the only thing that knows a request is one.
	LinkPassword *ratelimit.Limiter
}

// NewLimiters builds the limits from configuration.
//
// One construction site for all three, so the composition root can hand the same
// values to the router, the redirect handler and the metrics collector without
// any of them re-deriving a limit from config.
// The Redis client may be nil — cache disabled, or Redis unreachable at boot —
// and then every limit is per instance exactly as it was before M24.
func NewLimiters(cfg config.Config, rdb *goredis.Client, log *slog.Logger) Limiters {
	shared := func(name string) *ratelimit.Shared {
		return ratelimit.NewShared(ratelimit.SharedConfig{
			Client: rdb, Name: name,
			// The cache read timeout, reused deliberately: it is the number an
			// operator already tuned to mean "how long this instance is willing
			// to wait on Redis", and a second knob meaning the same thing is a
			// second knob to get wrong.
			Timeout: cfg.Redis.ReadTimeout,
			Logger:  log,
		})
	}
	return Limiters{
		Login: ratelimit.New(cfg.Auth.LoginRatePerMin, ratelimit.Options{
			Shared: shared("login"),
		}),
		API: ratelimit.New(cfg.Auth.APIRatePerMin, ratelimit.Options{
			Shared: shared("api"),
		}),
		// Deliberately not shared. Sharing it would put a Redis round trip on
		// the 20ms redirect budget and make an optional dependency load-bearing
		// on the hot path — and its job, making alias scanning tedious, is
		// served well enough per instance.
		NotFound: ratelimit.New(cfg.Redirect.NotFoundLimit, ratelimit.Options{}),
		// Shared, unlike NotFound, and the difference is what the two limits
		// protect. A probe limit makes scanning tedious; this one is the only
		// thing standing between a link password and a wordlist, so an attacker
		// must not multiply their budget by the replica count. It is on the
		// redirect tree, so the round trip is paid only by a *submitted*
		// password — never by a visit — and a Redis that does not answer falls
		// back to this instance's own buckets rather than blocking the request.
		LinkPassword: ratelimit.New(cfg.Redirect.PasswordLimit, ratelimit.Options{
			Shared: shared("link_password"),
		}),
	}
}

// Stats returns the enabled limiters for the metrics collector, keyed by the
// label they report under.
//
// Disabled limits are omitted rather than passed as nil pointers: a nil pointer
// in an interface is not a nil interface, so a disabled limiter would otherwise
// be collected as a working one reporting zeros.
func (l Limiters) Stats() map[string]observability.LimiterStats {
	out := make(map[string]observability.LimiterStats, 4)
	for name, lim := range map[string]*ratelimit.Limiter{
		"login": l.Login, "api": l.API, "redirect_404": l.NotFound,
		"link_password": l.LinkPassword,
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
			// The limiter deliberately does not take the request context; see
			// ratelimit.Shared.take. Charging must not be cancellable by the
			// client being charged.
			ok, retry := l.Allow(ClientIPFrom(r.Context())) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
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
