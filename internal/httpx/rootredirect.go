package httpx

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// RootRedirect serves the root of the link domain.
//
// It lives on the redirect tree, under the same latency budget as an alias, and
// the bare domain is a URL crawlers and scanners ask for constantly — so the
// value is cached rather than read per request. A database round trip here would
// put one on the most-probed path in the product for a value that changes
// approximately never.
//
// Refreshed on a TTL and invalidated on write, the same shape as a link
// snapshot. The TTL alone would be enough for correctness and would leave an
// operator reloading the page they just configured and seeing the old answer.
type RootRedirect struct {
	// Load reads the current destination. Empty means "not configured", which
	// is answered 404.
	Load func(context.Context) (string, error)
	// TTL bounds staleness if an invalidation is ever missed. Zero means one
	// minute.
	TTL time.Duration
	// Status is the redirect code, defaulting to 302. It follows the instance's
	// configured default rather than being pinned here: an operator who chose
	// 301 for links made that decision once already. The 302 default is the
	// product's usual reasoning — a 301 cached in browsers and intermediaries
	// cannot be recalled — and it applies most strongly to this destination,
	// which is the one most likely to be repointed later.
	Status int
	// LoadTimeout bounds one refill; see DefaultRootLoadTimeout.
	LoadTimeout time.Duration

	mu       sync.RWMutex
	url      string
	loadedAt time.Time
	valid    bool
	// load admits one refill at a time. Separate from mu because the read it
	// guards is long relative to the field writes mu guards, and holding a
	// write lock across a database call would block every cache *hit* too.
	load sync.Mutex
}

// InvalidateRoot drops the cached value.
func (h *RootRedirect) InvalidateRoot() {
	h.mu.Lock()
	h.valid = false
	h.mu.Unlock()
}

func (h *RootRedirect) ttl() time.Duration {
	if h.TTL <= 0 {
		return time.Minute
	}
	return h.TTL
}

// DefaultRootLoadTimeout bounds one refill when LoadTimeout is zero.
//
// It exists because this handler is mounted on the redirect mux, which
// deliberately has no `RequestTimeout` — that middleware lives in `appHandler`,
// and the redirect tree is outside it. So until F48 the only bound on this read
// was the server's 30s `WriteTimeout`, on the one redirect-tree path that talks
// to Postgres on a request.
const DefaultRootLoadTimeout = 2 * time.Second

func (h *RootRedirect) loadTimeout() time.Duration {
	if h.LoadTimeout <= 0 {
		return DefaultRootLoadTimeout
	}
	return h.LoadTimeout
}

// current returns the cached destination, refreshing when stale.
//
// One refill at a time, and everybody else waits for it. M23's subscriber calls
// InvalidateRoot from `flush()` on every (re-)establishment including the first,
// so a miss here is fleet-simultaneous rather than staggered: without the
// exclusion below, every in-flight request on every replica issues its own query
// for a value that changes approximately never. The query is an indexed lookup
// on a table holding one row, so this is cheap rather than critical — but it is
// cheap in both directions, and the herd is the shape the invalidation design
// guarantees rather than one that needs bad luck.
func (h *RootRedirect) current(ctx context.Context) (string, error) {
	h.mu.RLock()
	if h.valid && time.Since(h.loadedAt) < h.ttl() {
		url := h.url
		h.mu.RUnlock()
		return url, nil
	}
	h.mu.RUnlock()

	h.load.Lock()
	defer h.load.Unlock()

	// Re-check under the load lock. Whoever held it may have just refilled, in
	// which case this request is a hit that arrived a moment early.
	h.mu.RLock()
	if h.valid && time.Since(h.loadedAt) < h.ttl() {
		url := h.url
		h.mu.RUnlock()
		return url, nil
	}
	h.mu.RUnlock()

	// The caller's context is a request's, so cancellation still propagates; the
	// timeout is what the mux does not supply.
	lctx, cancel := context.WithTimeout(ctx, h.loadTimeout())
	defer cancel()

	url, err := h.Load(lctx)
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	h.url, h.loadedAt, h.valid = url, time.Now(), true
	h.mu.Unlock()
	return url, nil
}

func (h *RootRedirect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Load == nil {
		http.NotFound(w, r)
		return
	}

	url, err := h.current(r.Context())
	if err != nil {
		// Same reasoning as a failed alias resolve: this is a link that may
		// exist, so it is a 503 the client may retry, never a 404 that tells a
		// crawler the domain has nothing.
		writeUnavailable(w, r)
		return
	}
	if url == "" {
		// Unconfigured. 404 rather than a default page: an instance that says
		// nothing about itself is a legitimate choice, and it is what the root
		// answered before this setting existed.
		http.NotFound(w, r)
		return
	}

	status := h.Status
	if status == 0 {
		status = http.StatusFound
	}
	// Set by hand rather than through http.Redirect, which writes an HTML body
	// naming the destination. The redirect tree sets nosniff on everything it
	// emits; see redirect.go.
	w.Header().Set("Location", url)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Never cached by an intermediary: the whole point of 302 here is that the
	// operator can repoint it, and a proxy holding the old value defeats that.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
