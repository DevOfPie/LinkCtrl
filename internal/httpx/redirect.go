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
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

//go:embed static/404.html
var notFoundPage []byte

//go:embed static/410.html
var gonePage []byte

// blockedPage is what a refused bot receives (M32.5).
//
// Embedded, so it is bytes in the binary before main runs — the strongest form
// of "pre-rendered at init" available, and the reason the refusal costs no
// template execution on a tree that has never rendered one. It is a fixed page
// that names no alias and no destination: a refusal echoing either would make
// the shortener a confirmation oracle for which short codes are real and where
// they point, which is precisely what a crawler asking ten thousand times is
// trying to find out.
//
//go:embed static/403.html
var blockedPage []byte

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
	canonical := alias.Canonical(code)

	// Anything that cannot be a stored alias is refused on shape, before the
	// limiter, the cache or the database is touched. A scanner spraying paths
	// would otherwise turn each one into a query and a negative cache entry.
	//
	// Deliberately not charged to the probe limit — refusing this costs a byte
	// scan, so there is nothing here to protect — and deliberately ahead of the
	// limiter check, so favicon.ico and robots.txt (which every browser asks
	// for, and which land on this tree) never touch the limiter's shards at
	// all, not even to read them.
	if !alias.WellFormed(canonical) {
		// Rejected before any lookup, so there is no cache tier to report.
		h.Metrics.ObserveRedirect("not_found", "rejected", time.Since(start))
		h.notFound(w, r)
		return
	}

	// Has this address been probing? Checked, not charged: only a miss pays.
	probing, retryAfter := h.probeStatus(r)

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

	// The bot gate (M32.5), and it runs before Decide deliberately.
	//
	// Answering the same refusal whatever state the link is in is what keeps
	// blocking from becoming a better enumeration oracle than the 404 path
	// already is: a crawler that could tell an active blocked link (403) from an
	// expired one (410) would learn something from being refused. It learns
	// nothing instead.
	//
	// Not charged to the probe limiter either. The alias exists — asking for it
	// is not probing, and the client is being refused for what it is rather than
	// for what it asked.
	if blockedAsBot(res.Snapshot, r.UserAgent()) {
		h.Metrics.ObserveRedirect("blocked_bot", string(res.Source), time.Since(start))
		h.blocked(w, r)
		// Counted, never audited. The audit log is for administrative change,
		// and a crawler hitting one link ten thousand times would write ten
		// thousand rows into the table M21 built a growth alert for. It is
		// already a click with is_bot true — the recorder derives that from the
		// same Classify call this gate used — so the traffic is visible where
		// traffic is read.
		h.record(r, res.Snapshot, start)
		return
	}

	outcome := res.Snapshot.Decide(start)

	// Deep-link path forwarding (M33), and the decision has to happen here
	// rather than beside the Location line below, because the metric is
	// recorded in between: converting the outcome after observing it would put
	// a 404 in the "redirect" series.
	//
	// A multi-segment request the link cannot forward is a miss, not a redirect
	// to the bare destination. Answering the destination anyway would mean
	// /{alias}/anything-at-all resolved for every link on the instance, which
	// turns one alias into an unbounded set of URLs that all go somewhere the
	// owner did not point them.
	//
	// forwardable also refuses a remainder it cannot join safely — see
	// appendPath — and refusing lands here rather than silently dropping the
	// path, for the same reason: a visitor who asked for /{alias}/a/../b must
	// not be sent somewhere else without being told.
	var target string
	if outcome == redirect.OutcomeRedirect {
		joined, ok := forwardable(res.Snapshot, r)
		if !ok {
			outcome = redirect.OutcomeNotFound
		}
		target = joined
	}

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

	if res.Snapshot.ForwardQuery && r.URL.RawQuery != "" {
		target = appendQuery(target, r.URL.RawQuery)
	}

	// 302, never 301. Links are editable by design, so a permanent redirect
	// would be cached by browsers and intermediaries and keep sending traffic
	// to the old destination long after an edit — with no way to recall it.
	// no-store for the same reason.
	h.Location(w, target, h.status())
	h.record(r, res.Snapshot, start)

	if h.LogSample > 0 && h.counter.Add(1)%h.LogSample == 0 {
		h.Logger.Info("redirect",
			slog.String("alias", canonical),
			slog.String("source", string(res.Source)),
			slog.Int64("duration_us", time.Since(start).Microseconds()),
		)
	}
}

