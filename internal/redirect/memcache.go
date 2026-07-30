package redirect

import (
	"hash/maphash"
	"sync"
	"time"
)

// memCache is a sharded, TTL'd, size-bounded in-process cache.
//
// This is the first tier, in front of Redis. It exists because even a local
// Redis round trip is a syscall, a wire encode and a context switch — around
// 100-200us — and the budget for the whole request is 20ms at 2,000 rps. A
// process-local map hit is tens of nanoseconds.
//
// Written rather than pulled in because the requirements are narrow: bounded
// size, TTL, and concurrent reads that do not contend. A general-purpose LRU
// would add a dependency and a lock on every read to maintain recency order.
//
// Eviction is deliberately not LRU. When a shard is full it drops expired
// entries first and otherwise clears the shard. That is cruder than LRU, but
// the access pattern here is heavily skewed — a small set of hot links serves
// most traffic — so a cleared shard refills from Redis almost immediately, and
// avoiding a write lock on every read is worth far more than perfect recency.
type memCache struct {
	shards    []*memShard
	mask      uint64
	seed      maphash.Seed
	perShard  int
	ttlJitter time.Duration
}

type memShard struct {
	mu      sync.RWMutex
	entries map[string]memEntry
}

type memEntry struct {
	snapshot *Snapshot
	expires  time.Time
}

func newMemCache(size int) *memCache {
	// Power-of-two shard count so the index is a mask rather than a modulo.
	shardCount := 32
	if size < 1024 {
		shardCount = 8
	}
	c := &memCache{
		shards:   make([]*memShard, shardCount),
		mask:     uint64(shardCount - 1),
		seed:     maphash.MakeSeed(),
		perShard: max(size/shardCount, 64),
	}
	for i := range c.shards {
		c.shards[i] = &memShard{entries: make(map[string]memEntry, c.perShard/2)}
	}
	return c
}

func (c *memCache) shard(key string) *memShard {
	return c.shards[maphash.String(c.seed, key)&c.mask]
}

func (c *memCache) get(key string, now time.Time) (*Snapshot, bool) {
	s := c.shard(key)
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()

	if !ok {
		return nil, false
	}
	if now.After(e.expires) {
		// Expired entries are left for the next write to reap rather than
		// deleted here, which would need a write lock on a read path.
		return nil, false
	}
	return e.snapshot, true
}

func (c *memCache) set(key string, snap *Snapshot, ttl time.Duration, now time.Time) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= c.perShard {
		c.reap(s, now)
		if len(s.entries) >= c.perShard {
			clear(s.entries)
		}
	}
	s.entries[key] = memEntry{snapshot: snap, expires: now.Add(ttl)}
}

func (c *memCache) delete(key string) {
	s := c.shard(key)
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// reap drops expired entries. Caller must hold the write lock.
func (c *memCache) reap(s *memShard, now time.Time) {
	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
		}
	}
}

// clearAll drops everything. Used when a cross-process invalidation arrives
// and the specific key is not known.
func (c *memCache) clearAll() {
	for _, s := range c.shards {
		s.mu.Lock()
		clear(s.entries)
		s.mu.Unlock()
	}
}

func (c *memCache) len() int {
	n := 0
	for _, s := range c.shards {
		s.mu.RLock()
		n += len(s.entries)
		s.mu.RUnlock()
	}
	return n
}
