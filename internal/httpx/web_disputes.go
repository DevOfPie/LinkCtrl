package httpx

import (
	"errors"
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
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
	// CanDecide is the deciding half of the permission D98 split in two. It
	// draws the Allow and Uphold controls, and nothing else: the enforcement is
	// in the service, which refuses the POST whatever this says. Somebody
	// holding only the reading half sees the queue and no buttons.
	CanDecide bool
	// CanAdminister is the instance-level principal, and it draws the reviewer
	// roster below the queue. False for a reviewer the principal appointed —
	// D98's bound is that a holder of instance-level review may not confer it
	// onwards, so they see neither the list nor the form.
	CanAdminister bool
	// Reviewers is the roster, loaded only when CanAdminister. Empty otherwise,
	// and empty is never rendered.
	Reviewers []instance.Reviewer
	// ReviewersReturn is where a grant or a revoke lands, carried on the forms
	// inside the panel (M48). See reviewersReturn.
	ReviewersReturn string
}

// reviewerRosterPath is the panel's own route: the reviewer roster served as an
// ordinary page.
const reviewerRosterPath = "/disputes/reviewers"

// reviewersReturn is where a reviewer write lands, given what the form asked
// for.
//
// Matched, never followed — the same rule qrReturn applies, and for the same
// reason. Two surfaces render the roster since M48, and the form is the only
// thing that knows which one the reader is standing on; the value is compared
// against the two literals below and anything else falls back to the queue.
func reviewersReturn(next, outcome string) string {
	if next == reviewerRosterPath {
		return reviewerRosterPath + "?reviewers=" + outcome
	}
	return "/disputes?reviewers=" + outcome
}

// reviewersPageData is what pages/dispute_reviewers.html renders.
type reviewersPageData struct {
	shell
	Reviewers       []instance.Reviewer
	ReviewersReturn string
	Notice          string
	Error           string
}

// DisputeReviewersPage serves the reviewer roster at its own URL (M48).
//
// The panel's route, and the second caller of the pattern the QR area
// introduced: what is on the queue is a summary, and changing the list is here.
//
// **The gate is unchanged and it is checked here rather than left to the POST.**
// `Instance.Reviewers` refuses anybody without instance.admin, which is exactly
// the guard the section on the queue is drawn under (D98) — a page that rendered
// the form for somebody who cannot use it would be offering a control that
// answers 403.
func (h *Web) DisputeReviewersPage(w http.ResponseWriter, r *http.Request) {
	reviewers, err := h.Instance.Reviewers(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		h.webError(w, r, err)
		return
	}

	data := reviewersPageData{
		shell:           h.shell(r, "Who reviews disputes", "disputes"),
		Reviewers:       reviewers,
		ReviewersReturn: reviewerRosterPath,
	}
	switch r.URL.Query().Get("reviewers") {
	case "granted":
		data.Notice = "Appointed. They can now read this queue and decide what is in it."
	case "revoked":
		data.Notice = "Withdrawn. They keep every permission their own organization gives them."
	case "unknown":
		data.Error = "No account on this instance has that address."
	}
	h.render(w, r, http.StatusOK, "dispute_reviewers", data)
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
		OpenOnly:      r.URL.Query().Get("all") != "1",
		CanDecide:     actor.Can(dispute.PermDecide),
		CanAdminister: actor.Can(instance.PermAdmin),
		// The panel's forms are rendered on this page, so a write from inside
		// the popup comes back to the queue rather than to the panel's route.
		ReviewersReturn: "/disputes",
	}

	if data.CanAdminister && h.Instance != nil {
		// Failure here is not a reason to replace the queue with an error: the
		// roster is a section below it, and the page's actual subject is still
		// readable. Reported in place, where somebody can see the list did not
		// load rather than assume it is empty.
		reviewers, err := h.Instance.Reviewers(r.Context(), actor)
		if err != nil {
			data.Error = "The reviewer list could not be loaded."
		} else {
			data.Reviewers = reviewers
		}
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
	switch r.URL.Query().Get("reviewers") {
	case "granted":
		data.Notice = "Appointed. They can now read this queue and decide what is in it."
	case "revoked":
		data.Notice = "Withdrawn. They keep every permission their own organization gives them."
	case "unknown":
		data.Error = "No account on this instance has that address."
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

// DisputeReviewerGrant appoints somebody to this instance's review queue.
//
// It carries an address rather than an id, because the principal appointing
// somebody knows who they are and not what their uuid is. An address with no
// account is a sentence on the page rather than an error page: the form is on
// the queue, and mistyping an address is not a reason to take the queue away.
func (h *Web) DisputeReviewerGrant(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	next := r.PostFormValue("next")
	_, err := h.Instance.GrantReviewer(r.Context(), IdentityFrom(r.Context()),
		strings.TrimSpace(r.PostFormValue("email")))
	switch {
	case errors.Is(err, domain.ErrValidation):
		seeOther(w, r, reviewersReturn(next, "unknown"))
	case err != nil:
		h.webError(w, r, err)
	default:
		seeOther(w, r, reviewersReturn(next, "granted"))
	}
}

// DisputeReviewerRevoke withdraws it.
//
// It cannot reach the principal's own instance.admin — the service revokes only
// what auth.InstanceGrantable holds — so the instance cannot be left with nobody
// able to appoint anybody. The page also declines to draw the control against
// the signed-in account, which is an affordance rather than the enforcement.
func (h *Web) DisputeReviewerRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	if err := h.Instance.RevokeReviewer(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, reviewersReturn(r.PostFormValue("next"), "revoked"))
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
