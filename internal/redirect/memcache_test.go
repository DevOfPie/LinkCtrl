package redirect

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// The in-process cache is the tier the redirect SLO and the throttled path's
// ResolveCached both stand on, so its edge behaviour — expiry, eviction, the
// small-cache sizing — is worth pinning down directly rather than through the
// resolver.

func TestMemCacheSetGetDelete(t *testing.T) {
	c := newMemCache(10_000)
	now := testNow
	snap := &Snapshot{Status: "active", URL: "https://example.com/a"}

	c.set("k1", snap, time.Minute, now)
	got, ok := c.get("k1", now)
	if !ok || got != snap {
		t.Fatalf("get after set = (%v, %v), want the stored snapshot", got, ok)
	}
	if _, ok := c.get("absent", now); ok {
		t.Error("get of an absent key reported a hit")
	}

	c.delete("k1")
	if _, ok := c.get("k1", now); ok {
		t.Error("get after delete reported a hit")
	}
}

func TestMemCacheExpiry(t *testing.T) {
	c := newMemCache(10_000)
	now := testNow
	c.set("k", &Snapshot{Status: "active"}, time.Minute, now)

	if _, ok := c.get("k", now.Add(59*time.Second)); !ok {
		t.Error("entry expired early")
	}
	// Expired entries answer as misses but are left in place — reaping on the
	// read path would need a write lock where the design wants an RLock.
	if _, ok := c.get("k", now.Add(61*time.Second)); ok {
		t.Error("expired entry still served")
	}
	if c.len() != 1 {
		t.Errorf("len = %d; expired entries are reaped by writes, not reads", c.len())
	}
}

// A full shard reaps expired entries first; only when nothing is reclaimable
// does it clear. Both behaviours are deliberate — crude beats a lock on reads —
// and both should survive refactors.
func TestMemCacheEvictionReapsBeforeClearing(t *testing.T) {
	// Small size → the small-cache path: 8 shards, floor of 64 per shard.
	c := newMemCache(1)
	if len(c.shards) != shardsSmall {
		t.Fatalf("small cache has %d shards, want %d", len(c.shards), shardsSmall)
	}
	now := testNow

	// Fill one shard to its cap with entries that are already expired by the
	// time the next write arrives, then keep writing: the reap must reclaim
	// them rather than the shard clearing live data.
	var shardKeys []string
	target := c.shard("probe")
	for i := 0; len(shardKeys) < c.perShard; i++ {
		k := fmt.Sprintf("k%d", i)
		if c.shard(k) == target {
			shardKeys = append(shardKeys, k)
			c.set(k, &Snapshot{Status: "active"}, time.Second, now)
		}
	}

	// The shard is at capacity; a later write with everything expired reaps.
	later := now.Add(time.Minute)
	c.set("probe", &Snapshot{Status: "active", URL: "survivor"}, time.Hour, later)

	if got, ok := c.get("probe", later); !ok || got.URL != "survivor" {
		t.Fatalf("the write that triggered the reap was lost")
	}
	for _, k := range shardKeys {
		if _, ok := c.get(k, later); ok {
			t.Fatalf("expired entry %s survived the reap", k)
		}
	}

	// Now fill with UNexpired entries and overflow: the shard clears, and the
	// newest write survives. Crude, but a cleared shard refills from the next
	// tier; a shard that refuses writes goes quietly read-only.
	c2 := newMemCache(1)
	t2 := c2.shard("probe2")
	n := 0
	for i := 0; n < c2.perShard; i++ {
		k := fmt.Sprintf("live%d", i)
		if c2.shard(k) == t2 {
			c2.set(k, &Snapshot{Status: "active"}, time.Hour, now)
			n++
		}
	}
	c2.set("probe2", &Snapshot{Status: "active", URL: "newest"}, time.Hour, now)
	if got, ok := c2.get("probe2", now); !ok || got.URL != "newest" {
		t.Error("the write that triggered the clear was lost")
	}
}

func TestMemCacheConcurrentAccess(t *testing.T) {
	c := newMemCache(1024)
	now := testNow
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k%d", (g*31+i)%512)
				switch i % 3 {
				case 0:
					c.set(k, &Snapshot{Status: "active"}, time.Minute, now)
				case 1:
					c.get(k, now)
				default:
					c.delete(k)
				}
			}
		}(g)
	}
	wg.Wait()
}
