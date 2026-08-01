package httpx

import (
	"errors"
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// disputesPageData is what pages/disputes.html renders.
//
// Every string in Items that came from whoever filed the dispute is already
// inert when it arrives — the service defangs on the way in and on the way out.
// This page therefore has nothing to remember, which is the point: a template
// that had to defang would be a template that could forget.
type disputesPageData struct {
	shell
	Items      []dispute.Dispute
	OpenCount  int64
	OpenOnly   bool
	NextCursor string
	Notice     string
	Error      string
}

// DisputesPage is the review queue.
//
// **It renders a URL a stranger chose, to the person who administers the
// instance.** Two rules carry that, and both are asserted against the rendered
// HTML by TestTheQueueNeverRendersADisputedDestinationAsALink: the destination
// appears defanged, and it never appears inside an anchor, a form action, an
// image source or anything else a browser would follow.
//
// The server also never fetches it. There is no preview, no screenshot and no
// liveness check anywhere behind this page.
func (h *Web) DisputesPage(w http.ResponseWriter, r *http.Request) {
	actor := IdentityFrom(r.Context())
	data := disputesPageData{
		shell: h.shell(r, "Blocked destinations", "disputes"),
		// Open-first is the default because this is a queue rather than an
		// archive: the reason to open it is that something is waiting.
		OpenOnly: r.URL.Query().Get("all") != "1",
	}

	n, err := h.Disputes.CountOpen(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return
	}
	data.OpenCount = n

	page, err := h.Disputes.List(r.Context(), actor, dispute.Filter{
		Cursor:   r.URL.Query().Get("cursor"),
		OpenOnly: data.OpenOnly,
	})
	if err != nil {
		h.webError(w, r, err)
		return
	}
	data.Items = page.Items
	data.NextCursor = page.NextCursor

	switch r.URL.Query().Get("decided") {
	case "allowed":
		data.Notice = "Allowed. The blocklist entry is gone and the person who asked has been told."
	case "upheld":
		data.Notice = "Upheld. The destination stays refused and the person who asked has been told."
	}

	h.render(w, r, http.StatusOK, "disputes", data)
}

// DisputeAllow lifts the entry that refused the destination.
func (h *Web) DisputeAllow(w http.ResponseWriter, r *http.Request) {
	h.decideDispute(w, r, "allowed")
}

// DisputeUphold leaves the refusal standing.
func (h *Web) DisputeUphold(w http.ResponseWriter, r *http.Request) {
	h.decideDispute(w, r, "upheld")
}

func (h *Web) decideDispute(w http.ResponseWriter, r *http.Request, outcome string) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}

	decide := h.Disputes.Uphold
	if outcome == "allowed" {
		decide = h.Disputes.Allow
	}
	if _, err := decide(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/disputes?decided="+outcome)
}

// DisputeFile is how a person who was refused asks for a review.
//
// It posts the URL they typed, not a reference to the refusal, because the
// refusal is not a row anybody holds an id for — the service re-judges the URL
// and only a low-confidence verdict produces a dispute. So a form field cannot
// claim an appealable tier, whatever it says.
//
// It answers by returning to the links page carrying the outcome, rather than by
// rendering a page of its own. There is nothing to show: the interesting state
// is now in somebody else's queue.
func (h *Web) DisputeFile(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	_, err := h.Disputes.File(r.Context(), IdentityFrom(r.Context()),
		strings.TrimSpace(r.PostFormValue("url")))
	if err != nil {
		// A refusal to file is a sentence on the next page, not a field
		// highlight: the form this came from is the link form, which has already
		// been re-rendered once carrying the block that started the whole thing.
		switch {
		case errors.Is(err, domain.ErrValidation):
			seeOther(w, r, "/links?dispute=refused")
		case errors.Is(err, domain.ErrConflict):
			seeOther(w, r, "/links?dispute=duplicate")
		default:
			h.webError(w, r, err)
		}
		return
	}
	seeOther(w, r, "/links?dispute=filed")
}
