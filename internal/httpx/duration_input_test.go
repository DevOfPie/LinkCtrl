package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// anonReader hides the concrete reader type from httptest.NewRequest, which
// only sets ContentLength for the three readers it recognises. Anything else
// gets ContentLength == -1 — exactly what a real chunked request carries.
type anonReader struct{ io.Reader }

func signReq(body io.Reader) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/links/x/sign", body)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return httptest.NewRecorder(), r
}

func validationCode(t *testing.T, err error, field string) string {
	t.Helper()
	var verrs domain.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("want ValidationErrors, got %v", err)
	}
	for _, ve := range verrs {
		if ve.Field == field {
			return ve.Code
		}
	}
	t.Fatalf("no error for field %q in %v", field, verrs)
	return ""
}

func TestParseSignTTL(t *testing.T) {
	t.Run("plain ttl is honoured", func(t *testing.T) {
		w, r := signReq(strings.NewReader(`{"ttl_seconds": 60}`))
		ttl, err := parseSignTTL(w, r)
		if err != nil || ttl != 60*time.Second {
			t.Fatalf("got ttl=%v err=%v, want 60s", ttl, err)
		}
	})

	t.Run("the ceiling itself is allowed through", func(t *testing.T) {
		w, r := signReq(strings.NewReader(`{"ttl_seconds": 2592000}`))
		ttl, err := parseSignTTL(w, r)
		if err != nil || ttl != link.MaxSignatureTTL {
			t.Fatalf("got ttl=%v err=%v, want %v", ttl, err, link.MaxSignatureTTL)
		}
	})

	t.Run("absent body means the default, signalled as zero", func(t *testing.T) {
		w, r := signReq(nil)
		ttl, err := parseSignTTL(w, r)
		if err != nil || ttl != 0 {
			t.Fatalf("got ttl=%v err=%v, want 0 and no error", ttl, err)
		}
	})

	t.Run("zero and negative are refused here", func(t *testing.T) {
		for _, body := range []string{`{"ttl_seconds": 0}`, `{"ttl_seconds": -5}`} {
			w, r := signReq(strings.NewReader(body))
			_, err := parseSignTTL(w, r)
			if code := validationCode(t, err, "ttl_seconds"); code != "out_of_range" {
				t.Fatalf("body %s: got code %q, want out_of_range", body, code)
			}
		}
	})

	// Everything past the ceiling must come out *past the ceiling*, so the
	// service's own range check refuses it. The interesting inputs are the ones
	// whose product wraps mod 2^64: one lands at ~290ms (a signature that can be
	// expired before the response arrives), one lands negative (which the
	// service reads as "no lifetime named" and silently upgrades to the 24h
	// default — the opposite of the field's refuse-don't-clamp contract).
	t.Run("over-ceiling and wrapping inputs all exceed the ceiling", func(t *testing.T) {
		for _, raw := range []string{
			`{"ttl_seconds": 2592001}`,             // one past the ceiling, no wrap
			`{"ttl_seconds": 18446744074}`,         // wraps to ~290ms
			`{"ttl_seconds": 9223372037}`,          // wraps negative
			`{"ttl_seconds": 9223372036854775807}`, // MaxInt64
		} {
			w, r := signReq(strings.NewReader(raw))
			ttl, err := parseSignTTL(w, r)
			if err != nil {
				t.Fatalf("body %s: unexpected error %v", raw, err)
			}
			if ttl <= link.MaxSignatureTTL {
				t.Fatalf("body %s: got ttl=%v, want a value above %v so the service refuses it",
					raw, ttl, link.MaxSignatureTTL)
			}
		}
	})

	t.Run("a chunked body is decoded, not ignored", func(t *testing.T) {
		w, r := signReq(anonReader{strings.NewReader(`{"ttl_seconds": 60}`)})
		if r.ContentLength != -1 {
			t.Fatalf("precondition: ContentLength = %d, want -1", r.ContentLength)
		}
		ttl, err := parseSignTTL(w, r)
		if err != nil || ttl != 60*time.Second {
			t.Fatalf("got ttl=%v err=%v, want 60s honoured from a chunked body", ttl, err)
		}
	})

	t.Run("a chunked empty body is the empty-body error", func(t *testing.T) {
		w, r := signReq(anonReader{strings.NewReader("")})
		if r.ContentLength != -1 {
			t.Fatalf("precondition: ContentLength = %d, want -1", r.ContentLength)
		}
		_, err := parseSignTTL(w, r)
		if code := validationCode(t, err, "body"); code != "empty" {
			t.Fatalf("got code %q, want empty", code)
		}
	})
}

func TestSaturateDuration(t *testing.T) {
	max := link.MaxSignatureTTL
	cases := []struct {
		name  string
		count int64
		unit  time.Duration
		want  time.Duration
	}{
		{"exact product survives", 90, time.Second, 90 * time.Second},
		{"the ceiling itself survives", 720, time.Hour, max},
		{"one past the ceiling pins to ceiling+unit", 721, time.Hour, max + time.Hour},
		// The web form takes hours; 5124096 of them multiply to ~2^64ns and
		// wrap to a small positive product without the guard.
		{"wrapping hours pin, not wrap", 5124096, time.Hour, max + time.Hour},
		{"MaxInt64 pins", 1<<63 - 1, time.Second, max + time.Second},
		// A negative count whose product would wrap can come out positive and
		// in-band; it must pin past the ceiling on the negative side so a
		// range check still refuses it.
		{"MinInt64 pins negative", -1 << 63, time.Second, -max - time.Second},
		{"small negatives survive for the caller's own check", -5, time.Second, -5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saturateDuration(tc.count, tc.unit, max); got != tc.want {
				t.Fatalf("saturateDuration(%d, %v, %v) = %v, want %v", tc.count, tc.unit, max, got, tc.want)
			}
		})
	}
}

func TestRotateGrace(t *testing.T) {
	inBand := func(g time.Duration) bool {
		return g >= auth.MinRotationGrace && g <= auth.MaxRotationGrace
	}

	t.Run("an in-band request passes through exactly", func(t *testing.T) {
		if g := rotateGrace(600); g != 10*time.Minute {
			t.Fatalf("got %v, want 10m", g)
		}
	})

	t.Run("one past the ceiling stays past the ceiling", func(t *testing.T) {
		if g := rotateGrace(86401); g <= auth.MaxRotationGrace {
			t.Fatalf("got %v, want above %v", g, auth.MaxRotationGrace)
		}
	})

	// The wrap cases. 2^55 seconds multiplies to exactly zero nanoseconds mod
	// 2^64, and zero means "use the default" to the service — a caller asking
	// for a billion years would silently get an hour. 18446744374 wraps to
	// ~300.3s, which sits INSIDE the accepted band, so the service would grant
	// it as if it were a legitimate five-minute window.
	t.Run("wrapping inputs land outside the band, not in it", func(t *testing.T) {
		for _, raw := range []int{
			36028797018963968, // 2^55: wraps to exactly 0 → silent default
			18446744374,       // wraps to ~300.3s → silently accepted in-band
		} {
			g := rotateGrace(raw)
			if g == 0 || inBand(g) {
				t.Fatalf("rotateGrace(%d) = %v, which the service would accept; want out of band", raw, g)
			}
		}
	})
}
