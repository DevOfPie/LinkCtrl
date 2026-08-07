package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// The QR panel on a link's page (M41, reworded by M49).
//
// One form, on the same no-JavaScript shape the folder and campaign pages use:
// two colour inputs and one number, posting to the link's own URL. The code
// beside it is server-rendered SVG, so the panel shows what it will produce
// without a round trip and without a script.
//
// **The number is the output size in pixels, and it used to be two numbers
// nobody knows.** The form asked for a quiet zone in modules and a module size
// in pixels; the person printing a poster knows neither and knows how big they
// want it. Both survive as the arithmetic behind the one control — see
// qr.FitSize — and the error-correction level left this surface for the API,
// because it is a scannability tradeoff a dashboard user has no basis to make.
// A save therefore carries the level the link already had rather than the
// default, which is what SetQRSize is for.
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

	// Which code the panel is open on. A slug that names nothing falls back to
	// the default inside linkQR rather than 404ing: the panel is a section of a
	// page that exists, and a code somebody removed in another tab should show
	// the link's codes rather than an error page.
	slug := r.URL.Query().Get("code")
	self := "/links/" + id.String() + "/qr"
	h.render(w, r, http.StatusOK, "link_qr", linkQRPageData{
		shell:      h.shell(r, "QR code · /"+l.Alias, "links"),
		Link:       l,
		linkQRView: h.linkQR(r.Context(), actor, l, self, slug),
		Notice:     qrNotice(r.URL.Query()),
	})
}

// qrReturn is where a style write lands, given what the form asked for.
//
// The panel is a route, so a save has two honest destinations — the link page it
// was opened over, and the panel's own page — and only the form knows which one
// the reader is looking at. The value is therefore **matched, never followed**:
// it is compared against the two paths this function builds itself, so the field
// is a choice between two and not a redirect target a POST body can name.
//
// `extra` is what the notice on the far side needs and the redirect is the only
// thing that survives to carry: a POST that snapped the size has to say so, and
// the page it lands on was rendered by a fresh request that never saw the number
// anybody typed.
func qrReturn(next string, id interface{ String() string }, marker, slug string, extra url.Values) string {
	q := url.Values{"qr": {marker}}
	for k, v := range extra {
		q[k] = v
	}
	page := "/links/" + id.String()
	if next == page+"/qr" {
		// The code being worked on survives the redirect, or every save on a
		// named code would drop the reader back onto the default one.
		if slug != "" {
			q.Set("code", slug)
		}
		return page + "/qr?" + q.Encode()
	}
	return page + "?" + q.Encode() + "#qr"
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
	slug := r.PostFormValue("code")
	actor := IdentityFrom(r.Context())

	// Adding and removing a code post the same form under their own names, the
	// way reset already does: one action attribute, one place a refusal comes
	// back to, and no second surface for managing a list that lives inside this
	// one.
	if r.PostFormValue("add") != "" {
		code, aerr := h.Links.CreateQRCode(r.Context(), actor, id, r.PostFormValue("label"))
		if aerr != nil {
			h.finishQRAction(w, r, id, aerr)
			return
		}
		// Landing on the code just made, because somebody who added one is about
		// to style it or download it.
		seeOther(w, r, qrReturn(next, id, "added", code.Slug, nil))
		return
	}
	if r.PostFormValue("remove") != "" {
		if derr := h.Links.DeleteQRCode(r.Context(), actor, id, slug); derr != nil {
			h.finishQRAction(w, r, id, derr)
			return
		}
		// Back to the default code: the one just removed has no page left.
		seeOther(w, r, qrReturn(next, id, "removed", "", nil))
		return
	}
	if r.PostFormValue("rename") != "" {
		if _, rerr := h.Links.SetQRCodeLabel(
			r.Context(), actor, id, slug, r.PostFormValue("label"),
		); rerr != nil {
			h.finishQRAction(w, r, id, rerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "renamed", slug, nil))
		return
	}

	// Removing the logo is a fourth named button rather than a form of its own
	// (M50.5). The upload has to be its own form — a file needs
	// `multipart/form-data`, which this handler does not read — but the removal
	// carries no body at all, so putting it here keeps the refusal coming back
	// to the panel the way every other refusal in this handler does.
	if r.PostFormValue("remove_logo") != "" {
		if lerr := h.Links.ClearQRCodeLogo(r.Context(), actor, id, slug); lerr != nil {
			h.finishQRAction(w, r, id, lerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "logo_removed", slug, nil))
		return
	}

	// The reset button posts the same form under a different name, so the
	// service sees an ordinary style rather than a second operation.
	if r.PostFormValue("reset") != "" {
		if rerr := h.Links.ResetQRStyle(r.Context(), actor, id); rerr != nil {
			h.finishQRAction(w, r, id, rerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "reset", "", nil))
		return
	}

	in := link.QRSizeInput{
		Foreground: strings.TrimSpace(r.PostFormValue("foreground")),
		Background: strings.TrimSpace(r.PostFormValue("background")),
		// No fallback that would make sense here: a size box submitted empty is
		// somebody who cleared it, not somebody asking for the default, and -1
		// is refused with a sentence naming the range.
		Size: formInt(r.PostFormValue("size"), -1),
	}
	_, fit, serr := h.Links.SetQRSizeBySlug(r.Context(), actor, id, slug, in)
	if serr != nil {
		h.finishQRAction(w, r, id, serr)
		return
	}

	// Only when it moved. A redirect carrying "you asked for 300 and got 300"
	// would put a sentence on the page for every save, which is how a message
	// that matters stops being read.
	var extra url.Values
	if fit.Snapped() {
		extra = url.Values{
			"want": {strconv.Itoa(fit.Requested)},
			"got":  {strconv.Itoa(fit.Size)},
		}
	}
	seeOther(w, r, qrReturn(next, id, "styled", slug, extra))
}

