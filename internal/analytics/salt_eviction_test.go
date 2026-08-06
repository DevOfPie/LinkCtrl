package analytics

import (
	"testing"
	"time"
)

// The in-memory salt cache holds exactly what the database holds.
//
// A salt for day D carries `purge_at = D + SaltRetentionDays`, and the purge
// statement deletes `purge_at < now()` — so the moment day D+2 begins, D's row
// is gone. The cache evicted `d.Before(day - SaltRetentionDays)`, which keeps
// exactly that day: the process held in memory the one salt the
// de-identification step had just deleted (F59).
//
// It is the *sequence* that matters, not the size of the window: two days is
// what this instance keeps, and the claim is that the copy in memory does not
// outlive the copy on disk.
func TestTheSaltCacheDropsWhatThePurgeDeletes(t *testing.T) {
	today := SaltDay(time.Now())
	c := &SaltCache{salts: map[time.Time][]byte{}}

	for i := range 5 {
		day := today.AddDate(0, 0, -i)
		c.salts[day] = []byte{byte(i)}
	}

	c.evictExpired(today)

	for i := range 5 {
		day := today.AddDate(0, 0, -i)
		_, held := c.salts[day]
		// purge_at is day+2, deleted once that instant has arrived — so days
		// two or more behind today are gone, and today and yesterday remain.
		wantHeld := i < SaltRetentionDays
		if held != wantHeld {
			verb := "is still cached"
			if wantHeld {
				verb = "was evicted"
			}
			t.Errorf("the salt for %d day(s) ago %s; purge_at is that day plus %d, "+
				"and the statement deletes every row whose purge_at has passed",
				i, verb, SaltRetentionDays)
		}
	}
}

// A cache hit trims too, which is what a replica that is not the leader relies
// on.
//
// Eviction ran only where a salt was created, and Purge — the other evictor —
// runs under leadership. So a follower that had already loaded today's salt
// never trimmed again, and held yesterday's for as long as the process lived.
func TestASaltCacheHitStillEvicts(t *testing.T) {
	today := SaltDay(time.Now())
	stale := today.AddDate(0, 0, -SaltRetentionDays)

	c := &SaltCache{salts: map[time.Time][]byte{
		today: {1},
		stale: {2},
	}}

	// The fast path: today's salt is present, so For returns without loading.
	if _, err := c.For(t.Context(), today); err != nil {
		t.Fatal(err)
	}
	if _, held := c.salts[stale]; held {
		t.Error("a cache hit left an expired salt in the map. On a replica that is " +
			"not the leader, a hit is the only eviction there is")
	}
}
