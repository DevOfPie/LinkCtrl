package analytics

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
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
	// Source is a resolved attribution token (M41) — see domain.ClickSource. It
	// replaces the referrer host on the stored row when set, because the clicks
	// it describes carry no Referer at all: a QR scan comes from a camera.
	//
	// No new column and no new dimension: the value lands in `referrer_host` and
	// is rolled up as the `referrer` breakdown, beside the `direct` sentinel that
	// column already holds for a click with no referrer.
	Source    string
	Language  string
	LatencyUS int32

	// TrackReturning asks the ingester to remember this visitor in the
	// within-day returning-visitor set (M34).
	//
	// Set by the redirect handler, from the link's own rules, and false for
	// every link that has none — which is what keeps the set from being
	// maintained for the whole instance. The alternative is the ingester asking
	// which links have a returning-visitor rule, which is a query per batch
	// against data the handler already had in its hand.
	TrackReturning bool

	// DestinationID is the destinations row this click was sent to (M36).
	//
	// The zero uuid means the link's own destination, and is written to the
	// column as NULL — see destinationOrNil. Every click on every link without
	// rules carries it, which is why the column is nullable rather than
	// backfilled: a default would be a per-row copy of what the link already
	// says.
	DestinationID uuid.UUID
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
	//
	// A CountryResolver, not the geoip package's whole Resolver, and that
	// narrowing is now load-bearing rather than tidy: M34 gave that type Region
	// and City, and the interface here is what says the click pipeline may not
	// call them. Region and city are resolvable on the redirect path and are
	// never stored, and this is where "never stored" is enforced by the type
	// system instead of by remembering.
	Geo CountryResolver

	// Returning maintains the within-day returning-visitor set (M34). Nil on an
	// instance with no Redis, and then nothing is written and every visitor
	// reads as new.
	Returning *ReturningSet

	// Observer is the add-on host, for add-ons holding `redirect.observe` (M66).
	// Nil is every instance that installed none, and it costs a nil check per
	// batch.
	//
	// **The pipeline is where the observe class is fed from, and that is a
	// placement rather than a convenience.** m66.md's rule for the class is that
	// it runs off the request path, after the response, and can delay nothing.
	// This goroutine is already all three. It is also the only place the derived
	// fields exist at all: the country and the visitor hash are computed here from
	// an address that is discarded in the same statement, so an observer fed from
	// the handler would have to be handed the address — which is the one thing the
	// ABI's privacy assertion says never crosses.
	Observer RedirectObserver
}

