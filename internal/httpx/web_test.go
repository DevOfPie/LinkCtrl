package httpx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// TestAnHTMXRefusalReachesThePage is F218.
//
// htmx's default response handling reads a 4xx, fires an error event, and swaps
// nothing. Every refusal `webError` writes is a 4xx error page, so six controls —
// a routing rule's delete, a split variant's, the link's danger zone, an
// invitation revoke, a member removal, a dispute reviewer revoke — dismissed
// their confirmation and left the page unchanged when the answer was 403 or 409.
// The refusal was rendered and thrown away.
//
// Two halves, and both matter: an htmx request gets something it will swap, and
// an ordinary navigation still gets the page it always got. A fix that turned
// every refusal into a fragment would break the second.
func TestAnHTMXRefusalReachesThePage(t *testing.T) {
	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	h := &Web{UI: r}

	t.Run("an htmx request gets a swappable fragment", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/links/x/rules/y", nil)
		req.Header.Set("HX-Request", "true")

		h.webError(rec, req, fmt.Errorf("%w: you cannot remove the last rule",
			domain.ErrConflict))

		if rec.Code < 200 || rec.Code >= 300 {
			t.Errorf("the refusal answered %d; htmx swaps a 2xx and ignores everything "+
				"else, so any other code is the reader seeing nothing at all", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "you cannot remove the last rule") {
			t.Errorf("the refusal's own sentence is not in the body: %q", body)
		}
		if strings.Contains(body, "<html") || strings.Contains(body, "<title") {
			t.Errorf("a whole page was written where a fragment was asked for; htmx "+
				"would swap the document into whatever element it was updating: %q", body)
		}
	})

	t.Run("an ordinary navigation still gets the page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/links/x/rules/y", nil)

		h.webError(rec, req, fmt.Errorf("%w: you cannot remove the last rule",
			domain.ErrConflict))

		if rec.Code != http.StatusConflict {
			t.Errorf("a browser navigation answered %d, want %d: the status is what "+
				"the reader's browser and every non-htmx client reads",
				rec.Code, http.StatusConflict)
		}
	})
}

// TestAnHTMXHeaderDoesNotTurnEveryRefusalIntoSuccess is review finding 7.
//
// The htmx limb was in [Web.errorPage], which has 77 callers, so a client sending
// `HX-Request: true` — a header it chooses for itself — turned every 401, 403,
// 404, 409, 429 and 500 across the dashboard into a `200`. [Web.tooManyRequests]
// is the `deny` every rate limiter on this surface hands its refusal to, so a
// throttled request answered success.
//
// Two kinds take that path deliberately, and this test is the fence around them.
func TestAnHTMXHeaderDoesNotTurnEveryRefusalIntoSuccess(t *testing.T) {
	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	h := &Web{UI: r}

	htmx := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/anything", nil)
		req.Header.Set("HX-Request", "true")
		return req
	}

	t.Run("a rate-limit refusal keeps its status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.tooManyRequests(rec, htmx())
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("a throttled htmx request answered %d, want 429. The limiter's "+
				"whole job is to say no, and a 200 says the opposite to anything "+
				"reading the status", rec.Code)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound},
		{"unhandled", fmt.Errorf("something broke"), http.StatusInternalServerError},
	} {
		t.Run(tc.name+" keeps its status", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.webError(rec, htmx(), tc.err)
			if rec.Code != tc.want {
				t.Errorf("an htmx %s answered %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}

	// And the two that deliberately do swap, so scoping this did not undo F218.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", fmt.Errorf("%w: nope", domain.ErrForbidden)},
		{"conflict", fmt.Errorf("%w: not yet", domain.ErrConflict)},
	} {
		t.Run("a "+tc.name+" refusal still reaches the page", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.webError(rec, htmx(), tc.err)
			if rec.Code != http.StatusOK {
				t.Errorf("an htmx %s answered %d; htmx swaps a 2xx and discards "+
					"everything else, so the reader sees nothing", tc.name, rec.Code)
			}
		})
	}
}
