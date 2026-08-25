package observability

import (
	"net/http"
	"slices"
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
	addonRedirect     *prometheus.HistogramVec
	addonRedirectKill *prometheus.CounterVec
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
		//
		// **It is core's own work and an inline add-on's time is not in it** (M66).
		// The handler subtracts the extension point before observing here, so this
		// is the series docs/slo.md's figure is taken from and the series that
		// stays comparable across installing an add-on. What the visitor waited is
		// linkctrl_http_request_duration_seconds and the click's own latency, both
		// of which include whatever a module spent.
		redirectDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "linkctrl_redirect_duration_seconds",
			Help: "Time to resolve and answer a short link, by outcome and cache tier. " +
				"Excludes time an inline add-on held the redirect, which is on " +
				"linkctrl_addon_redirect_duration_seconds.",
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
			Help: "Requests refused by a rate limit, or work a concurrency bound would " +
				"not admit, by limit name.",
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
		// `outcome` is addon.Outcome and is nine words: loaded, manifest_invalid,
		// abi_unsupported, checksum_mismatch, module_unreadable,
		// instantiate_failed, storage_failed, name_collision, load_timeout. No error
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
				"storage_failed, name_collision, load_timeout).",
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

		// Refused ABI calls (M62). `permission` is the set of distinct Requires values
		// across addon.abi's functions, which is narrower than the seven-token
		// vocabulary, so this is bounded at six series per installed add-on — the
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
		// that declared storage, so it is among the narrowest here and bounded the
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

		// What each add-on costs the redirect path, per module (M66), and it is the
		// owner's second requirement on the redirect answer stated as a series:
		// *so an operator can efficiently track a problem down and report it to the
		// right team*. **Separate from linkctrl_redirect_duration_seconds and never
		// folded into it**, because the whole point is that core's p99 and each
		// add-on's p99 are different curves. A single number covering both would
		// answer "is this instance slow" and never "whose fault is it", which is the
		// question this exists for.
		//
		// It measures the *invocation* — instantiating the module and calling it —
		// and not the redirect around it. **The separation is enforced from the
		// other side too**, and it has to be or this series would be a duplicate of
		// what the redirect histogram already contained: `RedirectHandler.ServeHTTP`
		// subtracts the whole extension point from the reading it hands
		// [Metrics.ObserveRedirect], so core's curve is this product's own work and
		// the two sit beside each other rather than one enclosing the other. What
		// is taken out of core is slightly more than what arrives here — a killed
		// invocation costs the deadline and is deliberately absent below — so the
		// two do not sum to the wall time, and neither is meant to: the question
		// they answer together is *whose latency is this*, not *where did every
		// microsecond go*.
		//
		// An invocation that never ran is absent rather than zero: a skipped one is
		// on the throttled series and a killed one is on the counter below, and a
		// bucket of zeroes would drag a p99 towards a latency nobody experienced.
		//
		// The same buckets the redirect histogram uses, so an operator reading the
		// two beside each other is reading one scale. `class` is the closed pair
		// inline/observe and `addon` is a validated manifest name, so the series
		// count is bounded by how many modules an operator installed times two.
		addonRedirect: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "linkctrl_addon_redirect_duration_seconds",
			Help: "Time one add-on spent on a redirect, by add-on and class (inline, " +
				"observe). Separate from linkctrl_redirect_duration_seconds so core's " +
				"latency and each add-on's are different curves.",
			Buckets: redirectBuckets,
		}, []string{"addon", "class"}),

		// The availability half, and it is the number the owner's boundary rests on:
		// an add-on's latency is its own, and an add-on that never returns is killed
		// so the instance's availability stays this product's. A non-zero rate here
		// is an add-on to go and fix, and it is what makes a thrashing module an
		// operator-visible fact rather than a mystery slowdown.
		//
		// Kills only. A module that was skipped because the host was saturated is on
		// linkctrl_rate_limited_total, and one that trapped is in the log: neither is
		// an overrun, and counting them here would put a number on the Add-on manager
		// that blames the wrong thing.
		//
		// `step` is the closed pair instantiate/call, and it is F326's half of this
		// series. The two are different faults with different owners: a kill at
		// `call` is the add-on holding a redirect open past its deadline, which is
		// what this counter was built for, and a kill at `instantiate` is this host
		// failing to *start* the module inside a bound of its own — a cold or slow
		// machine rather than a slow add-on. Sharing one number made them
		// indistinguishable, and an instance where every invocation died at
		// `instantiate` looked exactly like an add-on that had declined to act.
		addonRedirectKill: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkctrl_addon_redirect_kills_total",
			Help: "Add-on invocations killed for overrunning a redirect bound, by " +
				"add-on and step (instantiate, call). The redirect completed without " +
				"them.",
		}, []string{"addon", "step"}),
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
		m.addonLargeObjects, m.addonRedirect, m.addonRedirectKill,
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
// Called from the redirect handler with the elapsed time **less whatever an
// inline add-on held the redirect for** (M66), so instrumentation adds a map
// lookup and a histogram observation — tens of nanoseconds against a 20ms
// budget. That subtraction is why this no longer takes the same number the click
// event carries: the click records what the visitor waited, and this records what
// this product's own work cost.
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

