package observability

import (
	"net/http"
	"strconv"
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

	jobRuns      *prometheus.CounterVec
	jobLastRun   *prometheus.GaugeVec
	jobStaleness *prometheus.GaugeVec

	auditBytes prometheus.Gauge

	feedChecks *prometheus.CounterVec

	webhookDeliveries *prometheus.CounterVec
	automationFirings *prometheus.CounterVec

	addonLoads        *prometheus.CounterVec
	addonInfo         *prometheus.GaugeVec
	addonRefusals     *prometheus.CounterVec
	addonSchemaBytes  *prometheus.GaugeVec
	addonLargeObjects *prometheus.GaugeVec
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

		// The durable counterpart of the gauge above, and the one M37's split
		// cadence needs. jobLastRun is process-local: it is set by whichever
		// replica ran the job, it is absent on the others, and it is forgotten on
		// restart — so on a rolling deploy a stalled job reads as healthy on
		// whichever replica happens to be scraped. This one is read out of
		// job_state, which every replica shares and no restart clears.
		//
		// Seconds-since rather than a timestamp because the thing being alerted on
		// is an age, and an age computed in PromQL against a clock that is not the
		// database's is an age with two clocks in it.
		jobStaleness: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "linkctrl_rollup_staleness_seconds",
			Help: "Seconds since each background job last succeeded, as recorded in " +
				"job_state and therefore shared by every replica. A job that has " +
				"never succeeded has no series at all.",
		}, []string{"job"}),

		auditBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "linkctrl_audit_log_bytes",
			Help: "On-disk size of audit_logs across every partition, including indexes. " +
				"Audit retention defaults to keeping everything, so this only ever grows until AUDIT_RETENTION_DAYS is set.",
		}),

		// The opt-in reputation feed (M32). Absent entirely on a default
		// instance, because nothing increments it until a feed is configured —
		// which makes the series itself the answer to "is this box sending
		// destinations anywhere".
		//
		// `error` is the label that matters operationally: a feed failure fails
		// open to the built-in tiers, so an outage at the third party is
		// invisible in the product's behaviour and visible only here.
		feedChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_destination_feed_checks_total",
			Help: "Third-party reputation feed checks by result: clean, malicious, " +
				"error (the feed did not answer usefully; the check fails open), or " +
				"skipped (the instance owner has allowed that host).",
		}, []string{"result"}),

		// Webhook delivery (M42). Two bounded labels and no third, which is what
		// M13's cardinality rule buys here: `outcome` is one of four words and
		// `status` is one of five classes, so the whole metric is at most twenty
		// series however many webhooks exist on the instance.
		//
		// A URL label is the obvious thing to want and the thing that must not
		// be here. Registrations are chosen by users, there is no ceiling on how
		// many distinct hosts they name across an instance, and a label with
		// that property is a way for anybody with a workspace to grow the
		// scrape. Which webhook failed is a question the delivery log answers,
		// per workspace, where it belongs.
		//
		// `status="none"` is the interesting one: no response at all — a refused
		// connection, a timeout, or this instance declining to open the socket
		// because the name resolved somewhere private.
		webhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_webhook_deliveries_total",
			Help: "Webhook delivery attempts by outcome (delivered, retry, abandoned) " +
				"and HTTP status class (2xx, 3xx, 4xx, 5xx, or none when there was no response).",
		}, []string{"outcome", "status"}),

		// Automation firings (M43). Two bounded labels for the same reason, and
		// the bound is tighter: `trigger` is one of three names from a closed
		// vocabulary and `outcome` is one of two words, so the whole metric is
		// six series however many rules exist.
		//
		// A rule name is the obvious thing to want and the thing that must not be
		// here — rules are named by users and there is no ceiling on how many
		// distinct names an instance accumulates. Which rule fired is a question
		// the audit log answers, per workspace, where it belongs.
		//
		// Counting only firings and not evaluations is deliberate. A rule that
		// matched nothing is the expected case on every tick, and a counter that
		// incremented for it would be a counter whose rate says how often the
		// scheduler ran rather than how much automation is happening.
		automationFirings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_automation_firings_total",
			Help: "Automation rule firings by trigger (link.expired, link.max_clicks, " +
				"destination.blocked) and outcome (fired, or partial when at least one " +
				"action failed).",
		}, []string{"trigger", "outcome"}),

		// Add-ons (M60, and the refusal counter below is M62's). Every `addon` label
		// in the three is bounded by how many modules an operator
		// installed — plus one, for the sentinel named below — which is the first
		// metric in this file whose cardinality is set by the deployment rather
		// than by a closed vocabulary in the code, and it is bounded all the same,
		// because installing an add-on is an operator action against a directory
		// and not something a tenant can do.
		//
		// `outcome` is addon.Outcome and is seven words: loaded, manifest_invalid,
		// abi_unsupported, checksum_mismatch, module_unreadable,
		// instantiate_failed, storage_failed. No error
		// string ever reaches a label. `addon` comes from the validated manifest
		// on the loaded path and from the *directory entry* on the refusal path,
		// where there is no manifest to take it from — bounded there by the host,
		// which publishes addon.InvalidName rather than a raw entry its own
		// regexp refuses. That bound is load-bearing twice: WithLabelValues
		// below panics on a label value that is not valid UTF-8, and a directory
		// name is not a name until something says so.
		addonLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_addon_loads_total",
			Help: "Add-on load attempts by add-on and outcome (loaded, manifest_invalid, " +
				"abi_unsupported, checksum_mismatch, module_unreadable, instantiate_failed, " +
				"storage_failed).",
		}, []string{"addon", "outcome"}),

		// The identity series, modelled on linkctrl_build_info: always 1, and the
		// labels are the answer. It exists only for add-ons that instantiated —
		// a refusal is a counter increment above, not an add-on with an identity
		// this instance is running.
		addonInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "linkctrl_addon_info",
			Help: "Always 1 per loaded add-on; the labels carry its identity and the " +
				"permissions it holds.",
		}, []string{"addon", "version", "abi_version", "failure_class", "permissions"}),

		// Refused ABI calls (M62). `permission` is addon.abi's closed vocabulary and
		// is six words, so this is bounded at six series per installed add-on — the
		// same deployment-set bound the pair above carries, times a closed set, which
		// is why a per-function label was not added: the permission is what an
		// operator can act on, and the function is in the log line beside it.
		//
		// It counts *undeclared* calls and nothing else. A call refused because the
		// host has not implemented the function yet is not here — a module probing a
		// capability is doing what the ABI invites — and neither is a settings key
		// outside the add-on's own manifest, which is a scope question rather than a
		// permission.
		addonRefusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_addon_refusals_total",
			Help: "ABI calls refused because the add-on did not declare the permission " +
				"the function requires, by add-on and permission.",
		}, []string{"addon", "permission"}),

		// How much disk each add-on's own schema holds (M63). One series per add-on
		// that declared storage, so it is the narrowest of the four and bounded the
		// same way.
		//
		// **It is the whole of this product's answer to add-on storage quotas**, and
		// that is deliberate rather than unfinished: there is no cap on how large an
		// add-on's schema may grow, for the same reason there is none on the audit
		// log, and a default that permits unbounded growth is only defensible if the
		// growth is visible. An operator who installs a module that writes a row per
		// redirect should find that out from a graph rather than from a full disk.
		//
		// **What is visible is data an add-on has stored**, this gauge and the one
		// below between them, and the qualifier is load-bearing rather than
		// cautious. It is also a claim of completeness over stored data, which held
		// only after D254: this gauge summed a *list* of relation kinds, and a
		// sequence — `relkind 'S'`, in the add-on's own schema, 8192 bytes from
		// creation and outside `pg_total_relation_size` of the table that owns it —
		// was not on the list. It now sums every kind except the ones already
		// counted inside another, and a `serial` column's sequence is 8192 bytes the
		// number was quietly short by for a well-behaved add-on all along.
		//
		// Transient disk an add-on's session holds is a different thing and remains
		// neither gauge's subject, with no gauge covering it: a `WITH HOLD` cursor
		// materialized at commit keeps a temporary file in `base/pgsql_tmp` for the
		// life of the session — 553
		// MB measured for one cursor inside the add-on's five-second statement timeout
		// — and both of these read zero throughout. It is not stored data and it is
		// freed when the backend ends, so it is a residual rather than a gap in the
		// quota answer; F279 carries it, and an operator told *the growth is visible*
		// while watching a flat gauge and a filling disk would have been told
		// something untrue.
		//
		// Measured on the maintenance job's schedule and by every replica, like
		// linkctrl_audit_log_bytes and for the same reason: a gauge the followers
		// never set reads as zero, and which replica answered the scrape is not
		// something an operator's alert should depend on.
		addonSchemaBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "linkctrl_addon_schema_bytes",
			Help: "On-disk size of each add-on's own Postgres schema: every relation in " +
				"it with storage — tables, sequences, materialized views — with their " +
				"indexes and TOAST. Nothing caps it; this is what makes the growth visible.",
		}, []string{"addon"}),

		// The other half of that answer, and the reason it is a count rather than a
		// size: a large object is in no schema, so the gauge above cannot see one,
		// and its bytes live in pg_largeobject, which only a superuser may read.
		// Nothing in this product creates one, so any value but zero here is an
		// add-on writing outside its schema.
		addonLargeObjects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "linkctrl_addon_large_objects",
			Help: "Large objects each add-on's database role owns. Always 0 unless an " +
				"add-on created one, which is data outside its schema.",
		}, []string{"addon"}),
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
		m.jobRuns, m.jobLastRun, m.jobStaleness,
		m.auditBytes,
		m.feedChecks,
		m.webhookDeliveries,
		m.automationFirings,
		m.addonLoads, m.addonInfo, m.addonRefusals, m.addonSchemaBytes,
		m.addonLargeObjects,
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
// is operational detail rather than something to publish — and, since M60, the
// name and version of every installed add-on, which is an inventory of what this
// instance runs. docs/SECURITY.md says the same thing to the operator.
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

