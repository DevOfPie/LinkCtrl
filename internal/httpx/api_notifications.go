package httpx

import (
	"math"
	"net/http"
	"strconv"

	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// NotificationAPI exposes the caller's own inbox.
//
// There is no endpoint that creates a notification. Notifications are a
// consequence of something the system observed, and an API that could post one
// would make the inbox a thing callers assert into rather than a record of what
// happened.
type NotificationAPI struct {
	Notify *notify.Service
}

func (a *NotificationAPI) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := notify.Filter{
		Cursor:     q.Get("cursor"),
		UnreadOnly: q.Get("unread") == "true",
	}
	if l := q.Get("limit"); l != "" {
		// Range-checked before narrowing: ?limit=2147483648 would otherwise
		// wrap to a negative int32 and reach the query as a negative LIMIT.
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= math.MaxInt32 {
			f.Limit = int32(n) //nolint:gosec // G109: range-checked on the line above
		}
	}

	page, err := a.Notify.List(r.Context(), IdentityFrom(r.Context()), f)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// Unread answers the badge count on its own, so a client polling it does not
// pay for a page of rows it will not render.
func (a *NotificationAPI) Unread(w http.ResponseWriter, r *http.Request) {
	n, err := a.Notify.Unread(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]int64{"unread": n})
}

// Read marks one notification read. Idempotent, and someone else's id is
// indistinguishable from one that does not exist.
func (a *NotificationAPI) Read(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Notify.MarkRead(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReadAll empties the badge and reports how many it cleared.
func (a *NotificationAPI) ReadAll(w http.ResponseWriter, r *http.Request) {
	n, err := a.Notify.MarkAllRead(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]int64{"marked_read": n})
}
