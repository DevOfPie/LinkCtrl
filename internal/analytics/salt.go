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
	// Drop anything older than retention so the map cannot grow without bound
	// on a long-running process.
	cutoff := day.AddDate(0, 0, -SaltRetentionDays-1)
	for d := range c.salts {
		if d.Before(cutoff) {
			delete(c.salts, d)
		}
	}
	c.mu.Unlock()

	return salt, nil
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
	if n > 0 {
		c.mu.Lock()
		cutoff := SaltDay(time.Now()).AddDate(0, 0, -SaltRetentionDays)
		for d := range c.salts {
			if d.Before(cutoff) {
				delete(c.salts, d)
			}
		}
		c.mu.Unlock()
	}
	return n, nil
}
