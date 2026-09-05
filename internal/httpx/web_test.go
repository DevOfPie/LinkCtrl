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
