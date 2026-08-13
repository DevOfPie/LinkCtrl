package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

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

// LinkQRPage serves the QR contents at their own URL.
//
// **The popup this route backed retired at the F212 reopening** (2026-08-11):
// the QR tab renders the same block in flow now. The route outlived it — the
// codes list selects a code through it (`?code=`), and it is what a bookmark
// and a shared URL reach. TestEveryPanelIsAlsoACompletePage still holds it to
// rendering as a complete page.
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
	// tab=qr since M47's reopening made the page tabs: every write this
	// function routes is QR work, so the link-page destination is the QR tab,
	// derived from what the handler is exactly as the destination itself is —
	// the field stays a choice between two surfaces and still cannot name a
	// third (D178).
	q.Set("tab", "qr")
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
			h.finishQRAction(w, r, id, next, slug, aerr)
			return
		}
		// Landing on the code just made, because somebody who added one is about
		// to style it or download it.
		seeOther(w, r, qrReturn(next, id, "added", code.Slug, nil))
		return
	}
	if r.PostFormValue("remove") != "" {
		promoted, derr := h.Links.DeleteQRCode(r.Context(), actor, id, slug)
		if derr != nil {
			h.finishQRAction(w, r, id, next, slug, derr)
			return
		}
		// Back to the default code: the one just removed has no page left. The
		// promotion travels as its own marker because the page it lands on is a
		// fresh request that never saw the delete (D183), and it is *stated*
		// rather than left to be noticed, since it moves where every untagged
		// picture of this link lands.
		//
		// The slug travels and the label does not. A label is workspace free text
		// and a sentence assembled out of a query string is a sentence somebody
		// else can write — the trade `dims` makes, for the same reason — while a
		// slug is `[a-z0-9]` and is re-checked on the far side against the shape
		// this product generates.
		marker, extra := "removed", url.Values(nil)
		if promoted != nil {
			marker, extra = "promoted", url.Values{"promoted": {promoted.Slug}}
		}
		seeOther(w, r, qrReturn(next, id, marker, "", extra))
		return
	}
	// Making a code the default (D183). Its own button on the row, beside that
	// row's Remove and carrying the same `code` field: the flag is a property of
	// *that* code, and a control that acted on whichever code the form beside the
	// list happened to be editing is the defect this reopening also fixes in
	// Restore defaults.
	if r.PostFormValue("make_default") != "" {
		if _, derr := h.Links.SetDefaultQRCode(r.Context(), actor, id, slug); derr != nil {
			h.finishQRAction(w, r, id, next, slug, derr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "defaulted", slug, nil))
		return
	}
	if r.PostFormValue("rename") != "" {
		if _, rerr := h.Links.SetQRCodeLabel(
			r.Context(), actor, id, slug, r.PostFormValue("label"),
		); rerr != nil {
			h.finishQRAction(w, r, id, next, slug, rerr)
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
			h.finishQRAction(w, r, id, next, slug, lerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "logo_removed", slug, nil))
		return
	}

	// The reset button posts the same form under a different name, so the
	// service sees an ordinary style rather than a second operation.
	//
	// **It carries the form's `code` since D183, and the redirect keeps it.** It
	// took no slug, so pressing it while a named code was selected cleared the
	// *default* code's style and dropped the reader onto the default code — a
	// button on a form about one code writing to another, which is the second
	// half of F222. It is scoped to the selection now, and returns to it.
	if r.PostFormValue("reset") != "" {
		if rerr := h.Links.ResetQRStyleBySlug(r.Context(), actor, id, slug); rerr != nil {
			h.finishQRAction(w, r, id, next, slug, rerr)
			return
		}
		seeOther(w, r, qrReturn(next, id, "reset", slug, nil))
		return
	}

	in := link.QRSizeInput{
		Foreground: strings.TrimSpace(r.PostFormValue("foreground")),
		Background: strings.TrimSpace(r.PostFormValue("background")),
		Size:       requestedQRSize(r),
	}
	if _, _, serr := h.Links.SetQRSizeBySlug(r.Context(), actor, id, slug, in); serr != nil {
		h.finishQRAction(w, r, id, next, slug, serr)
		return
	}
	// No `extra` any more. The size the form asked for is the size that is
	// stored, so there is nothing for the far side to report (D182); the pair of
	// numbers that travelled here was the snap, and the snap is gone.
	seeOther(w, r, qrReturn(next, id, "styled", slug, nil))
}

