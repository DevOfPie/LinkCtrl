package analytics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// SaltRetentionDays is how long a salt is kept before deletion.
//
// The salt must outlive the events it keyed for as long as unique-visitor
// counting needs to recompute, but no longer — its deletion is what makes
// those hashes permanently unlinkable to an address. Two days covers a
// same-day rollup plus the finalize pass over the previous day.
const SaltRetentionDays = 2

// SaltCache resolves the salt for a UTC day, creating it on first use.
//
// Cached in memory because every batch needs it and it changes once a day. The
// cache is small and bounded by retention, so it is a plain map rather than
// anything with eviction.
type SaltCache struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries

	mu    sync.RWMutex
	salts map[time.Time][]byte

	// single-flight per day, so a cold start under load creates one salt
	// rather than having every concurrent batch race to insert.
	loading sync.Map
}

func NewSaltCache(pool *pgxpool.Pool) *SaltCache {
	return &SaltCache{
		pool:  pool,
		q:     dbgen.New(pool),
		salts: make(map[time.Time][]byte, 4),
	}
}

// For returns the salt for a day, creating it if absent.
func (c *SaltCache) For(ctx context.Context, day time.Time) ([]byte, error) {
	day = SaltDay(day)

	c.mu.RLock()
	if s, ok := c.salts[day]; ok {
		c.mu.RUnlock()
		// Evicted on a **hit** as well as a miss (F59). Eviction used to run
		// only where a salt was created, so a replica that had already loaded
		// today's salt never trimmed again — and Purge, the other evictor, runs
		// under leadership, so a follower held yesterday's salts for as long as
		// the process lived. Two map reads on a hot path, against a map that
		// holds three days.
		c.evictExpired(day)
		return s, nil
	}
	c.mu.RUnlock()

	// Serialize creation per day.
	lock, _ := c.loading.LoadOrStore(day, &sync.Mutex{})
	// Checked rather than asserted: nothing else writes to this map, so a
	// failure here would mean memory corruption, and taking no lock at all
	// would be worse than saying so.
	mu, ok := lock.(*sync.Mutex)
	if !ok {
		return nil, fmt.Errorf("analytics: salt lock for %s has unexpected type %T",
			day.Format(time.DateOnly), lock)
	}
	mu.Lock()
	defer mu.Unlock()

	// Another goroutine may have won while we waited.
	c.mu.RLock()
	if s, ok := c.salts[day]; ok {
		c.mu.RUnlock()
		return s, nil
	}
	c.mu.RUnlock()

	salt, err := c.load(ctx, day)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.salts[day] = salt
	c.mu.Unlock()
	c.evictExpired(day)

	return salt, nil
}

// evictExpired drops every cached salt whose row the purge has deleted, or
// would delete now.
//
// **The predicate is the SQL's, not an approximation of it.** A salt for day D
// carries `purge_at = D + SaltRetentionDays` and the statement deletes
// `purge_at < now()`, so the moment day D+2 begins, D's row is gone. The old
// cutoff was `d.Before(day - SaltRetentionDays)`, which keeps exactly that day —
// so the process held in memory the one salt the de-identification step had just
// deleted from the database (F59).
//
// The claim it makes true is `README.md`'s and `salt.go`'s own: those are claims
// about what a *database* holds, and a heap copy in a live process is not one —
// but "the salt is gone" is a sentence worth being able to say without a
// footnote about which copy.
func (c *SaltCache) evictExpired(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for d := range c.salts {
		if d.AddDate(0, 0, SaltRetentionDays).Before(now) || d.AddDate(0, 0, SaltRetentionDays).Equal(now) {
			delete(c.salts, d)
		}
	}
}

// Cached returns a day's salt only if it is already in memory.
//
// The redirect path's caller, and the reason it exists: For creates or fetches
// the salt, which is a database query, and M34 claims that evaluating a routing
// rule adds none. A miss here is answered by the caller as "this visitor is
// new" rather than by going to Postgres — see ReturningSet.Seen for why that is
// true rather than merely cheap.
func (c *SaltCache) Cached(day time.Time) ([]byte, bool) {
	day = SaltDay(day)
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.salts[day]
	return s, ok
}

func (c *SaltCache) load(ctx context.Context, day time.Time) ([]byte, error) {
	existing, err := c.q.GetSalt(ctx, day)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read salt: %w", err)
	}

	fresh, err := NewSalt()
	if err != nil {
		return nil, err
	}

	// ON CONFLICT DO NOTHING ... RETURNING yields no row when another replica
	// inserted first, so the winner is read back rather than assumed. Two
	// replicas using different salts for the same day would split every
	// visitor in two.
	stored, err := c.q.CreateSalt(ctx, dbgen.CreateSaltParams{
		ValidOn: day,
		Salt:    fresh,
		PurgeAt: day.AddDate(0, 0, SaltRetentionDays),
	})
	if err == nil {
		return stored, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return c.q.GetSalt(ctx, day)
	}
	return nil, fmt.Errorf("create salt: %w", err)
}

// Purge deletes salts past their retention.
//
// This is the de-identification step, not housekeeping: once a salt is gone,
// the hashes it produced cannot be linked back to an address even by someone
// holding the original addresses.
func (c *SaltCache) Purge(ctx context.Context) (int64, error) {
	n, err := c.q.PurgeExpiredSalts(ctx)
	if err != nil {
		return 0, fmt.Errorf("purge salts: %w", err)
	}
	// Unconditionally, not only when this call deleted something: on a
	// multi-replica instance the leader's delete is the one that returns a
	// count, and every other replica's memory needs trimming just the same.
	c.evictExpired(SaltDay(time.Now()))
	return n, nil
}
