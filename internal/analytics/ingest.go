package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is a click as the redirect handler observes it.
//
// The raw IP is present here and nowhere else. It is consumed to derive the
// visitor hash and then dropped; it is never written, never logged and never
// leaves this package.
type Event struct {
	LinkID      uuid.UUID
	WorkspaceID uuid.UUID
	OccurredAt  time.Time
	IP          netip.Addr
	UserAgent   string
	Referrer    string
	Language    string
	LatencyUS   int32
}

// Stats are the ingester's counters.
type Stats struct {
	Enqueued atomic.Int64
	Dropped  atomic.Int64
	Flushed  atomic.Int64
	Failed   atomic.Int64
	Batches  atomic.Int64
}

// CountryResolver turns an address into an ISO 3166-1 alpha-2 country code, or
// "" when it cannot.
//
// An interface, not the geoip package, for two reasons: analytics should not
// depend on how geography is looked up, and a test needs to enrich events
// without a MaxMind database on disk.
type CountryResolver interface {
	Country(netip.Addr) string
}

type IngestConfig struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Logger        *slog.Logger

	// Geo is optional. Nil leaves the country column null, which is the default
	// state: the MaxMind database cannot be shipped in the image.
	Geo CountryResolver
}

// Ingester buffers click events and writes them in batches.
//
// The contract that shapes everything else: recording a click must never delay
// a redirect and must never fail one. So the queue is bounded and Record drops
// rather than blocks — applying backpressure to the hot path would trade a
// complete analytics record for a slow site, which is the wrong way round.
// Drops are counted, and a non-zero counter is an alert rather than a silent
// gap.
type Ingester struct {
	pool  *pgxpool.Pool
	salts *SaltCache
	cfg   IngestConfig
	log   *slog.Logger

	ch     chan Event
	Stats  Stats
	wg     sync.WaitGroup
	closed atomic.Bool
}

func NewIngester(pool *pgxpool.Pool, salts *SaltCache, cfg IngestConfig) *Ingester {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 16384
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 250 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Ingester{
		pool:  pool,
		salts: salts,
		cfg:   cfg,
		log:   cfg.Logger,
		ch:    make(chan Event, cfg.QueueSize),
	}
}

// Start launches the batching goroutine.
func (i *Ingester) Start() {
	i.wg.Add(1)
	go i.loop()
}

// Record enqueues an event, or drops it.
//
// Never blocks, never returns an error, never panics after Close. The default
// branch is the entire point: when the queue is full the event is discarded so
// the redirect returns on time.
func (i *Ingester) Record(ev Event) {
	if i.closed.Load() {
		return
	}
	select {
	case i.ch <- ev:
		i.Stats.Enqueued.Add(1)
	default:
		i.Stats.Dropped.Add(1)
	}
}

// Close stops accepting events and flushes what is buffered.
//
// Called during shutdown after the listener has closed, so no new events can
// arrive. Without this, every restart loses up to a full batch.
func (i *Ingester) Close(ctx context.Context) error {
	if !i.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(i.ch)

	done := make(chan struct{})
	go func() { i.wg.Wait(); close(done) }()

	select {
	case <-done:
		i.log.Info("analytics flushed on shutdown",
			slog.Int64("flushed", i.Stats.Flushed.Load()),
			slog.Int64("dropped", i.Stats.Dropped.Load()))
		return nil
	case <-ctx.Done():
		return fmt.Errorf("analytics: flush timed out with %d events buffered", len(i.ch))
	}
}

