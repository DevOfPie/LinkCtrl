package httpx

import (
	"context"
	"math"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
)

// DisputeAPI exposes the blocked-attempt appeal path and the review queue.
//
// **Nothing here fetches the destination it is handling.** Not to preview it,
// not to screenshot it, not to check whether it still resolves. A preview fetch
// would be exactly the SSRF the destination validator exists to refuse, arriving
// as a convenience feature — and this file is one of the ones
// TestTheQueueFetchesNothing parses to make sure it stays true.
//
// Nothing here defangs either, and that is not an omission: the service returns
// a Dispute whose destination and host are already inert, so no handler and no
// template is the place where remembering has to happen.
type DisputeAPI struct {
	Disputes *dispute.Service
}

// fileDisputeRequest is a URL and nothing else.
//
// No reason, no note, no message to the reviewer. A dispute says "look at this
// host"; a free-text field would be a second stranger-controlled string rendered
// to the person who administers the instance, buying context the defanged URL
// and the reason code already carry.
type fileDisputeRequest struct {
	URL string `json:"url"`
}

// File opens a dispute about a destination that was refused.
//
// The tier is re-derived from the URL inside the service. There is deliberately
// no field here naming a refusal, a reason code or an audit record: a caller who
// could name the tier could claim the appealable one, which is the whole of what
// "creatable only from a low-confidence refusal" has to prevent.
func (a *DisputeAPI) File(w http.ResponseWriter, r *http.Request) {
	var req fileDisputeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	d, err := a.Disputes.File(r.Context(), IdentityFrom(r.Context()), req.URL)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, d)
}

// List answers one page of the queue, newest first.
func (a *DisputeAPI) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := dispute.Filter{Cursor: q.Get("cursor"), OpenOnly: q.Get("open") == "true"}
	if l := q.Get("limit"); l != "" {
		// Range-checked before narrowing, the same trap as every other list:
		// ?limit=2147483648 would otherwise wrap to a negative int32 and reach
		// the query as a negative LIMIT.
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= math.MaxInt32 {
			f.Limit = int32(n) //nolint:gosec // G109: range-checked on the line above
		}
	}

	page, err := a.Disputes.List(r.Context(), IdentityFrom(r.Context()), f)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// Allow lifts the blocklist entry that refused the destination.
func (a *DisputeAPI) Allow(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, a.Disputes.Allow)
}

// Uphold leaves the refusal standing.
func (a *DisputeAPI) Uphold(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, a.Disputes.Uphold)
}

// decideOp is the shape both decisions share. Named so the two handlers above
// are one line each and cannot drift apart in their error handling.
type decideOp func(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*dispute.Dispute, error)

func (a *DisputeAPI) decide(w http.ResponseWriter, r *http.Request, op decideOp) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := op(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, d)
}
