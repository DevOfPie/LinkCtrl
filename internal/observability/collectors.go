package observability

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Collectors for state that already exists somewhere else.
//
// The connection pools and the analytics ingester both keep authoritative
// counters of their own. Mirroring them into Prometheus gauges at write time
// would mean two sources of truth that can drift; reading them at scrape time
// cannot drift, and costs nothing between scrapes.

// poolCollector reports pgxpool statistics.
type poolCollector struct {
	pools map[string]*pgxpool.Pool

	acquired    *prometheus.Desc
	idle        *prometheus.Desc
	total       *prometheus.Desc
	max         *prometheus.Desc
	waitCount   *prometheus.Desc
	waitSeconds *prometheus.Desc
}

// NewPoolCollector reports on the named pools.
//
// Both pools are labelled separately because the entire point of splitting them
// is that they saturate independently: the alert worth having is "the redirect
// pool is exhausted", which an aggregate number hides.
func NewPoolCollector(pools map[string]*pgxpool.Pool) prometheus.Collector {
	const ns = "linkctrl_db_pool_"
	label := []string{"pool"}
	return &poolCollector{
		pools: pools,
		acquired: prometheus.NewDesc(ns+"acquired_connections",
			"Connections currently checked out.", label, nil),
		idle: prometheus.NewDesc(ns+"idle_connections",
			"Connections open and unused.", label, nil),
		total: prometheus.NewDesc(ns+"total_connections",
			"Connections open, in use or idle.", label, nil),
		max: prometheus.NewDesc(ns+"max_connections",
			"Configured ceiling. Saturation is total/max.", label, nil),
		waitCount: prometheus.NewDesc(ns+"acquire_waits_total",
			"Acquires that had to wait for a connection. Nonzero on the redirect pool means the hot path is queueing.",
			label, nil),
		waitSeconds: prometheus.NewDesc(ns+"acquire_wait_seconds_total",
			"Cumulative time spent waiting to acquire.", label, nil),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.acquired
	ch <- c.idle
	ch <- c.total
	ch <- c.max
	ch <- c.waitCount
	ch <- c.waitSeconds
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, pool := range c.pools {
		if pool == nil {
			continue
		}
		s := pool.Stat()
		gauge := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, name)
		}
		counter := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, name)
		}
		gauge(c.acquired, float64(s.AcquiredConns()))
		gauge(c.idle, float64(s.IdleConns()))
		gauge(c.total, float64(s.TotalConns()))
		gauge(c.max, float64(s.MaxConns()))
		counter(c.waitCount, float64(s.EmptyAcquireCount()))
		counter(c.waitSeconds, s.AcquireDuration().Seconds())
	}
}

// IngestStats is what the analytics ingester reports about itself.
//
// An interface rather than the concrete type, because observability must not
// import analytics — analytics is where the click pipeline lives and a cycle
// through logging would be waiting to happen. The composition root adapts.
type IngestStats interface {
	// QueueDepth is the leading indicator for the whole pipeline: it climbs
	// minutes before drops start.
	QueueDepth() int
	Counters() (enqueued, dropped, flushed, failed, batches int64)
}

type ingestCollector struct {
	stats IngestStats

	depth    *prometheus.Desc
	enqueued *prometheus.Desc
	dropped  *prometheus.Desc
	flushed  *prometheus.Desc
	failed   *prometheus.Desc
	batches  *prometheus.Desc
}

// NewIngestCollector reports the click pipeline's counters.
func NewIngestCollector(stats IngestStats) prometheus.Collector {
	const ns = "linkctrl_analytics_"
	return &ingestCollector{
		stats: stats,
		depth: prometheus.NewDesc(ns+"queue_depth",
			"Click events buffered and not yet written.", nil, nil),
		enqueued: prometheus.NewDesc(ns+"events_enqueued_total",
			"Click events accepted into the buffer.", nil, nil),
		dropped: prometheus.NewDesc(ns+"events_dropped_total",
			"Click events discarded because the buffer was full. Nonzero is alertable: analytics is losing data to protect redirect latency.",
			nil, nil),
		flushed: prometheus.NewDesc(ns+"events_flushed_total",
			"Click events written to the database.", nil, nil),
		failed: prometheus.NewDesc(ns+"events_failed_total",
			"Click events lost to a write failure.", nil, nil),
		batches: prometheus.NewDesc(ns+"batches_total",
			"Completed write batches.", nil, nil),
	}
}

func (c *ingestCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.depth
	ch <- c.enqueued
	ch <- c.dropped
	ch <- c.flushed
	ch <- c.failed
	ch <- c.batches
}

func (c *ingestCollector) Collect(ch chan<- prometheus.Metric) {
	if c.stats == nil {
		return
	}
	enqueued, dropped, flushed, failed, batches := c.stats.Counters()
	ch <- prometheus.MustNewConstMetric(c.depth, prometheus.GaugeValue, float64(c.stats.QueueDepth()))
	ch <- prometheus.MustNewConstMetric(c.enqueued, prometheus.CounterValue, float64(enqueued))
	ch <- prometheus.MustNewConstMetric(c.dropped, prometheus.CounterValue, float64(dropped))
	ch <- prometheus.MustNewConstMetric(c.flushed, prometheus.CounterValue, float64(flushed))
	ch <- prometheus.MustNewConstMetric(c.failed, prometheus.CounterValue, float64(failed))
	ch <- prometheus.MustNewConstMetric(c.batches, prometheus.CounterValue, float64(batches))
}

// LimiterStats is what a rate limiter reports about its own bookkeeping.
//
// An interface for the same reason as IngestStats: observability must not import
// the packages it observes.
type LimiterStats interface {
	// Len is tracked keys, which is the memory the limiter is using.
	Len() int
	// Overflows counts requests allowed because the key table was full — the
	// number that says the limiter has stopped limiting.
	Overflows() int64
}

type limiterCollector struct {
	limiters map[string]LimiterStats

	keys      *prometheus.Desc
	overflows *prometheus.Desc
}

// NewLimiterCollector reports the named limiters' bookkeeping.
//
// Neither series is about throttling — linkctrl_rate_limited_total covers that.
// These two answer a different question: is the limiter still able to do its
// job. A climbing overflow count means it is not, and that failure is otherwise
// completely silent, because the design choice on a full table is to allow the
// request.
//
// Disabled limits must be left out of the map by the caller rather than passed
// as a nil pointer: a nil pointer inside an interface is not a nil interface, so
// it would be collected as a working limiter reporting zeros — which reads as
// "enforcing, and nothing to report" instead of "off".
func NewLimiterCollector(limiters map[string]LimiterStats) prometheus.Collector {
	const ns = "linkctrl_rate_limit_"
	label := []string{"limit"}
	return &limiterCollector{
		limiters: limiters,
		keys: prometheus.NewDesc(ns+"tracked_keys",
			"Client keys currently tracked by each limiter.", label, nil),
		overflows: prometheus.NewDesc(ns+"overflow_total",
			"Requests allowed without being counted because the key table was full. Nonzero means the limit is no longer being enforced.",
			label, nil),
	}
}

func (c *limiterCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.keys
	ch <- c.overflows
}

func (c *limiterCollector) Collect(ch chan<- prometheus.Metric) {
	for name, l := range c.limiters {
		if l == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.keys, prometheus.GaugeValue, float64(l.Len()), name)
		ch <- prometheus.MustNewConstMetric(c.overflows, prometheus.CounterValue, float64(l.Overflows()), name)
	}
}