func (i *Ingester) loop() {
	defer i.wg.Done()

	ticker := time.NewTicker(i.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, i.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		i.write(batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev, ok := <-i.ch:
			if !ok {
				// Channel closed by Close. Drain whatever is left, then flush
				// a final time so buffered clicks are not lost.
				for ev := range i.ch {
					batch = append(batch, ev)
					if len(batch) >= i.cfg.BatchSize {
						flush()
					}
				}
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= i.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// write persists a batch.
func (i *Ingester) write(batch []Event) {
	// Detached from any request context and given its own deadline. Using a
	// request's context would mean a client disconnecting mid-flush discards
	// everyone else's clicks in the same batch.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, counts, err := i.prepare(ctx, batch)
	if err != nil {
		i.Stats.Failed.Add(int64(len(batch)))
		i.log.Error("failed to prepare click batch", slog.Any("error", err), slog.Int("events", len(batch)))
		return
	}

	tx, err := i.pool.Begin(ctx)
	if err != nil {
		i.Stats.Failed.Add(int64(len(batch)))
		i.log.Error("failed to begin click batch", slog.Any("error", err))
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// CopyFrom rather than a multi-row INSERT: it uses the binary COPY
	// protocol, which is several times faster for batches this size and does
	// not build a statement whose length grows with the batch.
	copied, err := tx.CopyFrom(ctx,
		pgx.Identifier{"click_events"},
		[]string{
			"id", "link_id", "workspace_id", "occurred_at", "visitor_hash",
			"is_first_visit", "country", "region", "city", "device", "browser",
			"os", "language", "referrer_host", "is_bot", "latency_us",
		},
		pgx.CopyFromSlice(len(rows), func(n int) ([]any, error) { return rows[n], nil }),
	)
	if err != nil {
		i.Stats.Failed.Add(int64(len(batch)))
		i.log.Error("failed to copy click events", slog.Any("error", err))
		return
	}

	// The denormalized counter is updated in the same transaction, so a click
	// row and its count never disagree. The counter remains approximate
	// overall, because a batch lost to SIGKILL takes both with it.
	for linkID, n := range counts {
		if _, err := tx.Exec(ctx, `
			UPDATE links
			   SET click_count = click_count + $2,
			       last_click_at = GREATEST(COALESCE(last_click_at, to_timestamp(0)), $3)
			 WHERE id = $1`, linkID, n.count, n.last); err != nil {
			i.log.Warn("failed to bump click count",
				slog.String("link_id", linkID.String()), slog.Any("error", err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		i.Stats.Failed.Add(int64(len(batch)))
		i.log.Error("failed to commit click batch", slog.Any("error", err))
		return
	}

	i.Stats.Flushed.Add(copied)
	i.Stats.Batches.Add(1)
}

type linkCount struct {
	count int64
	last  time.Time
}

// prepare enriches events into COPY rows.
//
// This is where the raw IP is consumed and discarded: it goes into the visitor
// hash and never reaches the row.
func (i *Ingester) prepare(ctx context.Context, batch []Event) ([][]any, map[uuid.UUID]linkCount, error) {
	rows := make([][]any, 0, len(batch))
	counts := make(map[uuid.UUID]linkCount, len(batch))

	// Cache salts per day within the batch: a batch almost always spans one
	// day, so this is one lookup rather than one per event.
	saltByDay := map[time.Time][]byte{}

	for _, ev := range batch {
		day := SaltDay(ev.OccurredAt)
		salt, ok := saltByDay[day]
		if !ok {
			s, err := i.salts.For(ctx, day)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve salt for %s: %w", day.Format(time.DateOnly), err)
			}
			salt = s
			saltByDay[day] = s
		}

		cls := Classify(ev.UserAgent)
		hash := VisitorHash(salt, ev.IP, ev.UserAgent, ev.WorkspaceID)

		// Country is derived here, in the same place and from the same value as
		// the visitor hash, and for the same reason: this is the last point at
		// which the address exists. It is not stored, so a country has to be
		// resolved now or never.
		country := i.country(ev.IP)

		rows = append(rows, []any{
			uuid.Must(uuid.NewV7()),
			ev.LinkID,
			ev.WorkspaceID,
			ev.OccurredAt,
			hash,
			false,   // is_first_visit, computed by the rollup job
			country, // nil unless a GeoIP database is configured
			nil,     // region — resolvable, deliberately not stored; see internal/geoip
			nil,     // city
			string(cls.Device),
			cls.Browser,
			cls.OS,
			PrimaryLanguage(ev.Language),
			ReferrerHost(ev.Referrer),
			cls.IsBot,
			ev.LatencyUS,
		})

		c := counts[ev.LinkID]
		c.count++
		if ev.OccurredAt.After(c.last) {
			c.last = ev.OccurredAt
		}
		counts[ev.LinkID] = c
	}

	return rows, counts, nil
}

// country resolves an address, returning nil rather than an empty string when
// there is no answer.
//
// The distinction matters at the column: NULL means "not resolved", and ” would
// be a country whose code is the empty string. Grouping analytics by the latter
// produces a bucket that looks like data.
func (i *Ingester) country(addr netip.Addr) *string {
	if i.cfg.Geo == nil {
		return nil
	}
	code := i.cfg.Geo.Country(addr)
	if code == "" {
		return nil
	}
	return &code
}

// QueueDepth reports buffered events.
//
// The leading indicator for the whole pipeline: depth climbing means the
// database is falling behind, minutes before drops start.
func (i *Ingester) QueueDepth() int { return len(i.ch) }

// Counters returns the lifetime totals, for the metrics collector.
//
// One method returning all five rather than five accessors, so a scrape reads
// them in one call and the set cannot be sampled half a flush apart.
func (i *Ingester) Counters() (enqueued, dropped, flushed, failed, batches int64) {
	return i.Stats.Enqueued.Load(), i.Stats.Dropped.Load(), i.Stats.Flushed.Load(),
		i.Stats.Failed.Load(), i.Stats.Batches.Load()
}

var ErrIngesterClosed = errors.New("analytics: ingester is closed")
