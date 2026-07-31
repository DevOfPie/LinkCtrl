package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// notificationsPageData is what pages/notifications.html renders.
type notificationsPageData struct {
	shell
	Items      []notify.Notification
	NextCursor string
	Notice     string
	Error      string
}

// NotificationsPage lists the signed-in user's own inbox.
func (h *Web) NotificationsPage(w http.ResponseWriter, r *http.Request) {
	data := notificationsPageData{shell: h.shell(r, "Notifications", "notifications")}

	page, err := h.Notify.List(r.Context(), IdentityFrom(r.Context()), notify.Filter{
		Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		h.webError(w, r, err)
		return
	}
	data.Items = page.Items
	data.NextCursor = page.NextCursor

	h.render(w, r, http.StatusOK, "notifications", data)
}

// NotificationRead marks one read and returns to the list.
//
// A form post rather than an hx-post, because the page it returns to has a
// different nav badge than the one it left — swapping a fragment would leave
// the count stale, which is the one number this page exists to make true.
func (h *Web) NotificationRead(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Notify.MarkRead(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/notifications")
}

// NotificationReadAll empties the badge.
func (h *Web) NotificationReadAll(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Notify.MarkAllRead(r.Context(), IdentityFrom(r.Context())); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/notifications")
}
