package observability

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/DevOfPie/LinkCtrl/internal/build"
)

// Metrics is the instrument panel, built once and passed explicitly.
//
// Its own registry rather than prometheus.DefaultRegisterer: a global registry
// makes two instances in one test process collide, and it lets any dependency
// that happens to import client_golang publish into our namespace. Passing the
// struct also means every metric has one obvious definition site.
//
// Every method is nil-safe. Tests and the CLI build routers without metrics,
// and an instrumentation call site should not have to know whether metrics
// happen to be enabled.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	redirectDuration *prometheus.HistogramVec
	redirects        *prometheus.CounterVec

	throttled *prometheus.CounterVec

	jobRuns    *prometheus.CounterVec
	jobLastRun *prometheus.GaugeVec

	auditBytes prometheus.Gauge
}

// redirectBuckets straddle the 20ms cached-redirect target, densely below it
// and sparsely above.
//
// Default buckets start at 5ms and jump to 10ms, which would put the entire
// interesting range — a cached redirect should be well under a millisecond
// server-side — into a single bucket, and p99 estimates read off it would be
// meaningless. The 0.02 boundary is present because that is the number the SLO
// names, so "fraction under target" is a single ratio of bucket counts.
var redirectBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 1,
}

// httpBuckets cover the API and dashboard budgets (150ms and 250ms) rather
// than the redirect one.
var httpBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5}

// NewMetrics builds the registry and registers every collector.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_http_requests_total",
			Help: "HTTP requests by surface, method and status class.",
		}, []string{"surface", "method", "status"}),

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "linkctrl_http_request_duration_seconds",
			Help:    "Wall-clock time to serve a request, including middleware.",
			Buckets: httpBuckets,
		}, []string{"surface", "method"}),

		// The SLO metric. `cache` is what makes "cached redirect, p99 under
		// 20ms" answerable from the server side: memory and redis are hits,
		// database is a miss, negative is a cached miss.
		redirectDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "linkctrl_redirect_duration_seconds",
			Help:    "Time to resolve and answer a short link, by outcome and cache tier.",
			Buckets: redirectBuckets,
		}, []string{"outcome", "cache"}),

		redirects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_redirects_total",
			Help: "Short-link requests by outcome and cache tier.",
		}, []string{"outcome", "cache"}),

		// Labelled by which limit fired rather than by client, deliberately: a
		// label per address would let anyone mint unbounded series, and the
		// question an alert asks is "is something being throttled", not "who".
		throttled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_rate_limited_total",
			Help: "Requests refused by a rate limit, by limit name.",
		}, []string{"limit"}),

		jobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_job_runs_total",
			Help: "Background job executions by name and result. Only the leader runs them.",
		}, []string{"job", "result"}),

		jobLastRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "linkctrl_job_last_success_timestamp_seconds",
			Help: "Unix time of each job's last success. Absent means it has never succeeded.",
		}, []string{"job"}),

		auditBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "linkctrl_audit_log_bytes",
			Help: "On-disk size of audit_logs across every partition, including indexes. " +
				"Audit retention defaults to keeping everything, so this only ever grows until AUDIT_RETENTION_DAYS is set.",
		}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "linkctrl_build_info",
		Help: "Always 1; the labels carry the build identity.",
	}, []string{"version", "commit", "go"})
	info := build.Get()
	buildInfo.WithLabelValues(info.Version, info.ShortCommit(), info.GoVersion).Set(1)

	reg.MustRegister(
		m.httpRequests, m.httpDuration,
		m.redirectDuration, m.redirects,
		m.throttled,
		m.jobRuns, m.jobLastRun,
		m.auditBytes,
		buildInfo,
		// Go runtime and process collectors: memory, goroutines, GC, file
		// descriptors, CPU. Free, standard, and the first thing anyone asks
		// for when a container starts misbehaving.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Register adds a collector that reads live state — pool statistics, queue
// depth — rather than being written to by instrumentation.
func (m *Metrics) Register(c prometheus.Collector) {
	if m == nil {
		return
	}
	m.registry.MustRegister(c)
}

// Handler serves the scrape endpoint.
//
// This is mounted on the metrics listener, never on the public one: the series
// below expose queue depths, pool saturation and the shape of traffic, which
// is operational detail rather than something to publish.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A broken collector should show up as an error in the scrape rather
		// than as a silently missing series.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Gather exposes the registry for tests.
func (m *Metrics) Gather() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// --- HTTP ------------------------------------------------------------------

// Surface is the coarse bucket a request belongs to.
//
// Deliberately coarse. A label per URL path would let anyone mint unbounded
// series by requesting random aliases — the classic way a metrics endpoint
// becomes the reason a server falls over — and the redirect tree's whole
// namespace is attacker-chosen. Per-route detail for the API lives in the
// access log, which is sampled and does not accumulate.
type Surface string

const (
	SurfaceRedirect Surface = "redirect"
	SurfaceAPI      Surface = "api"
	SurfaceWeb      Surface = "web"
	SurfaceStatic   Surface = "static"
	SurfaceOps      Surface = "ops"
)

// webPrefixes are the dashboard's own paths. Anything not matched here, not
// under /api or /static, and not an operational endpoint is a short link.
var webPrefixes = []string{
	"/login", "/logout", "/setup", "/dashboard", "/docs",
	"/links", "/keys", "/account",
}

// ClassifySurface maps a request path to its surface.
func ClassifySurface(path string) Surface {
	switch {
	case path == "/healthz" || path == "/readyz":
		return SurfaceOps
	case strings.HasPrefix(path, "/api/"):
		return SurfaceAPI
	case strings.HasPrefix(path, "/static/"):
		return SurfaceStatic
	case path == "/":
		return SurfaceWeb
	}
	for _, p := range webPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return SurfaceWeb
		}
	}
	return SurfaceRedirect
}