// RedirectObserver receives one recorded redirect, out of band.
//
// An interface for the reason [CountryResolver] is one: this package should not
// know what a WASM host is, and a test has to be able to watch what would be
// handed over without starting one. Its one implementation is *addon.Host.
//
// The contract is the whole of why this is safe to call from the flush loop:
// [addon.Host.Observe] never blocks, never fails and never returns anything. An
// add-on that cannot keep up loses observations and says so on a counter; it does
// not make a click batch late.
type RedirectObserver interface {
	Observe(ev addon.RedirectEvent)
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

	ch chan Event
	// stop is closed instead of ch, so that a Record already past its closed
	// check cannot send on a closed channel. See Close.
	stop   chan struct{}
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
		stop:  make(chan struct{}),
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
//
// Signals through stop rather than closing ch. Record's closed check and its
// send cannot be one atomic step, so a redirect that passed the check just
// before Close ran would send on a closed channel and panic — killing the
// process during the one window where it is trying to save data. That is not
// hypothetical during a shutdown that times out with requests still in flight,
// which is exactly when Close is called. An event that races the drain is lost
// instead, which is what a full queue already does to it.
func (i *Ingester) Close(ctx context.Context) error {
	if !i.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(i.stop)

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
		case ev := <-i.ch:
			batch = append(batch, ev)
			if len(batch) >= i.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-i.stop:
			// Drain what is buffered, then flush a final time so a restart
			// does not lose up to a full batch. A non-blocking drain rather
			// than `range i.ch`, because ch is never closed — see Close.
			for {
				select {
				case ev := <-i.ch:
					batch = append(batch, ev)
					if len(batch) >= i.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
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

	rows, counts, marks, observed, err := i.prepare(ctx, batch)
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
		clickEventColumns,
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
	//
	// One statement, ordered by link id, for two reasons. Ordering: two
	// instances flushing batches that touch the same popular links would take
	// row locks in whatever order Go's map iteration produced, which is a
	// deadlock the database resolves by killing one of them. Single statement:
	// the previous loop logged a warning and continued on error, which cannot
	// work inside a transaction — the first failed UPDATE aborts it, every
	// later Exec fails with 25P02, and the Commit rolls back the click rows
	// that were already copied. A whole batch disappeared while the log said
	// "warning".
	if len(counts) > 0 {
		ids := make([]uuid.UUID, 0, len(counts))
		for linkID := range counts {
			ids = append(ids, linkID)
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })

		bumps := make([]int64, len(ids))
		lasts := make([]time.Time, len(ids))
		for n, linkID := range ids {
			bumps[n] = counts[linkID].count
			lasts[n] = counts[linkID].last
		}

		if _, err := tx.Exec(ctx, `
			UPDATE links l
			   SET click_count = l.click_count + b.bump,
			       last_click_at = GREATEST(COALESCE(l.last_click_at, to_timestamp(0)), b.last)
			  FROM unnest($1::uuid[], $2::bigint[], $3::timestamptz[]) AS b(id, bump, last)
			 WHERE l.id = b.id`, ids, bumps, lasts); err != nil {
			i.Stats.Failed.Add(int64(len(batch)))
			i.log.Error("failed to bump click counts; the batch is being rolled back",
				slog.Any("error", err), slog.Int("links", len(ids)))
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		i.Stats.Failed.Add(int64(len(batch)))
		i.log.Error("failed to commit click batch", slog.Any("error", err))
		return
	}

	i.Stats.Flushed.Add(copied)
	i.Stats.Batches.Add(1)

	// After the commit, deliberately. The set is a cache of what the click rows
	// already say, so marking a visitor whose batch then rolled back would leave
	// Redis asserting a visit Postgres has no record of — and nothing would ever
	// correct it, because the set is only ever added to.
	i.cfg.Returning.mark(ctx, marks, time.Now())

	// After the commit, for the reason the marks are: an add-on told about a
	// redirect whose batch then rolled back would have written a row about a click
	// this product has no record of, in a schema nothing here can correct. So an
	// observation is a fact about a *stored* click, which is the same thing the
	// analytics an operator reads is a fact about.
	//
	// Handing them over one at a time rather than as a batch, because the queue on
	// the other side is bounded and the point of that bound is that a slow add-on
	// loses the oldest observations rather than the newest — a batch offered whole
	// would be all-or-nothing against the same limit.
	for _, ev := range observed {
		i.cfg.Observer.Observe(ev)
	}
}

type linkCount struct {
	count int64
	last  time.Time
}

// clickEventColumns is the COPY column list, and prepare builds each row in
// exactly this order.
//
// **Positional, binary, and silent when it is wrong.** pgx.CopyFrom sends values
// by position: a column list and a row slice that disagree about order do not
// produce an error, they produce rows whose values are in the wrong columns —
// a browser stored as a country, a latency stored as a language. Nothing fails,
// nothing logs, and the analytics are quietly untrue.
//
// Two things keep the two lists in step, and neither is remembering. The list is
// named here rather than written inline at the CopyFrom call, so it sits beside
// the function that builds the rows; and TestCopyRowsMatchTheColumnList asserts
// the widths agree while the integration suite reads a written row back column
// by column, which is the only check that catches a *reordering* rather than an
// omission.
//
// destination_id (M36) was appended rather than inserted next to link_id, which
// is where it would read best. Appending leaves the sixteen positions that
// existed before it untouched, so the edit that added it could not silently
// shift any of them.
// referrerOrSource decides what goes in `referrer_host` (M41).
//
// The source wins when there is one, and there is one only for a click carrying
// a value from domain's closed vocabulary. The two cannot both be meaningful:
// a scan sends no Referer, so the branch is a choice between a token and an
// empty string rather than between two facts about the same visit.
//
// It returns the source verbatim rather than passing it through ReferrerHost.
// That function exists to strip a URL down to its host and to throw away the
// personal data the rest of a referrer carries; a token that never was a URL
// has nothing to strip, and running it through anyway would make the stored
// value depend on a parser it has no reason to meet.
func referrerOrSource(ev Event) string {
	if ev.Source != "" {
		return ev.Source
	}
	return ReferrerHost(ev.Referrer)
}

var clickEventColumns = []string{
	"id", "link_id", "workspace_id", "occurred_at", "visitor_hash",
	"is_first_visit", "country", "region", "city", "device", "browser",
	"os", "language", "referrer_host", "is_bot", "latency_us",
	"destination_id",
}

// prepare enriches events into COPY rows.
//
// This is where the raw IP is consumed and discarded: it goes into the visitor
// hash and never reaches the row.
func (i *Ingester) prepare(ctx context.Context, batch []Event) (
	[][]any, map[uuid.UUID]linkCount, []returningMark, []addon.RedirectEvent, error,
) {
	rows := make([][]any, 0, len(batch))
	counts := make(map[uuid.UUID]linkCount, len(batch))
	var marks []returningMark
	// Built only when something is going to read them, so an instance with no
	// observing add-on allocates nothing here — which is every instance until an
	// operator installs one, and this loop runs per click.
	var observed []addon.RedirectEvent

	// Cache salts per day within the batch: a batch almost always spans one
	// day, so this is one lookup rather than one per event.
	saltByDay := map[time.Time][]byte{}

	for _, ev := range batch {
		day := SaltDay(ev.OccurredAt)
		salt, ok := saltByDay[day]
		if !ok {
			s, err := i.salts.For(ctx, day)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("resolve salt for %s: %w", day.Format(time.DateOnly), err)
			}
			salt = s
			saltByDay[day] = s
		}

		cls := Classify(ev.UserAgent)
		hash := VisitorHash(salt, ev.IP, ev.UserAgent, ev.WorkspaceID)

		// The within-day returning-visitor set (M34), for the links whose rules
		// asked. Derived here for the same reason the country is: this is the
		// last point at which the address exists.
		if ev.TrackReturning && i.cfg.Returning.Enabled() {
			marks = append(marks, returningMark{
				linkID: ev.LinkID, day: day,
				member: i.cfg.Returning.Member(salt, ev.IP, ev.UserAgent, ev.WorkspaceID),
			})
		}

		// Country is derived here, in the same place and from the same value as
		// the visitor hash, and for the same reason: this is the last point at
		// which the address exists. It is not stored, so a country has to be
		// resolved now or never.
		country := i.country(ev.IP)

		if i.cfg.Observer != nil {
			// Built from the same derived values the row below is built from, in the
			// same iteration, rather than read back out of the row slice: the row is
			// positional and untyped, and a record assembled by indexing into it
			// would be the silent-mis-ordering failure clickEventColumns warns about
			// with a second reader.
			//
			// **Every field is one of the columns beside it**, which is what
			// abi_test.go asserts of the ABI record against the migration. region and
			// city are columns and are deliberately absent: the privacy stance is
			// country-level and an add-on with storage is where they would stop being
			// transient. So is latency, which is not a fact about the visitor but is
			// also not one an add-on has any use for, and so is the destination id,
			// which names a row in a table no add-on can read.
			observed = append(observed, addon.RedirectEvent{
				LinkID:       ev.LinkID.String(),
				WorkspaceID:  ev.WorkspaceID.String(),
				OccurredAt:   ev.OccurredAt.UTC().Format(time.RFC3339Nano),
				VisitorHash:  hex.EncodeToString(hash),
				IsFirstVisit: false,
				Country:      countryOrEmpty(country),
				Device:       string(cls.Device),
				Browser:      cls.Browser,
				OS:           cls.OS,
				Language:     PrimaryLanguage(ev.Language),
				ReferrerHost: referrerOrSource(ev),
				IsBot:        cls.IsBot,
			})
		}

		rows = append(rows, []any{
			uuid.Must(uuid.NewV7()),
			ev.LinkID,
			ev.WorkspaceID,
			ev.OccurredAt,
			hash,
			// is_first_visit is dormant: nothing computes it and nothing reads
			// it, and Phase 2 left it that way (D12). It is storage for a
			// new-versus-returning split, and deriving it here would cost a
			// per-click lookup for a number no surface displays. Always false
			// until something needs it — this named Phase 2 until 0.2.0, which
			// described the column as scheduled rather than as waiting.
			false,
			country, // nil unless a GeoIP database is configured
			nil,     // region — resolvable, deliberately not stored; see internal/geoip
			nil,     // city
			string(cls.Device),
			cls.Browser,
			cls.OS,
			PrimaryLanguage(ev.Language),
			referrerOrSource(ev),
			cls.IsBot,
			ev.LatencyUS,
			// Last, matching clickEventColumns. See the comment there for why
			// this was appended rather than placed beside link_id.
			destinationOrNil(ev.DestinationID),
		})

		c := counts[ev.LinkID]
		c.count++
		if ev.OccurredAt.After(c.last) {
			c.last = ev.OccurredAt
		}
		counts[ev.LinkID] = c
	}

	return rows, counts, marks, observed, nil
}

// countryOrEmpty flattens the nullable country the row carries into the string
// the ABI record does.
//
// The two spellings are not the same fact told twice. NULL in the column means
// *this instance has no GeoIP database, or the address was in nobody's range*,
// and the column keeps that distinct from a country because a breakdown that
// bucketed unknown as a place would be a chart that lies. Across the ABI it is
// an empty string, because the record is JSON and a publisher reading a field
// that is sometimes absent and sometimes a code has two shapes to handle for one
// meaning.
func countryOrEmpty(c *string) string {
	if c == nil {
		return ""
	}
	return *c
}

// destinationOrNil turns the zero uuid into a NULL.
//
// The distinction matters at the column for the same reason it does for the
// country: NULL means "the link's own destination", and a zero uuid stored
// literally would be a destination id that resolves to nothing and would appear
// in the breakdown as a bucket that looks like data.
func destinationOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
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