// requestedQRSize reads the size control, which is two inputs and a witness
// since the second M49 reopening (D182).
//
// **A slider and a number for one value, and no script to keep them in step.**
// The dashboard runs on htmx and nothing else, so nothing in the browser can
// copy a dragged slider into the number beside it — and the milestone's own risk
// names the failure that invites: the number becoming a second source of truth
// for the same setting. The rule here is what stops it, and it is decided on the
// server where it can be tested:
//
//	`size_shown` is the value the form was rendered with.
//	The slider wins when it has moved off that; otherwise the number does.
//
// So dragging the slider and pressing save uses the slider, typing in the box
// and pressing save uses the box, and moving both uses the slider — one rule,
// stated, rather than whichever input the browser happened to serialise first.
//
// A form with no slider at all — an API-shaped post, or a page cached from
// before this reopening — is the number alone, which is what M49 shipped.
//
// The fallback for an empty or unreadable box is -1 and not a default: a size
// box submitted empty is somebody who cleared it, not somebody asking for the
// default, and -1 is refused downstream with a sentence naming the range.
func requestedQRSize(r *http.Request) int {
	typed := formInt(r.PostFormValue("size"), -1)
	raw := r.PostFormValue("size_slider")
	if raw == "" {
		return typed
	}
	slider := formInt(raw, -1)
	if shown := formInt(r.PostFormValue("size_shown"), -1); slider != shown {
		return slider
	}
	return typed
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
// The default code is reachable here exactly as a named one is. Its `code` value
// is whatever slug the panel is open on — the empty string only on a link whose
// single code has none — and the service resolves the default from the flag
// rather than from that emptiness (D183). The owner's ruling of 2026-08-07 is
// what made the default addressable for a logo at all.
//
// **The form submits itself the moment a file is chosen** (F214c), through an
// htmx `change` trigger on the form and no script of this product's own. Nothing
// about this handler changes for it: htmx sends the same multipart body, and
// `seeOther` already answers an htmx request with `HX-Redirect`, which is a full
// page load rather than a swap. What *did* change is the refusal — see
// finishQRAction, which cannot render a 422 into an htmx swap because htmx does
// not swap one.
func (h *Web) LinkQRLogo(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	// Read before the refusal is routed, and read from whatever the reader got
	// through: readUploadedFile walks every part and returns what it collected
	// alongside its error, so a body that failed after the two hidden inputs
	// still says which surface it came from. A body that failed before them
	// falls back to the link page, which is where this path always landed.
	upload, err := readUploadedFile(w, r, "logo")
	next, slug := upload.Fields.Get("next"), upload.Fields.Get("code")
	if err != nil {
		h.finishQRAction(w, r, id, next, slug, err)
		return
	}
	_, fit, serr := h.Links.SetQRCodeLogo(
		r.Context(), IdentityFrom(r.Context()), id, slug, upload.File)
	if serr != nil {
		h.finishQRAction(w, r, id, next, slug, serr)
		return
	}

	// Only when the image was shrunk, on the same terms the size control reports
	// a snap: a sentence on every upload saying it stored what you sent is a
	// sentence nobody reads by the third time. Both sizes travel, because the
	// page it lands on is a fresh request that never saw the file.
	var extra url.Values
	if fit.Resampled() {
		extra = url.Values{
			"from": {dims(fit.SourceWidth, fit.SourceHeight)},
			"to":   {dims(fit.Width, fit.Height)},
		}
	}
	seeOther(w, r, qrReturn(next, id, "logo", slug, extra))
}

// dims writes a WxH pair for the query string, and dimsParam reads one back.
//
// Re-derived rather than echoed, on the reason the size control's own pair used
// to be: these arrive in a URL anybody can edit, and a sentence assembled from
// a query string is a sentence somebody else can write. Anything that is not two
// integers inside the bounds a stored logo actually has says nothing, which
// leaves the ordinary "logo stored" message.
func dims(w, h int) string { return strconv.Itoa(w) + "x" + strconv.Itoa(h) }

func dimsParam(raw string) (int, int, bool) {
	wRaw, hRaw, ok := strings.Cut(raw, "x")
	if !ok {
		return 0, 0, false
	}
	w, werr := strconv.Atoi(wRaw)
	h, herr := strconv.Atoi(hRaw)
	if werr != nil || herr != nil || w < 1 || h < 1 ||
		w > qr.MaxLogoDimension || h > qr.MaxLogoDimension {
		return 0, 0, false
	}
	return w, h, true
}

// finishQRAction puts a refusal back on the page it was made from, with the
// reason beside the code — the same trade finishFolderAction makes, and for the
// same reason: every refusal here is something the reader can fix in the form
// they are looking at.
//
// **"The page it was made from" is two pages, and `next` is which** (F170,
// D174). The panel is a route since M48, so the same form is submitted from the
// link page and from `/links/{id}/qr`; the success path has always honoured the
// field through qrReturn and this one rendered the link page whatever the form
// said, which answered somebody working in the panel with a different page and
// left them to find their way back. The refusal itself is unchanged — 422, the
// message beside the form, nothing stored.
//
// `next` is **matched, never followed**, on exactly qrReturn's terms: it is
// compared against a path this function builds from the id it was given, so the
// field chooses between two surfaces rather than naming one.
//
// **The status is 200 for an htmx request, and that is not a fudge** (F214c).
// htmx's default `responseHandling` does not swap a 4xx at all — the response is
// read, an error event fires, and the page is left exactly as it was. Every
// other form in this panel posts natively and never meets that rule; the logo
// form submits itself the moment a file is chosen, which makes it the first one
// that does, and a refusal nobody can see is worse than the two-step it
// replaced. This is the shape renderAutomation and renderDomains already use
// for the same reason — an htmx request gets what it can render, and the code
// that says "this was refused" is the message in the page rather than the
// status line the swap discarded.
func (h *Web) finishQRAction(
	w http.ResponseWriter, r *http.Request, id uuid.UUID, next, slug string, err error,
) {
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		h.webError(w, r, err)
		return
	}
	status := http.StatusUnprocessableEntity
	if isHTMX(r) {
		status = http.StatusOK
	}

	if self := "/links/" + id.String() + "/qr"; next == self {
		actor := IdentityFrom(r.Context())
		l, lerr := h.Links.Get(r.Context(), actor, id)
		if lerr != nil {
			h.webError(w, r, lerr)
			return
		}
		// The same assembly LinkQRPage makes, on the code the form was editing:
		// a refusal that dropped the reader back onto the default code would
		// lose the selection along with the save. No notice — the ?qr= markers
		// are what a *redirect* carries, and nothing here redirects.
		view := h.linkQR(r.Context(), actor, l, self, slug)
		view.QRError = ve[0].Message
		h.render(w, r, status, "link_qr", linkQRPageData{
			shell:      h.shell(r, "QR code · /"+l.Alias, "links"),
			Link:       l,
			linkQRView: view,
		})
		return
	}

	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	// The QR tab, because the refusal renders in the QR section and a POST
	// carries no ?tab= — qrReturn's re-derivation, on the 422 path (D178).
	data.Tab = "qr"
	data.QRError = ve[0].Message
	h.render(w, r, status, "link_detail", data)
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
// **The snap had a sentence here and it is gone** (D182). M49 required the size
// actually produced to appear beside the one that was asked for, because the two
// could differ; the second reopening made them the same number at every value,
// so the pair no longer travels in the query and there is nothing to spend it
// on. A message explaining that 300px came out as 300px is the kind of sentence
// that teaches readers to stop reading them.
func qrNotice(q url.Values) string {
	switch q.Get("qr") {
	case "styled":
		return "QR code restyled. The code says the same thing — a style changes how it " +
			"is drawn, never what it encodes."
	case "reset":
		// It says defaults rather than colours because the button does: M49 put
		// a size on this form and reset clears that too, so the old sentence
		// named a third of what happened (F213).
		//
		// *This* code since D183, because the button is scoped to the selection
		// now. It used to clear the default code's style whichever one you were
		// looking at, and the sentence said nothing about which — which was
		// exactly why the defect went unreported until the owner met it.
		return "This code restored to the default style — black on white, at the default " +
			"size. Any logo on it is left alone; removing one is its own control."
	case "added":
		return "A second code, with a name and an identity of its own. It points at the " +
			"same destination — what it changes is that a scan of this one is told apart " +
			"from a scan of the others."
	case "removed":
		return "Code removed. What it already recorded stays in the breakdown; anything " +
			"printed with it is counted as the link's default code from now on."
	case "promoted":
		// The code that was removed held the flag, so something had to take it.
		// Said in as many words rather than left to be discovered: every picture
		// of this link that carries no code parameter — which is every picture
		// printed before codes had identities — now lands on a different row
		// (D183). The slug is named because it is what is printed; the label is
		// not, because it arrived in a URL.
		slug := q.Get("promoted")
		if !domain.ValidQRCodeSlug(slug) {
			return "Code removed, and it was the default — so another code is the default " +
				"now. Scans from pictures that carry no code of their own are counted " +
				"against whichever code holds that role."
		}
		return "Code removed. It was this link's default, so the oldest code left — " +
			slug + " — is the default now: scans from any picture that carries no code " +
			"of its own are counted against it from here on, including every picture " +
			"printed before codes had identities. What the removed code already " +
			"recorded stays in the breakdown."
	case "defaulted":
		return "Default code set. A scan that carries no code of its own — which is what " +
			"every picture printed before codes had identities carries — is counted " +
			"against this one from now on. Nothing already printed changed what it says."
	case "renamed":
		return "Code renamed. The name is what the dashboard calls it — what the code " +
			"says is unchanged, so nothing already printed is affected."
	case "logo":
		// M50.5's version of this said nothing drew the logo yet, because nothing
		// did. M50.6 is the milestone that made that sentence false, so it is
		// M50.6's to replace — and what it replaces it with is the two things that
		// changed about the picture, because both are visible and neither was
		// asked for directly.
		const stored = "Logo stored, re-encoded as a PNG by this server, and drawn in " +
			"the middle of the code. Error correction is now at level H, which is what " +
			"makes a code readable with part of it covered — so the picture is a little " +
			"denser than it was. What the code says is unchanged."
		// The third thing that can have changed, and only when it did (F214a).
		// Silently shrinking somebody's artwork and reporting an unqualified
		// success is the shape this reopening was called for.
		fw, fh, fok := dimsParam(q.Get("from"))
		tw, th, tok := dimsParam(q.Get("to"))
		if !fok || !tok || (fw == tw && fh == th) {
			return stored
		}
		return fmt.Sprintf("%s It was resized on the way in: you uploaded %d×%d and what "+
			"is stored is %d×%d, because a stored logo holds at most %d pixels in total. "+
			"The shape is unchanged and the code draws it at a fraction of its own size, "+
			"so this is not detail you will see.", stored, fw, fh, tw, th, qr.MaxLogoPixels)
	case "logo_removed":
		return "Logo removed. The image is gone from the row rather than merely " +
			"unreferenced, so nothing is left behind. The code stays at error " +
			"correction level H: dropping it back would redraw a picture that may " +
			"already be printed, and H is the safer of the two to be left at."
	default:
		return ""
	}
}