// statusClass reduces a status code to its class, e.g. 404 to "4xx".
//
// Three-digit codes would be a fine label too, but the class is what alerts
// are written against and it keeps the series count flat as new codes appear.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// statusRecorder captures the status code for the metrics middleware.
//
// Unwrap lets http.ResponseController reach the underlying writer, so wrapping
// does not break flushing or hijacking — the reason hand-rolled wrappers
// usually have to be replaced later.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// HTTPMiddleware counts and times every request.
//
// Placed outermost, so the numbers include session lookup, CSRF checks and
// everything else a handler does not control. The redirect surface is also
// measured in finer detail inside its handler; this one is the outside view.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		surface := string(ClassifySurface(r.URL.Path))
		code := rec.code
		if code == 0 {
			// A handler that wrote nothing still produced 200 on the wire.
			code = http.StatusOK
		}
		m.httpRequests.WithLabelValues(surface, r.Method, statusClass(code)).Inc()
		m.httpDuration.WithLabelValues(surface, r.Method).Observe(time.Since(start).Seconds())
	})
}

// --- redirects --------------------------------------------------------------

// ObserveRedirect records one short-link request.
//
// Called from the redirect handler with the duration it already measures for
// the click event, so instrumentation adds a map lookup and a histogram
// observation — tens of nanoseconds against a 20ms budget.
//
// One measurement caveat, verified rather than assumed: on a Windows host Go's
// monotonic clock cannot resolve an interval this short, and time.Since returns
// exactly zero for 100,000 out of 100,000 back-to-back samples. A cache-served
// redirect therefore lands in the zero bucket, making _sum and any average
// useless locally. Bucket counts, and so the "fraction under 20ms" ratio the
// SLO is stated as, remain correct — and the SLO itself is measured on Linux in
// containers, where the clock has nanosecond resolution.
func (m *Metrics) ObserveRedirect(outcome, cache string, d time.Duration) {
	if m == nil {
		return
	}
	if cache == "" {
		// A resolver error answers 404 without ever reaching a tier. Labels
		// must never be empty, or the series is hard to query.
		cache = "none"
	}
	m.redirectDuration.WithLabelValues(outcome, cache).Observe(d.Seconds())
	m.redirects.WithLabelValues(outcome, cache).Inc()
}

// --- rate limits ------------------------------------------------------------

// ObserveThrottled records one request refused by a rate limit.
//
// The label names the limit — "login", "api", "redirect_404" — not the client.
// That is what makes the series bounded, and it is also the more useful cut: an
// operator wants to know that logins are being throttled, and finds out who from
// the log if it matters.
func (m *Metrics) ObserveThrottled(limit string) {
	if m == nil {
		return
	}
	m.throttled.WithLabelValues(limit).Inc()
}

// --- jobs -------------------------------------------------------------------

// ObserveJob records a background job run.
func (m *Metrics) ObserveJob(job string, err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.jobRuns.WithLabelValues(job, "error").Inc()
		return
	}
	m.jobRuns.WithLabelValues(job, "ok").Inc()
	m.jobLastRun.WithLabelValues(job).Set(float64(time.Now().Unix()))
}

// ObserveJobSkipped records a run that another replica held the lock for.
//
// Counted rather than ignored: on a healthy multi-replica deployment most runs
// are skips, and a follower that never skips is a follower that never tried.
func (m *Metrics) ObserveJobSkipped(job string) {
	if m == nil {
		return
	}
	m.jobRuns.WithLabelValues(job, "skipped").Inc()
}

// SetAuditLogBytes records the audit log's on-disk size.
//
// A plain gauge rather than a collector that queries at scrape time, because
// /metrics has to keep answering while the database is unwell — it is the
// endpoint an operator scrapes to find out that it is. The cost is that the
// value is up to an hour stale, which does not matter for a series whose whole
// purpose is a growth trend measured in days.
//
// Set by every replica, not only the job leader. A gauge only the leader wrote
// would read as zero on every follower, so whether an alert fired would depend
// on which replica answered the scrape.
func (m *Metrics) SetAuditLogBytes(n int64) {
	if m == nil {
		return
	}
	m.auditBytes.Set(float64(n))
}