// LinkQRLogo stores an image against the code the panel is open on (M50.5).
//
// **Its own route because a file needs its own body.** Every other write in this
// panel posts `application/x-www-form-urlencoded` to `POST /links/{id}/qr`,
// which LinkQRStyle reads with `parseForm`; a file cannot travel in that, and a
// handler that branched on the content type would be two handlers wearing one
// name. So the upload posts here and the removal stays there, which is also
// what keeps the removal available with no file selected.
//
// **`next` and `code` arrive as parts of the multipart body**, because that is
// the only body this form has. They are the same two hidden inputs every other
// form in the panel carries and they mean the same things — where the save
// returns to, matched against the two paths qrReturn builds itself, and which
// code is being edited, which the service refuses if the link does not have it.
//
// The default code is reachable here exactly as a named one is: its `code` value
// is the empty string, which is its identity (D130) and what the owner's ruling
// of 2026-08-07 made addressable for a logo.
func (h *Web) LinkQRLogo(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	upload, err := readUploadedFile(w, r, "logo")
	if err != nil {
		h.finishQRAction(w, r, id, err)
		return
	}
	slug := upload.Fields.Get("code")
	if _, serr := h.Links.SetQRCodeLogo(
		r.Context(), IdentityFrom(r.Context()), id, slug, upload.File,
	); serr != nil {
		h.finishQRAction(w, r, id, serr)
		return
	}
	seeOther(w, r, qrReturn(upload.Fields.Get("next"), id, "logo", slug, nil))
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
		// Out of every range this product has, so the validator downstream
		// refuses it with a sentence rather than this function quietly choosing
		// a value. *(The comment here said "Zero" and the code has returned -1
		// since M41; zero is a value Normalize reads as "unset" and fills in with
		// a default, which is the opposite of what this line is for. Corrected
		// 2026-08-07 under M49, which is the milestone that started routing a
		// size through it.)*
		return -1
	}
	return n
}

// qrNotice turns the ?qr= marker into a sentence.
//
// **The snap is reported here or nowhere.** m49.md requires the size actually
// produced to appear beside the one that was asked for, and after the redirect
// the form shows the produced size with nothing to say it is not the number the
// reader typed. So the pair travels in the query and this is where it is spent.
//
// Both numbers are re-derived as integers rather than echoed: they arrive in a
// URL anybody can edit, and a sentence built from a query string is a sentence
// somebody else can write. A value that is not a number in range says nothing,
// which leaves the ordinary "restyled" message.
func qrNotice(q url.Values) string {
	switch q.Get("qr") {
	case "styled":
		const unchanged = "The code says the same thing — a style changes how it is " +
			"drawn, never what it encodes."
		want, wok := sizeParam(q.Get("want"))
		got, gok := sizeParam(q.Get("got"))
		if !wok || !gok || want == got {
			return "QR code restyled. " + unchanged
		}
		return fmt.Sprintf("QR code restyled, and drawn at %dpx rather than the %dpx you "+
			"asked for: a code is a whole number of squares, and %dpx is the nearest size "+
			"that keeps them whole. %s", got, want, got, unchanged)
	case "reset":
		return "QR code back to black on white."
	case "added":
		return "A second code, with a name and an identity of its own. It points at the " +
			"same destination — what it changes is that a scan of this one is told apart " +
			"from a scan of the others."
	case "removed":
		return "Code removed. What it already recorded stays in the breakdown; anything " +
			"printed with it is counted as the link's original code from now on."
	case "renamed":
		return "Code renamed. The name is what the dashboard calls it — what the code " +
			"says is unchanged, so nothing already printed is affected."
	case "logo":
		// M50.5's version of this said nothing drew the logo yet, because nothing
		// did. M50.6 is the milestone that made that sentence false, so it is
		// M50.6's to replace — and what it replaces it with is the two things that
		// changed about the picture, because both are visible and neither was
		// asked for directly.
		return "Logo stored, re-encoded as a PNG by this server, and drawn in the middle " +
			"of the code. Error correction is now at level H, which is what makes a " +
			"code readable with part of it covered — so the picture is a little denser " +
			"than it was. What the code says is unchanged."
	case "logo_removed":
		return "Logo removed. The image is gone from the row rather than merely " +
			"unreferenced, so nothing is left behind. The code stays at error " +
			"correction level H: dropping it back would redraw a picture that may " +
			"already be printed, and H is the safer of the two to be left at."
	default:
		return ""
	}
}

// sizeParam reads a pixel size out of the query string.
//
// Bounded above by what internal/qr will draw and below by one pixel rather than
// by qr.MinSize: the *requested* size is inside the range, but the size it snaps
// to can sit below the floor — the shortest code at the smallest scale is 58px,
// and a request for 64 lands there. Refusing to mention that would suppress the
// sentence in exactly the case it explains most.
func sizeParam(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > qr.MaxSize {
		return 0, false
	}
	return n, true
}
