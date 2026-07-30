// Package httpx contains the HTTP layer: routing, middleware and handlers.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/platform/redis"
)

// Health serves the liveness and readiness endpoints.
type Health struct {
	DB    *pgxpool.Pool
	Redis *goredis.Client

	draining atomic.Bool

	mu       sync.Mutex
	cached   Report
	cachedAt time.Time
}

// Report is the readiness response body.
type Report struct {
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Errors       map[string]string `json:"errors,omitempty"`
}

// readyCacheTTL keeps a probe every few seconds from turning into a query
// every few seconds against a database that may already be struggling.
const readyCacheTTL = time.Second

// StartDraining flips readiness to 503 so a load balancer deregisters this
// instance before the HTTP server stops accepting connections.
func (h *Health) StartDraining() { h.draining.Store(true) }

// Live handles GET /healthz.
//
// Liveness answers exactly one question: is this process wedged? It
// deliberately touches neither Postgres nor Redis. If it did, a database
// outage would cause the orchestrator to kill and restart every replica
// simultaneously, which turns a recoverable dependency failure into a much
// worse outage.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// Ready handles GET /readyz: should traffic be routed here?
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, Report{
			Status:  "draining",
			Version: build.Get().Version,
		})
		return
	}

	rep, ok := h.check(r.Context())
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rep)
}

func (h *Health) check(ctx context.Context) (Report, bool) {
	h.mu.Lock()
	if time.Since(h.cachedAt) < readyCacheTTL && h.cachedAt != (time.Time{}) {
		rep := h.cached
		h.mu.Unlock()
		return rep, rep.Status == "ok" || rep.Status == "degraded"
	}
	h.mu.Unlock()

	rep := Report{
		Status:       "ok",
		Version:      build.Get().Version,
		Dependencies: map[string]string{},
	}
	healthy := true

	// Postgres is fatal: without it the service cannot resolve a single link.
	if h.DB == nil {
		rep.Dependencies["postgres"] = "not configured"
		healthy = false
	} else {
		pingCtx, cancel := context.WithTimeout(ctx, postgres.PingTimeout)
		err := h.DB.Ping(pingCtx)
		cancel()
		if err != nil {
			rep.Dependencies["postgres"] = "down"
			rep.Errors = putErr(rep.Errors, "postgres", err)
			healthy = false
		} else {
			rep.Dependencies["postgres"] = "ok"
		}
	}

	// Redis is not fatal. The redirect path falls through to Postgres and
	// still meets the uncached target, so reporting unready here would remove
	// a working instance from rotation because of a cache problem.
	if h.Redis == nil {
		rep.Dependencies["redis"] = "disabled"
	} else {
		pingCtx, cancel := context.WithTimeout(ctx, redis.PingTimeout)
		err := h.Redis.Ping(pingCtx).Err()
		cancel()
		if err != nil {
			rep.Dependencies["redis"] = "degraded"
			rep.Errors = putErr(rep.Errors, "redis", err)
		} else {
			rep.Dependencies["redis"] = "ok"
		}
	}

	switch {
	case !healthy:
		rep.Status = "unavailable"
	case rep.Dependencies["redis"] == "degraded":
		rep.Status = "degraded"
	}

	h.mu.Lock()
	h.cached, h.cachedAt = rep, time.Now()
	h.mu.Unlock()

	return rep, healthy
}

func putErr(m map[string]string, key string, err error) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[key] = err.Error()
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
