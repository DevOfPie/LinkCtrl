package httpx

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// The QR panel on a link's page (M41).
//
// One form, on the same no-JavaScript shape the folder and campaign pages use:
// two colour inputs, an error-correction select and two numbers, posting to the
// link's own URL. The code beside it is server-rendered SVG, so the panel shows
// what it will produce without a round trip and without a script.
//
// **Reset is a value of the same form rather than a second one.** The style is
// replaced whole by every submit, which is what makes "back to plain black on
// white" a button that posts the defaults instead of a delete somebody has to
// find.

// linkQRPageData is what pages/link_qr.html renders: the panel's contents,
// served as an ordinary page (M48).
//
// Its own struct rather than linkDetailPageData, because the link page's data is
// three page-replacing reads and five soft ones for eight sections, and this
// page has one section. Opening the panel's route must not cost a statistics
// rollup.
type linkQRPageData struct {
	shell
	Link *domain.Link
	linkQRView
	Notice string
}

// LinkQRPage serves the QR panel's contents at their own URL.
//
// **This route is the panel**, and the popover on the link page is what a
// browser does with the same block when it can. m48.md states the property as a
// requirement — *"the panel is a route that renders as an ordinary page when
// opened directly"* — and this handler is one half of it;
// TestEveryPanelIsAlsoACompletePage is the other.
//
// Gated by loading the link, which is `links.read`: the same permission the
// section on the link page is drawn under. Nothing here widens who may see a
// code, and the style form inside it is still drawn only for `links.update`.
func (h *Web) LinkQRPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	actor := IdentityFrom(r.Context())
	l, err := h.Links.Get(r.Context(), actor, id)
	if err != nil {
		h.webError(w, r, err)
		return
	}

	self := "/links/" + id.String() + "/qr"
	h.render(w, r, http.StatusOK, "link_qr", linkQRPageData{
		shell:      h.shell(r, "QR code · /"+l.Alias, "links"),
		Link:       l,
		linkQRView: h.linkQR(r.Context(), actor, l, self),
		Notice:     qrNotice(r.URL.Query().Get("qr")),
	})
}

// qrReturn is where a style write lands, given what the form asked for.
//
// The panel is a route, so a save has two honest destinations — the link page it
// was opened over, and the panel's own page — and only the form knows which one
// the reader is looking at. The value is therefore **matched, never followed**:
// it is compared against the two paths this function builds itself, so the field
// is a choice between two and not a redirect target a POST body can name.
func qrReturn(next string, id interface{ String() string }, marker string) string {
	page := "/links/" + id.String()
	if next == page+"/qr" {
		return page + "/qr?qr=" + marker
	}
	return page + "?qr=" + marker + "#qr"
}

// LinkQRStyle stores how this link's code is drawn.
func (h *Web) LinkQRStyle(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	next := r.PostFormValue("next")

	// The reset button posts the same form under a different name, so the
	// service sees an ordinary style rather than a second operation.
	if r.PostFormValue("reset") != "" {
		if rerr := h.Links.ResetQRStyle(r.Context(), IdentityFrom(r.Context()), id); rerr != nil {
			h.finishQRAction(w, r, id, rerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "reset"))
		return
	}

	style := qr.Style{
		Foreground: strings.TrimSpace(r.PostFormValue("foreground")),
		Background: strings.TrimSpace(r.PostFormValue("background")),
		Level:      qr.Level(strings.TrimSpace(r.PostFormValue("level"))),
		Margin:     formInt(r.PostFormValue("margin"), qr.DefaultMargin),
		Scale:      formInt(r.PostFormValue("scale"), qr.DefaultScale),
	}
	if _, serr := h.Links.SetQRStyle(r.Context(), IdentityFrom(r.Context()), id, style); serr != nil {
		h.finishQRAction(w, r, id, serr)
		return
	}
	seeOther(w, r, qrReturn(next, id, "styled"))
}

// finishQRAction puts a refusal back on the page it was made from, with the
// reason beside the code — the same trade finishFolderAction makes, and for the
// same reason: every refusal here is something the reader can fix in the form
// they are looking at.
func (h *Web) finishQRAction(w http.ResponseWriter, r *http.Request, id interface{ String() string }, err error) {
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		h.webError(w, r, err)
		return
	}
	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	data.QRError = ve[0].Message
	h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
}

// formInt reads a number box, falling back to a default for an empty or
// unreadable one. Out-of-range values are the service's refusal to make, not
// this function's: a form that silently clamped would report success for a
// setting nobody asked for.
func formInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		// Zero, so Normalize's range check refuses it with a sentence rather
		// than this function quietly choosing a value.
		return -1
	}
	return n
}

// qrNotice turns the ?qr= marker into a sentence.
func qrNotice(marker string) string {
	switch marker {
	case "styled":
		return "QR code restyled. The code says the same thing — a style changes how " +
			"it is drawn, never what it encodes."
	case "reset":
		return "QR code back to black on white."
	default:
		return ""
	}
}
