package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// QR codes over the API (M41).
//
// **Four operations on one subresource, and only one of them answers with an
// image.** `GET /links/{id}/qr` returns the code as JSON — what it encodes and
// how it is drawn — and `GET /links/{id}/qr.svg` returns the picture. Two paths
// rather than content negotiation on `Accept`, because a QR code is something
// somebody puts in an `<img src>` or downloads, and both of those send an
// `Accept` header nobody chose.
//
// **No permission of its own.** A QR code is a picture of the link's own short
// URL, so seeing one is `links.read` and styling one is `links.update` — see
// internal/link/qr.go and decisions.md, D75.
//
// The SVG response is the one non-JSON body this API has besides the spec
// document itself, which is why the contract test validates it by hand: the
// kin-openapi filter has no decoder for it, exactly as it has none for YAML.

// SVGMaxAge is how long a rendered code may be cached, in seconds.
//
// Private, and short. A QR code changes when its link's alias changes or when
// somebody restyles it, and neither is frequent — but it is derived from a
// workspace's own data behind an authenticated request, so a shared cache must
// not keep it. Five minutes is enough for a dashboard page that renders the code
// twice, inline and in a download link.
const SVGMaxAge = "private, max-age=300"

func (a *LinkAPI) GetQR(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.QRCode(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The levels travel with the answer, the way ListFolders advertises the
	// depth cap: a client building a style form learns the vocabulary from the
	// response rather than from this file.
	WriteJSON(w, http.StatusOK, map[string]any{
		"qr":     code,
		"levels": qr.Levels,
	})
}

// GetQRSVG serves the picture.
//
// Written by hand rather than through WriteJSON, on the same shape docs.SpecYAML
// uses: set the content type, set the cache policy, write the bytes.
func (a *LinkAPI) GetQRSVG(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	svg, err := a.Links.RenderQR(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", qr.ContentType)
	w.Header().Set("Cache-Control", SVGMaxAge)
	// The bytes are generated from integers and validated colours and carry no
	// script, but a response served from this origin that a browser will parse
	// as a document gets the same refusal to sniff as everything else.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// gosec's taint analysis follows the link id from the request into the
	// response and cannot see what internal/qr does in between. The renderer
	// never writes the content into the document — a QR code carries no title,
	// no aria-label and no metadata — and the only style values that reach the
	// output have been parsed as `#rgb`/`#rrggbb`. See the package comment
	// there, and TestNothingButAColourReachesTheDrawing.
	_, _ = w.Write(svg) //nolint:gosec // G705: the drawing holds integers and parsed colours only
}

type setQRRequest struct {
	Style qr.Style `json:"style"`
}

// SetQR stores the style. A PUT rather than a PATCH: the style is replaced
// whole, and an omitted field means its default rather than "leave it alone" —
// which is what makes "put it back to plain black on white" expressible as an
// empty object rather than as five explicit fields.
func (a *LinkAPI) SetQR(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req setQRRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.SetQRStyle(r.Context(), IdentityFrom(r.Context()), id, req.Style)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"qr":     code,
		"levels": qr.Levels,
	})
}

// DeleteQR returns the link's code to the default style. Not "delete the QR
// code" — there is no such thing to delete, because every link has one.
func (a *LinkAPI) DeleteQR(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.ResetQRStyle(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
