//go:build integration

package integration

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The Redis tier had no coverage anywhere. Every other fixture constructs the
// resolver with a nil client — deliberately, to exercise the degraded path — so
// the Get, Set and Del branches never executed in any test, in any package,
// while both CI workflows provisioned a Redis nothing connected to. A key-format
// divergence between the write path and the invalidate path, or a change to the
// stored payload, would have shipped green.
//
// These use the real client against the Redis CI already runs, rather than a
// fake: the failure being guarded against lives in the wire format.

func newRedisClient(t *testing.T) *goredis.Client {
	t.Helper()

	url := os.Getenv("LINKCTRL_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatalf("LINKCTRL_REDIS_URL is not a valid Redis URL: %v", err)
	}
	c := goredis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		t.Skipf("Redis is not reachable at %s (%v). Start it with `docker compose up -d redis`.", url, err)
	}
	// A clean keyspace per test, so a developer's running instance and a
	// parallel test cannot see each other's entries.
	if err := c.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// aliasKeys returns every cached alias entry, so a test can assert on the tier
// without reaching for the resolver's unexported key format.
func aliasKeys(t *testing.T, c *goredis.Client) []string {
	t.Helper()
	keys, err := c.Keys(context.Background(), "lc:a:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// createLink makes a link through the API and returns its alias.
func createLink(t *testing.T, f *apiFixture, alias, url string) string {
	t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": url, "alias": alias})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating %s returned %d", alias, resp.StatusCode)
	}
	f.decode(resp, nil)
	return alias
}

// The whole round trip: a cold resolve writes through to Redis, and a resolver
// with a cold memory tier reads it back instead of going to Postgres.
func TestRedisTierIsPopulatedAndRead(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	rdb := newRedisClient(t)
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "cached01", "https://example.com/cached")

	opts := redirect.Options{TTL: time.Hour, NegativeTTL: time.Minute}
	resolver := redirect.NewResolver(f.pool, rdb, opts)

	// First resolve: nothing cached, so it comes from Postgres and writes through.
	res, err := resolver.Resolve(ctx, dom.ID, alias)
	if err != nil {
		t.Fatalf("cold resolve: %v", err)
	}
	if res.Source != redirect.SourceDatabase {
		t.Errorf("cold resolve came from %s, want database", res.Source)
	}
	if got := aliasKeys(t, rdb); len(got) != 1 {
		t.Fatalf("Redis holds %d alias keys after a cold resolve, want 1: the "+
			"write-through path did not run", len(got))
	}

	// A second resolver shares Redis but has an empty memory tier, which is what
	// a restarted process looks like. It must be served from Redis.
	fresh := redirect.NewResolver(f.pool, rdb, opts)
	res, err = fresh.Resolve(ctx, dom.ID, alias)
	if err != nil {
		t.Fatalf("warm resolve: %v", err)
	}
	if res.Source != redirect.SourceRedis {
		t.Errorf("resolve with a cold memory tier came from %s, want redis: the "+
			"read path and the write path disagree about the key or the payload",
			res.Source)
	}
	if res.Snapshot == nil || res.Snapshot.URL != "https://example.com/cached" {
		t.Errorf("snapshot from Redis = %+v, want the stored destination", res.Snapshot)
	}
}

// Invalidation has to remove the key the write path created. These are the two
// halves whose agreement nothing checked: if either changes its key format, the
// stale entry survives and keeps serving the previous destination for a day.
func TestRedisInvalidationRemovesTheWrittenKey(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	rdb := newRedisClient(t)
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "invald01", "https://example.com/before")

	resolver := redirect.NewResolver(f.pool, rdb, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	if _, err := resolver.Resolve(ctx, dom.ID, alias); err != nil {
		t.Fatal(err)
	}
	if got := aliasKeys(t, rdb); len(got) != 1 {
		t.Fatalf("Redis holds %d alias keys before invalidation, want 1", len(got))
	}

	resolver.InvalidateAlias(ctx, dom.ID, alias)

	if got := aliasKeys(t, rdb); len(got) != 0 {
		t.Errorf("Redis still holds %v after InvalidateAlias; the invalidate path "+
			"is deleting a key the write path did not create", got)
	}
}

// A negative entry must expire. go-redis reads a zero expiration as "no TTL",
// so an operator setting REDIRECT_NEGATIVE_TTL=0 to turn negative caching off
// used to write a permanent key for every well-formed alias anyone probed.
func TestNegativeCacheEntriesAlwaysExpire(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	rdb := newRedisClient(t)
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Zero, the way an operator writes "do not cache misses".
	resolver := redirect.NewResolver(f.pool, rdb, redirect.Options{
		TTL: time.Hour, NegativeTTL: 0,
	})

	res, err := resolver.Resolve(ctx, dom.ID, "nosuchal")
	if err != nil {
		t.Fatalf("resolving a missing alias returned an error: %v", err)
	}
	if res.Source != redirect.SourceNegative {
		t.Errorf("source = %s, want negative", res.Source)
	}

	keys := aliasKeys(t, rdb)
	if len(keys) != 1 {
		t.Fatalf("Redis holds %d keys after a probe, want 1", len(keys))
	}
	ttl, err := rdb.TTL(ctx, keys[0]).Result()
	if err != nil {
		t.Fatal(err)
	}
	// -1 is go-redis for "key exists with no expiry", which is the defect.
	if ttl < 0 {
		t.Errorf("negative cache entry has TTL %v; every 404 probe would be a "+
			"permanent key and a scanner could fill Redis", ttl)
	}
}
