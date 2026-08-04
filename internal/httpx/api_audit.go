package httpx

import (
	"math"
	"net/http"
	"strconv"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
)

// AuditAPI exposes the audit log.
//
// Read-only, and there is no write endpoint by design: events are a consequence
// of actions taken elsewhere, and an API that could post one would make the log
// a thing a caller asserts rather than a thing the system observed.
type AuditAPI struct {
	Audit *audit.Service
}

// List answers one page of the audit records the caller's own authority covers
// in their organization.
//
// Authorization is the ordinary permission check in the service — audit.read —
// with nothing here about which credential the caller used. That the permission
// is not delegable to an API key is enforced once, in auth.NonDelegableScopes,
// so no key can ever hold it and this handler never has to ask.
func (a *AuditAPI) List(w http.ResponseWriter, r *http.Request) {
	page, err := a.Audit.List(r.Context(), IdentityFrom(r.Context()), auditFilter(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// ListInstance answers one page of the instance-wide audit records — the acts
// that belong to the instance rather than to any tenant (F36).
//
// Gated on audit.read.instance in the service, which only the instance principal
// holds (D98). A separate endpoint rather than a query parameter on the one
// above, because it is a separate authorization and a separate result set: a
// flag on a list is a thing a client sets by accident, and this one would
// silently change which permission the request needed.
func (a *AuditAPI) ListInstance(w http.ResponseWriter, r *http.Request) {
	page, err := a.Audit.ListInstance(r.Context(), IdentityFrom(r.Context()), auditFilter(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// auditFilter reads the page request both surfaces share.
func auditFilter(r *http.Request) audit.Filter {
	q := r.URL.Query()

	f := audit.Filter{Cursor: q.Get("cursor")}
	if l := q.Get("limit"); l != "" {
		// Range-checked before narrowing, the same trap as the link list:
		// ?limit=2147483648 would otherwise wrap to a negative int32 and reach
		// the query as a negative LIMIT. Out-of-range values fall through to
		// the service default rather than erroring.
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= math.MaxInt32 {
			f.Limit = int32(n) //nolint:gosec // G109: range-checked on the line above
		}
	}
	return f
}
