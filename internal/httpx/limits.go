package httpx

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/link"
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
	// BlockedAudit bounds how often one actor makes the *same* destination
	// refusal write an audit row (F14).
	//
	// The odd one out here: every other limiter refuses a request, and this one
	// refuses nothing. The refusal happens either way — what it bounds is the
	// row. `destination.blocked` is the only audited action recording something
	// that did not happen, so it is the only one with no successful state change
	// bounding how often a caller can provoke it.
	//
	// Keyed per actor and per reason code, never per actor alone, because the
	// attacker picks the noise: a per-actor budget would let a flood of one
	// refusal bury a different one. Shared through Redis like Login, so the
	// bound means one thing on a four-replica instance; a Redis that does not
	// answer falls back to local buckets, which errs toward writing more rows
	// rather than fewer.
	BlockedAudit *ratelimit.Limiter
	// Upload guards the endpoints that accept a file (M50.5).
	//
	// **The API limit is not this limit, and the difference is what a request
	// costs rather than how many there are.** Everything else under `/api/v1` is
	// a JSON body this product caps at 256 KiB and decodes with the standard
	// library's parser; an upload is megabytes of somebody else's bytes handed to
	// a decoder or a compiler, bounded by whichever endpoint took them.
	// `API_RATE_PER_MIN`'s 600 was chosen about the first kind, and inheriting it
	// for the second would be a number nobody set for what it would then bound.
	//
	// **One bucket for every endpoint that takes a file**, whatever the file is
	// and however much larger one of them may be than another: what the bucket is
	// about is true of all of them, and a second number would be a second thing
	// to tune. D345 is where that is argued and what it costs is stated.
	//
	// Shared through Redis like Login, because a per-replica budget on a
	// four-replica instance is four times the limit an operator configured, and
	// bandwidth is the resource being protected. A Redis that does not answer
	// falls back to this instance's own buckets, which errs toward refusing less
	// rather than refusing a legitimate upload.
	Upload *ratelimit.Limiter
}

// NewLimiters builds the limits from configuration.
//
// One construction site for every limit, so the composition root can hand the
// same values to the router, the redirect handler, the link service and the
// metrics collector without any of them re-deriving a limit from config.
// Deliberately not "all three", or all four: a count here is a fact nothing
// keeps true, which is the shape F69 was.
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
		BlockedAudit: ratelimit.New(link.BlockedAuditRatePerMin, ratelimit.Options{
			Shared: shared("blocked_audit"),
		}),
		Upload: ratelimit.New(cfg.Auth.UploadRatePerMin, ratelimit.Options{
			Shared: shared("upload"),
		}),
	}
}

// Stats returns the enabled limiters for the metrics collector, keyed by the
// label they report under.
//
// Disabled limits are omitted rather than passed as nil pointers: a nil pointer
// in an interface is not a nil interface, so a disabled limiter would otherwise
// be collected as a working one reporting zeros.
//
// A limiter left out of this map is worse than one reporting zeros: it emits no
// series at all — no tracked keys, no overflows, no fallbacks — and absence is
// indistinguishable from health on a dashboard. BlockedAudit was absent here
// from the milestone that added it, so the audit-flood it exists to bound was
// invisible to the runbook's overflow alert; the Stats test now holds this map
// to the struct's own field count so the next limiter cannot vanish the same
// way. BlockedAudit meters through AllowKey rather than middleware and never
// refuses a request, so it has no rate_limited_total series — that is
// structural, and these three bookkeeping series are the only view of it.
func (l Limiters) Stats() map[string]observability.LimiterStats {
	out := make(map[string]observability.LimiterStats, 6)
	for name, lim := range map[string]*ratelimit.Limiter{
		"login": l.Login, "api": l.API, "redirect_404": l.NotFound,
		"link_password": l.LinkPassword, "blocked_audit": l.BlockedAudit,
		"upload": l.Upload,
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
	return RateLimitWhen(l, name, metrics, deny, nil)
}

// RateLimitWhen is RateLimit with a shape test in front of the charge: a request
// `chargeable` refuses is passed through untouched and spends nothing. A nil
// test charges everything, which is what RateLimit is.
//
// **It exists so that a budget a person needs can only be spent by traffic that
// reached the thing the budget is about** (D309). The precedent is the 404-probe
// limiter, which charges a miss and never a hit and is enforced inside the
// redirect handler for exactly that reason (`Limiters.NotFound`); the difference
// is only that some routes can tell a real request from a probe *before* the
// handler runs, from a path value and something the boot already decided. Where
// that holds, the test sits here, at the registration, where the rule is visible
// beside the pattern rather than buried in what the pattern serves.
//
// The test runs before `Allow`, so a refused shape is never charged and never
// metered. It must not be the expensive half of the request: it decides whether
// the request may be *counted*, so anything it does is done by every caller
// including the one being limited.
func RateLimitWhen(l *ratelimit.Limiter, name string, metrics *observability.Metrics,
	deny Deny, chargeable func(*http.Request) bool) func(http.Handler) http.Handler {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if deny == nil {
		deny = writeTooManyRequests
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if chargeable != nil && !chargeable(r) {
				next.ServeHTTP(w, r)
				return
			}
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
// `rate-limited` is the only 429 this API produces. It used to share the status
// with `account-locked`, and the two were deliberately distinguishable so a
// client would not retry the wrong one; finding F92 removed that second type,
// because which of them a caller got answered whether the address they named is
// registered. What survives is the distinction a client can still act on: this
// refusal carries Retry-After and is worth waiting out, and a sign-in refusal is
// a 401 whatever caused it.
func writeTooManyRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	WriteProblem(w, r, Problem{
		Type:   problemBase + "rate-limited",
		Title:  "Too many requests",
		Status: http.StatusTooManyRequests,
		Detail: "Too many requests from your address. Retry after the interval in the Retry-After header.",
	})
}