// ForgetAddon drops the gauges that describe an add-on this instance no longer
// runs (M67).
//
// **Gauges only, and the counters deliberately stay.** `linkctrl_addon_info` and
// the two size gauges are statements about the present tense — *this instance
// runs this module, and its schema is this big* — and leaving them behind after a
// removal makes each one a lie that reads exactly like a fact. The counters are
// statements about the past: `linkctrl_addon_loads_total` and
// `linkctrl_addon_refusals_total` describe attempts that really happened, and
// deleting them would erase the record of an add-on that was installed and
// removed, which is the history an operator reading a scrape after an incident
// most needs.
//
// The info series is deleted by exact label values because that is the only way
// to address one: its identity is every label it carries. The caller therefore
// passes what it published, which is why this takes five arguments rather than a
// name.
//
// **The two size gauges go too, and that is the answer to a real question.**
// Removal leaves the add-on's schema behind — an orphan M63 makes enumerable and
// M68 offers to purge — so there is an argument for keeping the number. It loses,
// because nothing sets it any more: the maintenance job measures the *loaded*
// add-ons, so a gauge left standing is a value frozen at the last measurement
// before the removal, and a frozen gauge reads exactly like a live one. Silence
// is the honest answer, and what an operator reads instead is the boot warning
// naming every schema no loaded module owns.
func (m *Metrics) ForgetAddon(addon, version string, abiVersion int,
	failureClass, permissions string) {
	if m == nil {
		return
	}
	m.addonInfo.DeleteLabelValues(addon, version, strconv.Itoa(abiVersion),
		failureClass, permissions)
	m.addonSchemaBytes.DeleteLabelValues(addon)
	m.addonLargeObjects.DeleteLabelValues(addon)
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

// ObserveAddonRedirect records what one add-on cost one redirect (M66).
//
// Called from the redirect path itself for the inline class and from the
// out-of-band worker for the observe one, so it is on the hot path and is a
// label lookup and an observation, exactly like ObserveRedirect beside it.
func (m *Metrics) ObserveAddonRedirect(addon, class string, d time.Duration) {
	if m == nil {
		return
	}
	m.addonRedirect.WithLabelValues(addon, class).Observe(d.Seconds())
}

// ObserveAddonRedirectKill counts an invocation the host stopped waiting for, at
// the step whose bound it overran.
//
// step is addon.StepInstantiate or addon.StepCall, spelled there rather than here
// because that package is the one that knows which bound applies to which step.
func (m *Metrics) ObserveAddonRedirectKill(addon, step string) {
	if m == nil {
		return
	}
	m.addonRedirectKill.WithLabelValues(addon, step).Inc()
}

// SetAddonLargeObjects records how many large objects one add-on's role owns
// (M63).
//
// Zero for every add-on that behaves, which is why it is published at all: a large
// object is outside every schema, so SetAddonSchemaBytes cannot see one and an
// add-on's growth would otherwise be invisible between loads. Same replica rule as
// SetAddonSchemaBytes.
func (m *Metrics) SetAddonLargeObjects(addon string, n int64) {
	if m == nil {
		return
	}
	m.addonLargeObjects.WithLabelValues(addon).Set(float64(n))
}

// --- add-on performance, read back --------------------------------------------

// AddonRedirectStats is what one add-on cost the redirect path, per class.
//
// The Add-on manager (M68) renders these on the page itself rather than linking to
// `/metrics`, which is the checkable form of the owner's "attribution without
// Prometheus": an operator asking *which add-on is slowing my redirects* gets an
// answer from the product they are already looking at, on an instance that scrapes
// nothing.
type AddonRedirectStats struct {
	// Class is `inline` or `observe` — addon.ClassInline and ClassObserve, spelled
	// there rather than here.
	Class string `json:"class"`
	// Count is how many invocations are behind the estimate. Rendered beside it,
	// because a p99 over four observations is a number with no meaning and the page
	// must not present one as though it had.
	Count uint64 `json:"count"`
	// P99 is the estimate read off the histogram's buckets.
	//
	// A Duration in Go and **seconds in JSON**, which is why the two fields below
	// exist rather than a tag on this one: a Duration marshals as a nanosecond
	// integer, and the series this is read from is `_seconds`. A client comparing
	// this answer against a scrape should not have to divide.
	P99 time.Duration `json:"-"`
	// Sum is the total time observed, so a mean is available without a second
	// gather.
	Sum time.Duration `json:"-"`

	// P99Seconds and SumSeconds are the two above as the wire carries them, filled
	// in beside them so that nothing has to convert at the call site and the two
	// cannot come to disagree.
	P99Seconds float64 `json:"p99_seconds"`
	SumSeconds float64 `json:"sum_seconds"`
}

// AddonKills is how many invocations of one add-on the host stopped waiting for,
// split by the step whose bound they overran (F326's split — the two are different
// faults with different owners).
type AddonKills struct {
	Instantiate uint64 `json:"instantiate"`
	Call        uint64 `json:"call"`
}

// Total is what the manager's list column shows: one number, because the list has
// one column and the split belongs on the detail page.
func (k AddonKills) Total() uint64 { return k.Instantiate + k.Call }

// AddonPerformance is one add-on's redirect-path record.
type AddonPerformance struct {
	// Classes is one entry per class this add-on has actually been observed in,
	// ordered inline-then-observe. **A class with no observations is absent rather
	// than zero**, which is what makes m68.md's "modules holding no redirect grant
	// show no redirect figures rather than zeros" expressible: an add-on that never
	// ran on the redirect path has an empty slice, and the page draws a dash.
	Classes []AddonRedirectStats `json:"classes,omitempty"`
	Kills   AddonKills           `json:"kills"`
}

// Observed reports whether this add-on has any redirect-path record at all.
func (p AddonPerformance) Observed() bool {
	return len(p.Classes) > 0 || p.Kills.Total() > 0
}

// AddonPerformance reads the two per-module series M66 publishes and returns them
// by add-on name.
//
// # Why this reads the registry instead of keeping its own numbers
//
// The alternative is a second set of counters written beside the Prometheus ones,
// and it was rejected for the reason a second store of anything is: the page and
// the scrape would be two answers to one question, and the first time they
// disagreed the disagreement would be the thing an operator had to debug. What is
// published *is* the record; this asks it.
//
// The cost is a `Gather()` per page render — every collector in the registry,
// including the Go runtime's — which is a few hundred microseconds and is on an
// authenticated dashboard page with a 250 ms budget, not on the redirect path. The
// manager is the only caller.
//
// # The p99 is the same estimate `histogram_quantile` makes, and it is an estimate
//
// A Prometheus histogram keeps bucket counts, not observations, so the quantile is
// interpolated linearly inside whichever bucket the rank falls in — the same
// arithmetic PromQL does, reproduced here rather than approximated differently, so
// the number on the page and the number on a dashboard agree. Two consequences are
// stated rather than left to be discovered: an estimate inside the last finite
// bucket cannot exceed that bucket's boundary, and one whose rank lands in `+Inf`
// saturates at that boundary, because there is no upper bound to interpolate
// towards.
//
// **A saturated estimate is not marked as one.** It is returned as an ordinary
// Duration and `shortDuration` prints it as an ordinary figure, so a p99 in the
// `+Inf` bucket reads as exactly the last boundary — 1s, `redirectBuckets` — and
// understates whatever it actually was. What keeps that far away in a shipped
// configuration is the deadlines rather than this function: a successful
// invocation of either class spends at most `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`
// plus `LINKCTRL_ADDON_INLINE_DEADLINE` under a bound, 525 ms together by default,
// and a killed one is not observed here at all. An operator who raises either past
// a second reaches the last bucket outright, and F331 carries what the page should
// say when they do.
//
// **Bounded is not the same as all of it**, and the difference is stated rather
// than rounded away: the observed window closes after the instance is released,
// and releasing one copies the guest's linear memory back over itself — up to
// `maxGuestMemoryPages`, on this host's own CPU, under no deadline at all. It is
// microseconds in practice and it is a memcpy rather than guest execution, so
// nothing about an add-on's own code can stretch it; but a machine under enough
// pressure to make it matter is a machine where the two deadlines above are not
// the whole story either. The claim this comment makes is therefore about the
// deadlines and not about the histogram's window, which is wider than they are.
//
// It is also **cumulative since this process started**, not a rate over a window:
// there is no time series here to take a rate of. An add-on that was slow an hour
// ago and is fine now still reads slow, and the manager says as much beside the
// figure rather than implying a live reading.
func (m *Metrics) AddonPerformance() map[string]AddonPerformance {
	if m == nil {
		return nil
	}
	families, err := m.registry.Gather()
	if err != nil && families == nil {
		// A partial gather still carries what it managed to collect, and a page that
		// refused to render because one collector failed would be the wrong trade —
		// the same reasoning the shell's badge query uses.
		return nil
	}
	out := map[string]AddonPerformance{}
	for _, f := range families {
		switch f.GetName() {
		case "linkctrl_addon_redirect_duration_seconds":
			for _, metric := range f.GetMetric() {
				name, class := labelOf(metric.GetLabel(), "addon"), labelOf(metric.GetLabel(), "class")
				h := metric.GetHistogram()
				if name == "" || h == nil || h.GetSampleCount() == 0 {
					continue
				}
				p := out[name]
				p99 := bucketQuantile(h.GetSampleCount(), h.GetBucket(), 0.99)
				p.Classes = append(p.Classes, AddonRedirectStats{
					Class:      class,
					Count:      h.GetSampleCount(),
					P99:        seconds(p99),
					Sum:        seconds(h.GetSampleSum()),
					P99Seconds: p99,
					SumSeconds: h.GetSampleSum(),
				})
				out[name] = p
			}
		case "linkctrl_addon_redirect_kills_total":
			for _, metric := range f.GetMetric() {
				name, step := labelOf(metric.GetLabel(), "addon"), labelOf(metric.GetLabel(), "step")
				n := uint64(metric.GetCounter().GetValue())
				if name == "" || n == 0 {
					continue
				}
				p := out[name]
				switch step {
				case "instantiate":
					p.Kills.Instantiate += n
				case "call":
					p.Kills.Call += n
				}
				out[name] = p
			}
		}
	}
	// Ordered so a page does not reorder its own rows between renders. `inline`
	// before `observe`, which is the order they cost a visitor in.
	for name, p := range out {
		slices.SortFunc(p.Classes, func(a, b AddonRedirectStats) int {
			return strings.Compare(classRank(a.Class), classRank(b.Class))
		})
		out[name] = p
	}
	return out
}

// classRank puts inline first and observe second, whatever they are called.
func classRank(class string) string {
	if class == "inline" {
		return "0" + class
	}
	return "1" + class
}

// labelOf reads one label off a gathered metric.
//
// Generic over the label pair rather than taking `[]*dto.LabelPair`, and the
// three functions below take the same shape for the same reason: naming a
// `client_model` type in a signature makes it a **direct** module dependency,
// and two guards in this repository assert that the direct set did not grow for
// a milestone that had no reason to grow it (M49's and M53's). The package is
// already in the graph — client_golang requires it — so what is avoided is a
// go.mod line claiming this milestone added a module, not a download. The
// constraint is what `Gather` actually hands back, so it is not a loosening.
func labelOf[L interface {
	GetName() string
	GetValue() string
}](labels []L, name string) string {
	for _, l := range labels {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// seconds turns a float of seconds into a Duration without going through a string.
func seconds(f float64) time.Duration {
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// bucketQuantile is `histogram_quantile` over one gathered histogram.
//
// Prometheus's own algorithm, and deliberately not a simplification of it: find
// the bucket the rank falls in, then interpolate linearly between that bucket's
// lower and upper boundaries by how far into the bucket's own count the rank
// sits. The three edge cases it inherits are the three that matter here — a rank
// in the first bucket interpolates from zero rather than from negative infinity, a
// rank in `+Inf` returns the last finite boundary, and an empty histogram returns
// zero.
//
// Takes the count and the buckets rather than the histogram, on [labelOf]'s
// reasoning about the dependency set.
func bucketQuantile[B interface {
	GetCumulativeCount() uint64
	GetUpperBound() float64
}](total uint64, buckets []B, q float64) float64 {
	if total == 0 {
		return 0
	}
	rank := q * float64(total)

	var prevCount uint64
	var prevBound float64
	for _, b := range buckets {
		if float64(b.GetCumulativeCount()) >= rank {
			upper := b.GetUpperBound()
			inBucket := b.GetCumulativeCount() - prevCount
			if inBucket == 0 {
				return upper
			}
			return prevBound + (upper-prevBound)*((rank-float64(prevCount))/float64(inBucket))
		}
		prevCount, prevBound = b.GetCumulativeCount(), b.GetUpperBound()
	}
	// The rank is in the +Inf bucket: everything finite is below it and there is no
	// upper bound to interpolate towards. The last finite boundary is the most this
	// data supports saying, and it is returned unmarked — the caller cannot tell
	// this answer from an estimate that genuinely landed there. See the saturation
	// paragraph on AddonPerformance, and F331.
	return prevBound
}
