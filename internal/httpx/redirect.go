package httpx

import (
	_ "embed"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

//go:embed static/404.html
var notFoundPage []byte

//go:embed static/410.html
var gonePage []byte

// RedirectHandler serves GET|HEAD /{alias}.
//
// This is the hot path and the whole reason for the router's shape. It runs
// with no session lookup, no CSRF check and no template rendering, because
// each of those is a cost the 20ms budget cannot absorb — the session check
// alone would be a database round trip on every visit.
type RedirectHandler struct {
	Resolver *redirect.Resolver
	// DomainID is resolved once at boot. Looking it up per request would add a
	// query to the path this whole design exists to keep short.
	DomainID uuid.UUID
	Status   int
	Logger   *slog.Logger
	// LogSample logs one in N successful redirects; 0 disables. Logging every
	// redirect at 2,000 rps produces more bytes than the redirects themselves.
	LogSample int64

	// Recorder receives click events. Nil until M8.
	Recorder ClickRecorder

	counter atomic.Int64
}

// ClickRecorder accepts a click for asynchronous recording.
//
// Deliberately returns nothing. Recording must never fail a redirect, never
// block it, and never be waited on.
type ClickRecorder interface {
	Record(ev ClickEvent)
}

type ClickEvent struct {
	LinkID      uuid.UUID
	WorkspaceID uuid.UUID
	OccurredAt  time.Time
	IP          string
	UserAgent   string
	Referrer    string
	Language    string
	LatencyUS   int32
}

func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	code := strings.TrimSpace(r.PathValue("alias"))

	// Reject anything that cannot be a valid alias before touching the cache
	// or the database. A scanner spraying long random paths would otherwise
	// turn each one into a lookup.
	if len(code) < alias.MinLength || len(code) > alias.MaxLength {
		h.notFound(w, r)
		return
	}
	canonical := alias.Canonical(code)

	res, err := h.Resolver.Resolve(r.Context(), h.DomainID, canonical)
	if err != nil {
		// A resolution failure is a 404 to the visitor, not a 500. They cannot
		// act on the difference, and an error page on a short link is a worse
		// experience than "not found".
		h.Logger.Error("redirect resolution failed",
			slog.String("alias", canonical), slog.Any("error", err))
		h.notFound(w, r)
		return
	}

	switch res.Snapshot.Decide(start) {
	case redirect.OutcomeGone:
		h.gone(w, r)
		return
	case redirect.OutcomeNotFound:
		h.notFound(w, r)
		return
	}

	target := res.Snapshot.URL
	if res.Snapshot.ForwardQuery && r.URL.RawQuery != "" {
		target = appendQuery(target, r.URL.RawQuery)
	}

	// 302, never 301. Links are editable by design, so a permanent redirect
	// would be cached by browsers and intermediaries and keep sending traffic
	// to the old destination long after an edit — with no way to recall it.
	// no-store for the same reason.
	h.Location(w, target, h.status())

	if h.Recorder != nil && r.Method != http.MethodHead {
		h.Recorder.Record(ClickEvent{
			LinkID:      res.Snapshot.LinkID,
			WorkspaceID: res.Snapshot.WorkspaceID,
			OccurredAt:  start,
			IP:          clientIPString(r),
			UserAgent:   r.UserAgent(),
			Referrer:    r.Referer(),
			Language:    r.Header.Get("Accept-Language"),
			LatencyUS:   int32(time.Since(start).Microseconds()),
		})
	}

	if h.LogSample > 0 && h.counter.Add(1)%h.LogSample == 0 {
		h.Logger.Info("redirect",
			slog.String("alias", canonical),
			slog.String("source", string(res.Source)),
			slog.Int64("duration_us", time.Since(start).Microseconds()),
		)
	}
}

// Location writes the redirect response.
func (h *RedirectHandler) Location(w http.ResponseWriter, target string, status int) {
	head := w.Header()
	head.Set("Location", target)
	head.Set("Cache-Control", "private, no-store, max-age=0")
	head.Set("Referrer-Policy", "unsafe-url")
	head.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
}

func (h *RedirectHandler) status() int {
	if h.Status == 0 {
		return http.StatusFound
	}
	return h.Status
}

func (h *RedirectHandler) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, notFoundPage)
}

func (h *RedirectHandler) gone(w http.ResponseWriter, r *http.Request) {
	// 410 rather than 404: the alias really did exist, and Gone tells crawlers
	// and link checkers to stop retrying rather than treating it as transient.
	h.errorPage(w, r, http.StatusGone, gonePage)
}

func (h *RedirectHandler) errorPage(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	head := w.Header()
	head.Set("Content-Type", "text/html; charset=utf-8")
	// Error pages must never be indexed: a shortener accumulates thousands of
	// dead aliases, and letting a crawler index them is pure noise.
	head.Set("X-Robots-Tag", "noindex, nofollow")
	head.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// appendQuery merges the incoming query string into the destination.
//
// The destination's own parameters win on conflict: they were configured
// deliberately, whereas the incoming ones are whatever a visitor arrived with.
func appendQuery(target, incoming string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	existing := u.Query()
	extra, err := url.ParseQuery(incoming)
	if err != nil {
		return target
	}
	for k, vs := range extra {
		if existing.Has(k) {
			continue
		}
		for _, v := range vs {
			existing.Add(k, v)
		}
	}
	u.RawQuery = existing.Encode()
	return u.String()
}

func clientIPString(r *http.Request) string {
	if addr := ClientIPFrom(r.Context()); addr.IsValid() {
		return addr.String()
	}
	return ""
}
