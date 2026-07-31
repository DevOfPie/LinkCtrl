package httpx

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// RequestTimeout bounds how long a request may spend in the application tree.
//
// A context deadline rather than http.TimeoutHandler, deliberately. The stdlib
// handler buffers the entire response in memory so it can replace it with a 503,
// which is a real cost on every request to gain a guarantee this service does
// not need: every database call here takes a context, so the deadline is what
// actually stops the work. What arrives at the client is then a 504 from the
// error mapper rather than a fabricated one from middleware.
//
// The redirect tree is deliberately not wrapped. It has its own, much shorter
// budget — REDIRECT_TIMEOUT, applied where the resolver would touch Postgres —
// and a 15-second ceiling would be meaningless there.
//
// A duration of zero disables it, returning the handler untouched.
func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	if d <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ServerTiming emits a Server-Timing header carrying the server's own view of
// how long a response took.
//
// Off by default, and that is a security default rather than a performance one:
// the header publishes internal timings to anyone who asks, and on a service
// where the interesting question is "does this alias exist" a timing difference
// is an answer. It is a development and debugging aid.
//
// What it measures is the interval from entering this middleware to the handler
// deciding a status code — time to headers, not time to last byte. A header
// cannot be set after the response has started, so the alternative would be
// trailers, which no browser surfaces in the place a reader would look.
//
// Never applied to the redirect tree: measuring it would mean instrumenting the
// path whose entire budget is 20ms, and the histogram already measures it more
// precisely.
func ServerTiming(enabled bool) func(http.Handler) http.Handler {
	if !enabled {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&timingWriter{ResponseWriter: w, start: time.Now()}, r)
		})
	}
}

// timingWriter stamps the header at the moment the status is written, which is
// the last point at which a header can still be added.
type timingWriter struct {
	http.ResponseWriter
	start   time.Time
	stamped bool
}

func (t *timingWriter) WriteHeader(code int) {
	t.stamp()
	t.ResponseWriter.WriteHeader(code)
}

func (t *timingWriter) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader is sending 200, and this
	// is the last moment before the header block leaves.
	t.stamp()
	return t.ResponseWriter.Write(b)
}

func (t *timingWriter) stamp() {
	if t.stamped {
		return
	}
	t.stamped = true
	ms := float64(time.Since(t.start).Microseconds()) / 1000
	t.Header().Set("Server-Timing", fmt.Sprintf("app;desc=\"server\";dur=%.1f", ms))
}

// Unwrap lets http.ResponseController reach the real writer, so wrapping does
// not quietly break flushing or hijacking.
func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