// webPrefixes are the dashboard's own paths, and they are **not** maintained by
// hand any more.
//
// They were, and the list stopped being updated after Phase 1 while eleven
// dashboard routes were added beside it — so `/notifications`, `/workspaces`,
// `/members`, `/invites`, `/organizations`, `/signup`, `/disputes` and the rest
// all fell through to the classifier's default and were counted as **redirects**
// (F16). Nothing was served differently; what was wrong was the numbers an
// operator reads, because `surface="redirect"` mixed short-link traffic under a
// 20ms budget with dashboard page loads under a 250ms one.
//
// The fix is not a longer list. `SetWebPaths` is called at boot with what the
// application mux was actually handed, so this cannot drift from the routes
// again — a hand-written second copy of a list that already exists is the defect
// rather than the omission. The value here is the Phase 1 set, kept only as the
// answer before boot and for callers with no router: a test, or the classifier
// invoked directly.
var webPrefixes = []string{
	"/login", "/logout", "/setup", "/dashboard", "/docs",
	"/links", "/keys", "/account",
}

// SetWebPaths replaces the dashboard path set with the routes the application
// mux was given.
//
// Called once at boot, before any request is served. Paths arrive as the mux
// spells them — an exact path like `/login`, or a subtree like `/links/` — and
// both are reduced to the prefix form this classifier matches on. The root
// pattern is dropped because `/` is handled explicitly below.
func SetWebPaths(paths []string) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSuffix(p, "/")
		if p == "" || strings.HasPrefix(p, "/{") {
			continue
		}
		out = append(out, p)
	}
	if len(out) > 0 {
		webPrefixes = out
	}
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

