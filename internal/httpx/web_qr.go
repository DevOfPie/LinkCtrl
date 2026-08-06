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

	// The reset button posts the same form under a different name, so the
	// service sees an ordinary style rather than a second operation.
	if r.PostFormValue("reset") != "" {
		if rerr := h.Links.ResetQRStyle(r.Context(), IdentityFrom(r.Context()), id); rerr != nil {
			h.finishQRAction(w, r, id, rerr)
			return
		}
		seeOther(w, r, "/links/"+id.String()+"?qr=reset#qr")
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
	seeOther(w, r, "/links/"+id.String()+"?qr=styled#qr")
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
