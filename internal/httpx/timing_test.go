package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerTimingIsOffByDefault(t *testing.T) {
	if _, ok := ServerTiming(false)(marker{}).(marker); !ok {
		t.Error("ServerTiming(false) wrapped the handler")
	}

	rec := httptest.NewRecorder()
	ServerTiming(false)(marker{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if got := rec.Header().Get("Server-Timing"); got != "" {
		t.Errorf("Server-Timing = %q with the feature off", got)
	}
}

func TestServerTimingStampsTheResponse(t *testing.T) {
	h := ServerTiming(true)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	got := rec.Header().Get("Server-Timing")
	if !strings.HasPrefix(got, "app;") || !strings.Contains(got, "dur=") {
		t.Errorf("Server-Timing = %q, want an app metric with a duration", got)
	}
}

// A handler that writes a body without calling WriteHeader is sending 200, and
// the header has to be stamped before those bytes leave — otherwise the feature
// silently does nothing for every handler that just writes.
func TestServerTimingStampsAnImplicit200(t *testing.T) {
	h := ServerTiming(true)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	if rec.Header().Get("Server-Timing") == "" {
		t.Error("no Server-Timing on a response written without WriteHeader")
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want it passed through untouched", rec.Body.String())
	}
}

// Wrapping a ResponseWriter is how flushing and hijacking usually get broken.
// http.ResponseController finds the real writer through Unwrap.
func TestServerTimingWriterCanStillBeFlushed(t *testing.T) {
	var flushErr error
	h := ServerTiming(true)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		flushErr = http.NewResponseController(w).Flush()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if flushErr != nil {
		t.Errorf("Flush through the timing writer: %v", flushErr)
	}
}

func TestRequestTimeoutZeroIsTransparent(t *testing.T) {
	if _, ok := RequestTimeout(0)(marker{}).(marker); !ok {
		t.Error("RequestTimeout(0) wrapped the handler")
	}
}

func TestRequestTimeoutGivesTheHandlerADeadline(t *testing.T) {
	var deadline time.Time
	var hadDeadline bool
	h := RequestTimeout(5 * time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, hadDeadline = r.Context().Deadline()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/links", nil))

	if !hadDeadline {
		t.Fatal("handler context carries no deadline")
	}
	if d := time.Until(deadline); d <= 0 || d > 5*time.Second {
		t.Errorf("deadline is %v away, want up to 5s", d)
	}
}

// The deadline is only useful if what a handler does with it turns into a
// sensible response. A 500 would tell the caller nothing about retrying.
func TestExpiredDeadlineBecomesAGatewayTimeout(t *testing.T) {
	h := RequestTimeout(time.Nanosecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		WriteError(w, r, r.Context().Err())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Type != problemBase+"timeout" {
		t.Errorf("problem type = %q, want %stimeout", p.Type, problemBase)
	}
}

// A client closing the connection is not a server fault. Counting it as one
// makes the 5xx alert fire for people closing tabs.
func TestClientDisconnectIsNotAServerError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
	WriteError(rec, req, context.Canceled)

	if rec.Code >= 500 {
		t.Errorf("status = %d, want a 4xx: the client went away", rec.Code)
	}
	if rec.Code != 499 {
		t.Errorf("status = %d, want 499", rec.Code)
	}
}