// blockedAsBot reports whether this request is an automated client the link
// refuses (M32.5).
//
// Two string comparisons when blocking is off, which is every link on a default
// instance, and one pass over the user agent when it is on. Nothing here reads
// the database, the cache or the session: both settings arrived inside the
// snapshot the resolver had already produced.
//
// The two halves are separate on purpose. domain.BlocksBots is the only place
// precedence is decided, for the redirect path and the management surfaces
// alike; analytics.Classify is the same pure function the click recorder uses,
// called rather than copied, so what gets blocked cannot drift from what gets
// counted as a bot. A second classifier here would produce exactly that drift,
// silently, and the analytics would go on insisting the refused traffic was
// human.
//
// Order matters for cost, not for correctness: the policy check is cheap and
// usually false, so Classify runs only on links that actually block.
func blockedAsBot(snap *redirect.Snapshot, ua string) bool {
	if snap == nil || snap.NotFound {
		return false
	}
	if !domain.BlocksBots(snap.BotPolicy, snap.DomainBotPolicy) {
		return false
	}
	return analytics.Classify(ua).IsBot
}

// record hands a click to the recorder, if there is one.
//
// Shared by the redirect and the refusal so the two cannot drift into
// disagreeing about what a click event carries. HEAD is excluded in both: it is
// a client asking about the link rather than following it, and counting it
// would inflate every figure a link's owner reads.
func (h *RedirectHandler) record(r *http.Request, snap *redirect.Snapshot, start time.Time) {
	if h.Recorder == nil || snap == nil || r.Method == http.MethodHead {
		return
	}
	h.Recorder.Record(ClickEvent{
		LinkID:      snap.LinkID,
		WorkspaceID: snap.WorkspaceID,
		OccurredAt:  start,
		IP:          clientIPString(r),
		UserAgent:   r.UserAgent(),
		Referrer:    r.Referer(),
		Language:    r.Header.Get("Accept-Language"),
		LatencyUS:   latencyUS(time.Since(start)),
	})
}

// blocked refuses an automated client.
//
// The same headers and the same shape as the other error pages, and a body that
// is fixed at compile time. There is no branch on which state the link was in,
// because there is no state to reveal.
func (h *RedirectHandler) blocked(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusForbidden, blockedPage)
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
	ok, retry := h.NotFoundLimiter.Check(ClientIPFrom(r.Context())) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
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
	h.NotFoundLimiter.Charge(ClientIPFrom(r.Context())) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
}

// tooManyRequests refuses a request from an address that has been probing.
//
// A bare status and one line of text, not the 404 page: the recipient has just
// been told they are asking for too much, and sending them a kilobyte of HTML to
// say so rewards the behaviour being limited.
func (h *RedirectHandler) tooManyRequests(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	head := w.Header()
	head.Set("Content-Type", "text/plain; charset=utf-8")
	head.Set("X-Content-Type-Options", "nosniff")
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
	writeUnavailable(w, r)
}

// writeUnavailable is the same answer for anything on the redirect tree that
// could not be resolved, shared so the root redirect cannot drift into a
// different set of headers than an alias.
func writeUnavailable(w http.ResponseWriter, r *http.Request) {
	head := w.Header()
	head.Set("Content-Type", "text/plain; charset=utf-8")
	head.Set("X-Content-Type-Options", "nosniff")
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
	// The redirect tree is outside the security-header chain, so every response
	// it writes sets nosniff itself — the same rule writeTooManyRequests
	// documents for the API limiter's refusals.
	head.Set("X-Content-Type-Options", "nosniff")
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

// forwardable produces the destination for this request, and reports whether
// there is one at all (M33).
//
// Three answers, and the middle one is the milestone:
//
//   - A bare /{alias}. The destination is the destination, exactly as before
//     this existed.
//   - Anything after the alias, forwarding off. Nothing to serve: reported
//     false, and the caller turns it into the ordinary miss. That includes a
//     bare trailing slash, which has answered 404 for as long as the redirect
//     tree has existed — TestRedirectMatrix names the case — and which this
//     milestone was not asked to change.
//   - Anything after the alias, forwarding on. Joined onto the destination, or
//     refused if it cannot be joined safely. An empty remainder joins to the
//     destination's own root — /{alias}/ is the top of the forwarded subtree
//     rather than a separate case.
//
// The remainder is taken from EscapedPath and never from PathValue("rest").
// ServeMux unescapes a wildcard before storing it, so PathValue turns
// /a/x%2Fy into "x/y" and /a/a%3Fb into "a?b" — feed that to the joiner and a
// visitor can split a segment in two, or inject a query the destination never
// had. EscapedPath is the bytes as they arrived, and net/url guarantees it
// carries no raw '?', '#' or space, which is what makes appending it safe.
func forwardable(snap *redirect.Snapshot, r *http.Request) (string, bool) {
	if snap == nil {
		return "", false
	}
	rest, deep := pathRemainder(r.URL.EscapedPath())
	if !deep {
		return snap.URL, true
	}
	if !snap.ForwardPath {
		return "", false
	}
	return appendPath(snap.URL, rest)
}

// pathRemainder returns whatever follows the alias segment, still escaped, and
// whether there was a separator at all.
//
// The two are not the same question: "/abc" and "/abc/" both have an empty
// remainder, and only the second one is a request for something beneath the
// alias.
//
// Sliced out of the escaped path rather than read back from the router,
// because the alias segment may itself be percent-encoded and the two
// spellings must not have to agree.
func pathRemainder(escaped string) (string, bool) {
	trimmed := strings.TrimPrefix(escaped, "/")
	i := strings.IndexByte(trimmed, '/')
	if i < 0 {
		return "", false
	}
	return trimmed[i+1:], true
}

// appendPath joins a visitor's extra path segments onto the destination.
//
// The escaped remainder is concatenated verbatim and the result's Path and
// RawPath are set together, so url.URL.String emits the bytes that arrived
// instead of re-encoding them. This is the same rule appendRaw follows for the
// query half: a destination the parser cannot round-trip must not be rewritten
// on its way past.
//
// The origin cannot move, and that is structural rather than checked. Nothing
// here touches u.Scheme or u.Host, the joined path always begins with a single
// '/', and the remainder is never resolved as a reference — url.ResolveReference
// would turn a remainder of "//evil.example" into a different host, which is
// precisely the shape the property test refutes.
func appendPath(target, rest string) (string, bool) {
	if !joinable(rest) {
		return "", false
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", false
	}
	joined := strings.TrimSuffix(u.EscapedPath(), "/") + "/" + rest
	unescaped, err := url.PathUnescape(joined)
	if err != nil {
		return "", false
	}
	u.Path, u.RawPath = unescaped, joined
	return u.String(), true
}

// joinable reports whether a remainder may be appended at all.
//
// Dot segments are refused rather than resolved. A browser normalizes them
// before it asks for anything — and the URL standard counts "%2e" and "%2E" as
// dots too, which is how one reaches us at all: ServeMux cleans the escaped
// path and redirects, so the literal spellings never arrive and only the
// encoded ones do.
//
// Refusing is the whole point. Resolving would let /{alias}/../../secret walk
// out of the subtree the owner pointed at, and silently dropping the segments
// would send the visitor somewhere they did not ask for while looking like it
// worked. A 404 says what happened.
func joinable(rest string) bool {
	for seg := range strings.SplitSeq(rest, "/") {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return false
		}
		if decoded == "." || decoded == ".." {
			return false
		}
	}
	return true
}

