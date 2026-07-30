package httpx

import (
	_ "embed"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

//go:embed static/404.html
var notFoundPage []byte

//go:embed static/410.html
var gonePage []byte

// RedirectHandler serves GET|HEAD /{alias}.
//
// This is the hot path and the whole reason for the router's shape. It runs
// with no session lookup, no CSRF check and no template rendering, because
// each of those is a cost the 20ms budget cannot absorb — the session check
// alone would be a database round trip on every visit.
type RedirectHandler struct {
	Resolver *redirect.Resolver
	// DomainID is resolved once at boot. Looking it up per request would add a
	// query to the path this whole design exists to keep short.
	DomainID uuid.UUID
	Status   int
	Logger   *slog.Logger
	// LogSample logs one in N successful redirects; 0 disables. Logging every
	// redirect at 2,000 rps produces more bytes than the redirects themselves.
	LogSample int64

	// Recorder receives click events. Nil until M8.
	Recorder ClickRecorder

	// Metrics is optional; a nil value makes every observation a no-op. This
	// is the SLO's own measurement point, so it lives here rather than in
	// middleware: the outer view includes the router's dispatch, and the
	// number the target names is the time to resolve and answer.
	Metrics *observability.Metrics

	// NotFoundLimiter throttles addresses that keep asking for aliases which do
	// not exist. Optional; nil disables it and costs nothing.
	//
	// It lives here rather than in middleware because only a miss may be charged.
	// A hit must never spend a token — otherwise a popular link would throttle
	// its own audience — and middleware cannot tell a hit from a miss without
	// intercepting the response.
	NotFoundLimiter *ratelimit.Limiter

	counter atomic.Int64
}

// outcomeLabel names an outcome for metrics. A switch rather than Stringer on
// the type, because the label vocabulary is this package's concern.
func outcomeLabel(o redirect.Outcome) string {
	switch o {
	case redirect.OutcomeRedirect:
		return "redirect"
	case redirect.OutcomeGone:
		return "gone"
	case redirect.OutcomeNotFound:
		return "not_found"
	default:
		return "unknown"
	}
}

// ClickRecorder accepts a click for asynchronous recording.
//
// Deliberately returns nothing. Recording must never fail a redirect, never
// block it, and never be waited on.
type ClickRecorder interface {
	Record(ev ClickEvent)
}

type ClickEvent struct {
	LinkID      uuid.UUID
	WorkspaceID uuid.UUID
	OccurredAt  time.Time
	IP          string
	UserAgent   string
	Referrer    string
	Language    string
	LatencyUS   int32
}

func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	code := strings.TrimSpace(r.PathValue("alias"))

	// Has this address been probing? Checked, not charged: only a miss pays.
	probing, retryAfter := h.probeStatus(r)

	canonical := alias.Canonical(code)

	// Anything that cannot be a stored alias is refused on shape, before the
	// cache or the database is touched. A scanner spraying paths would otherwise
	// turn each one into a query and a negative cache entry.
	//
	// Deliberately not charged to the probe limit: refusing this costs a byte
	// scan, so there is nothing here to protect. It is also what keeps favicon.ico
	// and robots.txt — which every browser asks for and which land on this tree —
	// from spending a real visitor's allowance.
	if !alias.WellFormed(canonical) {
		// Rejected before any lookup, so there is no cache tier to report.
		h.Metrics.ObserveRedirect("not_found", "rejected", time.Since(start))
		h.notFound(w, r)
		return
	}

	var res redirect.Result
	if probing {
		// Throttled. Answer from the in-process cache or not at all: a live link
		// keeps working for the cost of one map lookup, and an alias nobody is
		// using cannot be turned into a database query by asking for it again.
		// This is the whole reason the limit does not simply refuse the request.
		cached, ok := h.Resolver.ResolveCached(h.DomainID, canonical)
		if !ok || cached.Snapshot.NotFound {
			h.Metrics.ObserveRedirect("throttled", "rejected", time.Since(start))
			// Counted under the same series as the other limits, not only as a
			// redirect outcome: an operator asking "is anything being throttled"
			// should get one answer, and the alert on that series would otherwise
			// never fire for the limit most likely to trip.
			h.Metrics.ObserveThrottled("redirect_404")
			h.tooManyRequests(w, r, retryAfter)
			return
		}
		res = cached
	} else {
		resolved, err := h.Resolver.Resolve(r.Context(), h.DomainID, canonical)
		if err != nil {
			// Unavailable, not 404. The load test made the difference concrete: a
			// resolution failure here is overwhelmingly a timeout under load, and
			// answering 404 is a claim that the link does not exist — which is
			// exactly the signal 410-versus-404 exists to control. A crawler or
			// link checker that believes it drops the link from its index, and a
			// retry never happens. 503 says "ask again", which is true.
			//
			// Not charged to the probe limit either: the failure is ours, and
			// throttling someone for it would turn a database blip into a block.
			h.Logger.Error("redirect resolution failed",
				slog.String("alias", canonical), slog.Any("error", err))
			h.Metrics.ObserveRedirect("error", "none", time.Since(start))
			h.unavailable(w, r)
			return
		}
		res = resolved
	}

	outcome := res.Snapshot.Decide(start)
	// Observed here, before the response is written, so the measurement covers
	// resolution and decision rather than however long a client takes to read
	// the body. Writing an empty 302 is a syscall on an already-open socket.
	h.Metrics.ObserveRedirect(outcomeLabel(outcome), string(res.Source), time.Since(start))

	switch outcome {
	case redirect.OutcomeGone:
		// Not charged to the probe limit: the alias really exists, so asking for
		// it is not probing. A link checker following a dead link is not abuse.
		h.gone(w, r)
		return
	case redirect.OutcomeNotFound:
		h.chargeProbe(r)
		h.notFound(w, r)
		return
	case redirect.OutcomeRedirect:
		// Falls through to the redirect below. Listed rather than left to a
		// default so that a new outcome is a compile-time-adjacent failure —
		// the exhaustive linter flags the missing case — instead of silently
		// being treated as "redirect anyway".
	}

	target := res.Snapshot.URL
	if res.Snapshot.ForwardQuery && r.URL.RawQuery != "" {
		target = appendQuery(target, r.URL.RawQuery)
	}

	// 302, never 301. Links are editable by design, so a permanent redirect
	// would be cached by browsers and intermediaries and keep sending traffic
	// to the old destination long after an edit — with no way to recall it.
	// no-store for the same reason.
	h.Location(w, target, h.status())

	if h.Recorder != nil && r.Method != http.MethodHead {
		h.Recorder.Record(ClickEvent{
			LinkID:      res.Snapshot.LinkID,
			WorkspaceID: res.Snapshot.WorkspaceID,
			OccurredAt:  start,
			IP:          clientIPString(r),
			UserAgent:   r.UserAgent(),
			Referrer:    r.Referer(),
			Language:    r.Header.Get("Accept-Language"),
			LatencyUS:   latencyUS(time.Since(start)),
		})
	}

	if h.LogSample > 0 && h.counter.Add(1)%h.LogSample == 0 {
		h.Logger.Info("redirect",
			slog.String("alias", canonical),
			slog.String("source", string(res.Source)),
			slog.Int64("duration_us", time.Since(start).Microseconds()),
		)
	}
}