// SetJobStaleness records how long ago a job last succeeded.
//
// Set by every replica, like SetAuditLogBytes and for the same reason: this is
// an observation of shared state rather than work that must happen once, and a
// gauge only the leader wrote would make an alert depend on which replica the
// scrape reached.
func (m *Metrics) SetJobStaleness(job string, seconds float64) {
	if m == nil {
		return
	}
	m.jobStaleness.WithLabelValues(job).Set(seconds)
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

// --- reputation feeds --------------------------------------------------------

// ObserveFeedCheck records one third-party reputation check.
//
// The count is what makes a failing feed observable at all. A check that errors
// fails open to the built-in tiers by design, so the destination is accepted and
// nothing in the product's behaviour says the feed stopped answering — an
// operator who enabled a feed and is relying on it would otherwise find out by
// noticing nothing was ever refused.
func (m *Metrics) ObserveFeedCheck(result string) {
	if m == nil {
		return
	}
	m.feedChecks.WithLabelValues(result).Inc()
}

// --- webhooks ----------------------------------------------------------------

// ObserveWebhookDelivery records one delivery attempt (M42).
//
// Both labels come from a closed vocabulary the caller computes: internal/webhook
// reduces an HTTP code to its class before calling, so nothing user-chosen can
// reach a label from here. See the metric's definition for why that matters.
func (m *Metrics) ObserveWebhookDelivery(outcome, status string) {
	if m == nil {
		return
	}
	m.webhookDeliveries.WithLabelValues(outcome, status).Inc()
}

// --- automation --------------------------------------------------------------

// ObserveAutomationFiring records one rule firing (M43).
//
// Called once per firing, not once per subject and not once per evaluation: the
// question this answers is "how much is the scheduler doing on somebody's
// behalf", and a rule that matched forty links did one thing.
func (m *Metrics) ObserveAutomationFiring(trigger, outcome string) {
	if m == nil {
		return
	}
	m.automationFirings.WithLabelValues(trigger, outcome).Inc()
}

// --- add-ons -----------------------------------------------------------------

// ObserveAddonLoad records one add-on load attempt (M60).
//
// Called for every attempt including the refusals, which is the point: an
// operator whose add-on is silently not there needs a series that says so, and a
// counter that only ever counted successes would leave the failure visible in a
// log line nobody is scraping.
func (m *Metrics) ObserveAddonLoad(addon, outcome string) {
	if m == nil {
		return
	}
	m.addonLoads.WithLabelValues(addon, outcome).Inc()
}

// SetAddonInfo publishes the identity of an add-on that loaded (M60), and the
// permissions it holds (M62).
//
// `permissions` is the *held* set, sorted and comma-separated — not what the
// manifest declared, since a permission the vocabulary carries and no host grants
// yet is declarable and not held. Sorted so a series does not change identity
// because a manifest listed the same grants in another order.
func (m *Metrics) SetAddonInfo(addon, version string, abiVersion int, failureClass, permissions string) {
	if m == nil {
		return
	}
	m.addonInfo.WithLabelValues(addon, version, strconv.Itoa(abiVersion), failureClass,
		permissions).Set(1)
}

// ObserveAddonRefusal records one ABI call refused for want of a declared
// permission (M62).
//
// Called from the host's own dispatch, on whatever goroutine the guest is running
// on, which from M66 is a request's. Both labels are bounded: the add-on's name
// comes from a validated manifest and the permission from a closed vocabulary, so
// neither is guest input however the module was written.
func (m *Metrics) ObserveAddonRefusal(addon, permission string) {
	if m == nil {
		return
	}
	m.addonRefusals.WithLabelValues(addon, permission).Inc()
}

// SetAddonSchemaBytes records the on-disk size of one add-on's own schema (M63) —
// every relation in it that has storage, not a list of the kinds somebody thought
// of, which is store.AddonSchemaBytes's own argument and D254's.
//
// Set by every replica, like SetAuditLogBytes and for the reason given there: a
// gauge the followers never set reads as zero. The add-on's name comes from a
// validated manifest, so the label is bounded whatever the module was written to
// do.
func (m *Metrics) SetAddonSchemaBytes(addon string, n int64) {
	if m == nil {
		return
	}
	m.addonSchemaBytes.WithLabelValues(addon).Set(float64(n))
}

// SetAddonLargeObjects records how many large objects one add-on's role owns
// (M63).
//
// Zero for every add-on that behaves, which is why it is published at all: a large
// object is outside every schema, so SetAddonSchemaBytes cannot see one and an
// add-on's growth would otherwise be invisible between loads. Same replica rule as
// the gauge above.
func (m *Metrics) SetAddonLargeObjects(addon string, n int64) {
	if m == nil {
		return
	}
	m.addonLargeObjects.WithLabelValues(addon).Set(float64(n))
}
