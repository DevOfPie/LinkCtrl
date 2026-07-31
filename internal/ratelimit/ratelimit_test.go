package ratelimit

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

// clock is a hand-wound clock. Rate limiting is defined in terms of elapsed
// time, and a test that sleeps to advance it is a test that is slow when it
// passes and flaky when the machine is busy.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

func TestDisabledWhenRateIsZeroOrNegative(t *testing.T) {
	for _, rate := range []int{0, -1} {
		if l := New(rate, Options{}); l != nil {
			t.Errorf("New(%d) = %p, want nil so the limit is off", rate, l)
		}
	}
}

func TestNilLimiterAllowsEverything(t *testing.T) {
	var l *Limiter
	ok, retry := l.Allow(addr(t, "203.0.113.9"))
	if !ok || retry != 0 {
		t.Fatalf("nil limiter: Allow = (%v, %v), want (true, 0)", ok, retry)
	}
	// The other methods must also tolerate nil, or "0 disables" would need a
	// branch at every call site.
	l.Charge(addr(t, "203.0.113.9"))
	if ok, _ := l.Check(addr(t, "203.0.113.9")); !ok {
		t.Error("nil limiter: Check = false, want true")
	}
	if l.Len() != 0 || l.Overflows() != 0 {
		t.Error("nil limiter: counters should be zero")
	}
}

func TestBurstThenThrottle(t *testing.T) {
	c := newClock()
	l := New(60, Options{Now: c.now})
	ip := addr(t, "203.0.113.9")

	// Burst defaults to the per-minute rate: a full minute's allowance at once.
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow(ip); !ok {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}

	ok, retry := l.Allow(ip)
	if ok {
		t.Fatal("request 61 allowed; the burst should be spent")
	}
	// 60/min is one per second, so the next token is a second away.
	if retry < 900*time.Millisecond || retry > 1100*time.Millisecond {
		t.Errorf("Retry-After = %v, want about 1s", retry)
	}
}