// Location writes the redirect response.
func (h *RedirectHandler) Location(w http.ResponseWriter, target string, status int) {
	head := w.Header()
	head.Set("Location", target)
	head.Set("Cache-Control", "private, no-store, max-age=0")
	head.Set("Referrer-Policy", "unsafe-url")
	head.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
}

func (h *RedirectHandler) status() int {
	if h.Status == 0 {
		return http.StatusFound
	}
	return h.Status
}

// probeStatus reports whether this address has spent its 404 allowance.
//
// Nothing happens at all when the limit is off — not even a context lookup — so
// the default hot path is exactly what it was before probe limiting existed.
func (h *RedirectHandler) probeStatus(r *http.Request) (bool, time.Duration) {
	if h.NotFoundLimiter == nil {
		return false, 0
	}
	ok, retry := h.NotFoundLimiter.Check(ClientIPFrom(r.Context()))
	return !ok, retry
}

// chargeProbe bills one miss to the client's 404 allowance.
//
// Called only where a miss cost a lookup. Two rules follow from that, and
// together they are what keep the limit from throttling real traffic: a bucket
// can only empty by asking for well-formed aliases that are not there, and a hit
// never spends anything.
func (h *RedirectHandler) chargeProbe(r *http.Request) {
	if h.NotFoundLimiter == nil {
		return
	}
	h.NotFoundLimiter.Charge(ClientIPFrom(r.Context()))
}

