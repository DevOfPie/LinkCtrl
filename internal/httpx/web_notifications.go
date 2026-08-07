package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// notificationView is one notification as a surface renders it: the row, plus
// where it leads.
//
// The destination is computed once, here, rather than in the template — a
// template that had to branch on a kind would be a second enumeration of the
// vocabulary, and notificationTargets is deliberately the only one. `Target` is
// empty for a kind that leads nowhere, and both surfaces read that as "draw no
// open control" rather than as a missing value.
type notificationView struct {
	notify.Notification
	Target string
}

// notificationViews attaches a destination to each row.
func notificationViews(items []notify.Notification) []notificationView {
	out := make([]notificationView, 0, len(items))
	for _, n := range items {
		out = append(out, notificationView{
			Notification: n,
			Target:       notificationTarget(n.Kind, n.Data),
		})
	}
	return out
}

// notificationsPageData is what pages/notifications.html renders.
type notificationsPageData struct {
	shell
	Items      []notificationView
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
	data.Items = notificationViews(page.Items)
	data.NextCursor = page.NextCursor

	h.render(w, r, http.StatusOK, "notifications", data)
}

// NotificationOpen goes to what a notification is about, and marks it read
// (M48).
//
// **A POST, not a link.** Opening one changes state, and a state change behind a
// GET is one a prefetch, a link checker or an <img> on somebody else's page can
// fire — the notification would be read before anybody saw it. The surfaces
// render the title as a submit button styled as a heading, which is the same
// trade the sign-out control in the header already makes.
//
// **The destination is computed from the row, never from the request.** The id
// in the path is the only thing the caller supplies; the kind and the data come
// out of the database, and the URL comes out of notificationTargets. A handler
// that redirected to a form field would be an open redirect with a notification
// in front of it.
//
// Read first and mark second, so a row that cannot be read leaves the badge
// alone. Marking a notification read that the reader is then not sent to would
// be the worst of both.
func (h *Web) NotificationOpen(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	actor := IdentityFrom(r.Context())

	n, err := h.Notify.Get(r.Context(), actor, id)
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Notify.MarkRead(r.Context(), actor, id); err != nil {
		h.webError(w, r, err)
		return
	}

	// A kind that leads nowhere is not an error and is not reachable from the
	// UI, which draws no control for one. Arriving here by hand lands on the
	// list, having marked the notification read — which is what the button next
	// to it would have done anyway.
	to := notificationTarget(n.Kind, n.Data)
	if to == "" {
		to = "/notifications"
	}
	seeOther(w, r, to)
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

// NotificationUnread puts one back in the unread list (M48).
//
// The owner's note is the whole justification — *"No way to mark a read message
// as unread if it was accidentally marked as read"* — and this milestone is what
// makes the accident common, because opening a notification now marks it read on
// the way past.
func (h *Web) NotificationUnread(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Notify.MarkUnread(r.Context(), IdentityFrom(r.Context()), id); err != nil {
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