func TestRefillIsProportionalToElapsedTime(t *testing.T) {
	c := newClock()
	l := New(60, Options{Now: c.now})
	ip := addr(t, "203.0.113.9")

	for i := 0; i < 60; i++ {
		l.Allow(ip)
	}
	if ok, _ := l.Allow(ip); ok {
		t.Fatal("bucket should be empty")
	}

	c.advance(5 * time.Second) // 60/min = 1/s, so five tokens
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(ip); !ok {
			t.Fatalf("refilled request %d refused", i+1)
		}
	}
	if ok, _ := l.Allow(ip); ok {
		t.Fatal("six requests allowed after five seconds of refill")
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	c := newClock()
	l := New(10, Options{Now: c.now})
	ip := addr(t, "203.0.113.9")

	for i := 0; i < 10; i++ {
		l.Allow(ip)
	}
	// An hour of idling must not accumulate an hour of tokens, or a client that
	// pauses can arrive later with an unbounded allowance.
	c.advance(time.Hour)
	allowed := 0
	for i := 0; i < 50; i++ {
		if ok, _ := l.Allow(ip); ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("allowed %d after an idle hour, want the burst of 10", allowed)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := New(2, Options{Now: c.now})
	a, b := addr(t, "203.0.113.9"), addr(t, "198.51.100.4")

	for i := 0; i < 2; i++ {
		l.Allow(a)
	}
	if ok, _ := l.Allow(a); ok {
		t.Fatal("first address should be throttled")
	}
	if ok, _ := l.Allow(b); !ok {
		t.Fatal("second address throttled by the first address's traffic")
	}
}

// The point of keying IPv6 by /64: a host with a /64 must not be able to walk
// through addresses to reset its allowance.
func TestIPv6IsKeyedBySlash64(t *testing.T) {
	c := newClock()
	l := New(2, Options{Now: c.now})

	l.Allow(addr(t, "2001:db8:abcd:1234::1"))
	l.Allow(addr(t, "2001:db8:abcd:1234::2"))
	if ok, _ := l.Allow(addr(t, "2001:db8:abcd:1234:ffff:ffff:ffff:ffff")); ok {
		t.Error("a different address in the same /64 was allowed; the /64 is spent")
	}
	// A different /64 is a different customer.
	if ok, _ := l.Allow(addr(t, "2001:db8:abcd:9999::1")); !ok {
		t.Error("a different /64 was throttled")
	}
	if n := l.Len(); n != 2 {
		t.Errorf("tracked %d keys, want 2 (one per /64)", n)
	}
}

func TestIPv4MappedAddressSharesTheIPv4Key(t *testing.T) {
	if got, want := Key(netip.MustParseAddr("::ffff:203.0.113.9")), "203.0.113.9"; got != want {
		t.Errorf("Key(4-in-6) = %q, want %q", got, want)
	}
}

func TestInvalidAddressesShareOneBucket(t *testing.T) {
	c := newClock()
	l := New(1, Options{Now: c.now})

	if ok, _ := l.Allow(netip.Addr{}); !ok {
		t.Fatal("first unparseable address refused")
	}
	if ok, _ := l.Allow(netip.Addr{}); ok {
		t.Error("unparseable addresses are exempt from the limit")
	}
}

func TestCheckDoesNotConsume(t *testing.T) {
	c := newClock()
	l := New(3, Options{Now: c.now})
	ip := addr(t, "203.0.113.9")

	for i := 0; i < 10; i++ {
		if ok, _ := l.Check(ip); !ok {
			t.Fatalf("Check %d reported throttled without any charge", i+1)
		}
	}
	// Three charges spend the bucket; Check then reports it.
	for i := 0; i < 3; i++ {
		l.Charge(ip)
	}
	if ok, retry := l.Check(ip); ok || retry <= 0 {
		t.Errorf("Check after three charges = (%v, %v), want throttled with a wait", ok, retry)
	}
}

func TestSweepReclaimsRefilledKeys(t *testing.T) {
	c := newClock()
	l := New(60, Options{Now: c.now})

	// Enough distinct keys to be worth reclaiming, each spending one token.
	for i := 0; i < 200; i++ {
		l.Allow(netip.AddrFrom4([4]byte{203, 0, 113, byte(i % 256)}))
	}
	before := l.Len()
	if before == 0 {
		t.Fatal("no keys tracked")
	}

	// Once every bucket has refilled to full it holds no information, so a sweep
	// should empty the table.
	c.advance(2 * time.Minute)
	for i := range l.shards {
		l.shards[i].mu.Lock()
		l.sweepLocked(&l.shards[i], c.now())
		l.shards[i].mu.Unlock()
	}
	if after := l.Len(); after != 0 {
		t.Errorf("swept table holds %d keys (was %d), want 0", after, before)
	}
}

// A throttled key must survive a sweep, or an attacker could clear their own
// penalty by generating enough unrelated traffic to trigger one.
func TestSweepKeepsThrottledKeys(t *testing.T) {
	c := newClock()
	l := New(60, Options{Now: c.now})
	ip := addr(t, "203.0.113.9")

	for i := 0; i < 60; i++ {
		l.Allow(ip)
	}
	for i := range l.shards {
		l.shards[i].mu.Lock()
		l.sweepLocked(&l.shards[i], c.now())
		l.shards[i].mu.Unlock()
	}
	if ok, _ := l.Allow(ip); ok {
		t.Error("a spent bucket was swept away, resetting the penalty")
	}
}

func TestTableIsBoundedAndFailsOpen(t *testing.T) {
	c := newClock()
	// One key per shard, so the cap is reachable in a test.
	l := New(1, Options{Now: c.now, MaxKeys: shardCount})

	for i := 0; i < 4096; i++ {
		l.Allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	if n := l.Len(); n > shardCount {
		t.Errorf("tracked %d keys, want at most the %d-key cap", n, shardCount)
	}
	if l.Overflows() == 0 {
		t.Error("no overflows counted; a full table must be visible, not silent")
	}
	// Failing open is the documented choice: the requests that could not be
	// tracked were allowed, not refused.
	ok, _ := l.Allow(netip.AddrFrom4([4]byte{10, 255, 255, 254}))
	if !ok {
		t.Error("a request was refused because the table was full; it must fail open")
	}
}

func TestRetryAfterSecondsRoundsUpWithAFloorOfOne(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{-time.Second, 1},
		{time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{30 * time.Second, 30},
	}
	for _, tc := range cases {
		if got := RetryAfterSeconds(tc.in); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The limiter is consulted from every request-handling goroutine, so the race
// detector needs something to look at.
func TestConcurrentUseIsRaceFree(t *testing.T) {
	l := New(600, Options{})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				a := netip.AddrFrom4([4]byte{203, 0, byte(g), byte(i % 251)})
				switch i % 3 {
				case 0:
					l.Allow(a)
				case 1:
					l.Check(a)
				default:
					l.Charge(a)
				}
			}
		}(g)
	}
	wg.Wait()
}