// tooManyRequests refuses a request from an address that has been probing.
//
// A bare status and one line of text, not the 404 page: the recipient has just
// been told they are asking for too much, and sending them a kilobyte of HTML to
// say so rewards the behaviour being limited.
func (h *RedirectHandler) tooManyRequests(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	head := w.Header()
	head.Set("Content-Type", "text/plain; charset=utf-8")
	head.Set("X-Robots-Tag", "noindex, nofollow")
	head.Set("Cache-Control", "no-store")
	setRetryAfter(w, retry)
	w.WriteHeader(http.StatusTooManyRequests)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("Too many requests\n"))
	}
}

func (h *RedirectHandler) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, notFoundPage)
}

// unavailable answers a request the server could not resolve.
//
// Retry-After: 1 rather than a page: the alias may well be fine, and the honest
// thing to publish is "ask again shortly". The body is deliberately tiny —
// whatever is overloading the server should not also be asked to serve HTML.
func (h *RedirectHandler) unavailable(w http.ResponseWriter, r *http.Request) {
	head := w.Header()
	head.Set("Content-Type", "text/plain; charset=utf-8")
	head.Set("Retry-After", "1")
	head.Set("X-Robots-Tag", "noindex, nofollow")
	head.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("Temporarily unavailable\n"))
	}
}

func (h *RedirectHandler) gone(w http.ResponseWriter, r *http.Request) {
	// 410 rather than 404: the alias really did exist, and Gone tells crawlers
	// and link checkers to stop retrying rather than treating it as transient.
	h.errorPage(w, r, http.StatusGone, gonePage)
}

func (h *RedirectHandler) errorPage(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	head := w.Header()
	head.Set("Content-Type", "text/html; charset=utf-8")
	// Error pages must never be indexed: a shortener accumulates thousands of
	// dead aliases, and letting a crawler index them is pure noise.
	head.Set("X-Robots-Tag", "noindex, nofollow")
	head.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// latencyUS narrows a duration to the microseconds column.
//
// Clamped rather than converted. click_events.latency_us is an int32, and a
// request held open for longer than about 36 minutes would otherwise wrap to a
// negative figure and quietly poison the latency percentiles — a saturated
// value is obviously an outlier, a negative one looks like a fast request.
func latencyUS(d time.Duration) int32 {
	us := d.Microseconds()
	if us < 0 {
		return 0
	}
	if us > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(us)
}

// appendQuery merges the incoming query string into the destination.
//
// The destination's own parameters win on conflict: they were configured
// deliberately, whereas the incoming ones are whatever a visitor arrived with.
func appendQuery(target, incoming string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	existing := u.Query()
	extra, err := url.ParseQuery(incoming)
	if err != nil {
		return target
	}
	for k, vs := range extra {
		if existing.Has(k) {
			continue
		}
		for _, v := range vs {
			existing.Add(k, v)
		}
	}
	u.RawQuery = existing.Encode()
	return u.String()
}

func clientIPString(r *http.Request) string {
	if addr := ClientIPFrom(r.Context()); addr.IsValid() {
		return addr.String()
	}
	return ""
}