// appendQuery merges the incoming query string into the destination.
//
// The destination's own parameters win on conflict: they were configured
// deliberately, whereas the incoming ones are whatever a visitor arrived with.
//
// The destination's query is only ever re-encoded when it round-trips exactly.
// url.Query() discards ParseQuery's error and drops every pair it could not
// read, so a destination holding a bare semicolon or a stray percent — both of
// which browsers accept and neither of which ValidateDestination rejects —
// silently lost those parameters the moment forwarding was switched on. The
// link worked with forward_query off and broke with it on, which points
// suspicion at exactly the wrong place. When the destination cannot be parsed
// losslessly, its raw query is preserved verbatim and the incoming pairs are
// appended textually instead.
func appendQuery(target, incoming string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	extra, err := url.ParseQuery(incoming)
	if err != nil {
		return target
	}

	existing, parseErr := url.ParseQuery(u.RawQuery)
	if parseErr != nil || countPairs(u.RawQuery) != countValues(existing) {
		return appendRaw(u, extra)
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

// appendRaw adds parameters to a query that cannot be safely re-encoded,
// leaving the original bytes untouched.
func appendRaw(u *url.URL, extra url.Values) string {
	// Skip anything whose name already appears in the raw query. A textual
	// check is coarser than url.Values.Has, and coarse in the safe direction:
	// the destination's own parameters are the ones that must win.
	add := url.Values{}
	for k, vs := range extra {
		if strings.Contains("&"+u.RawQuery+"&", "&"+url.QueryEscape(k)+"=") ||
			strings.HasPrefix(u.RawQuery, url.QueryEscape(k)+"=") {
			continue
		}
		add[k] = vs
	}
	if len(add) == 0 {
		return u.String()
	}
	encoded := add.Encode()
	if u.RawQuery == "" {
		u.RawQuery = encoded
	} else {
		u.RawQuery += "&" + encoded
	}
	return u.String()
}

// countPairs and countValues detect a lossy parse.
//
// Comparing the parsed pair count to the raw one catches anything ParseQuery
// dropped without saying so. A straight string comparison would not work:
// Encode sorts and re-escapes, so "b=2&a=1" differs from its own round trip
// while losing nothing.
func countPairs(raw string) int {
	n := 0
	for _, part := range strings.Split(raw, "&") {
		if part != "" {
			n++
		}
	}
	return n
}

func countValues(v url.Values) int {
	n := 0
	for _, vs := range v {
		n += len(vs)
	}
	return n
}

func clientIPString(r *http.Request) string {
	if addr := ClientIPFrom(r.Context()); addr.IsValid() {
		return addr.String()
	}
	return ""
}
