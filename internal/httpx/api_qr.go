package httpx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// QR codes over the API (M41, M49).
//
// **Five operations on one subresource, and two of them answer with an image.**
// `GET /links/{id}/qr` returns the code as JSON — what it encodes and how it is
// drawn — and `GET /links/{id}/qr.svg` and `GET /links/{id}/qr.png` return the
// picture. Paths rather than content negotiation on `Accept`, because a QR code
// is something somebody puts in an `<img src>` or downloads, and both of those
// send an `Accept` header nobody chose. The extension is also the whole of what
// a person has to know to get the other format, which an `Accept` header is not.
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

// PNGDisposition is what the PNG response asks a browser to do with the bytes.
//
// **A filename that is not the link's alias, and that is deliberate.** The
// dashboard's anchor carries `download="qr-<alias>.png"`, which is where a
// workspace-controlled string belongs: in markup the template engine escapes.
// Putting it in a response header instead would be the one place in this product
// where workspace data is written into an HTTP header, for a filename the
// dashboard already overrides — so the header states the constant and the
// browser falls back to it only for somebody fetching the URL directly, who
// asked for `qr.png` and gets `qr.png`.
const PNGDisposition = `attachment; filename="qr.png"`

// GetQRPNG serves the picture as a raster image (M49).
//
// **This is the endpoint D11 said would never exist, and the reversal is in
// m49.md and in internal/qr's package comment.** D11's premise was that nothing
// should rasterise on a request; this rasterises only when somebody asks for a
// file, and internal/qr bounds what that can allocate. A refusal for a code
// larger than that bound is a 422 — the size is something the reader can change.
func (a *LinkAPI) GetQRPNG(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Links.RenderQRPNG(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", qr.PNGContentType)
	// The same policy the SVG carries, for the same reason: it is a workspace's
	// own data behind an authenticated request, so a shared cache must not keep
	// it, and the two formats of one picture must not expire differently.
	w.Header().Set("Cache-Control", SVGMaxAge)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", PNGDisposition)
	_, _ = w.Write(out) //nolint:gosec // G705: a paletted bitmap of two parsed colours
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

// DeleteQR returns the link's default code to the default style. Not "delete the
// QR code" — there is no such thing to delete, because every link has one.
// Removing one of the *named* codes M50 added is DeleteQRCode below.
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

// More than one code per link (M50).
//
// **A collection under the link, whose members are keyed by their printed
// slug.** A code's slug is what a person has in hand — it is in the payload of
// the picture in front of them — so it is what the path is written in, and the
// row's uuid is reported for completeness rather than used to address anything.
//
// The five operations above are untouched and now name the link's *default*
// code: the one whose payload carries no code parameter, which is every picture
// this product drew before this milestone. That choice is recorded in
// decisions.md under M50, because silently changing what a shipped endpoint
// answers for is the thing the contract test exists to catch.
//
// **The empty slug is not addressable here.** `/qr/codes/` is not a route, and
// the default code is reached through `/qr` — one identity, one path. What
// `/qr/codes` does list is every code including the default, because a client
// asking what codes a link has wants the answer to include the one it started
// with.

// ListQRCodes answers with every code a link carries, default first.
func (a *LinkAPI) ListQRCodes(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	codes, err := a.Links.ListQRCodes(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"codes": codes,
		"max":   domain.MaxQRCodesPerLink,
	})
}

type qrCodeRequest struct {
	Label string   `json:"label"`
	Style qr.Style `json:"style"`
}

// CreateQRCode adds a named code to a link.
//
// The slug is not in the request and cannot be. It is generated, because it is
// printed: a caller-chosen one would be a name the caller has to keep unique
// across the link's codes and correct across every copy already in the world,
// and the failure mode of getting it wrong is a poster attributing to somebody
// else's campaign.
func (a *LinkAPI) CreateQRCode(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req qrCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.CreateQRCode(r.Context(), IdentityFrom(r.Context()), id, req.Label)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"code": code, "levels": qr.Levels})
}

// GetQRCode answers with one named code.
func (a *LinkAPI) GetQRCode(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.QRCodeBySlug(r.Context(), IdentityFrom(r.Context()), id, r.PathValue("slug"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"code": code, "levels": qr.Levels})
}

// SetQRCode replaces a named code's label and style.
//
// PUT rather than PATCH, for the reason SetQR is a PUT: an omitted style field
// means its default, which is what makes "back to plain black on white" an empty
// object rather than five explicit fields. The label follows the same rule —
// omitting it clears the name — because a request that replaces a resource
// whole and quietly preserves one field is the shape nobody can predict.
func (a *LinkAPI) SetQRCode(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	slug := r.PathValue("slug")
	var req qrCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	actor := IdentityFrom(r.Context())
	if _, err := a.Links.SetQRCodeLabel(r.Context(), actor, id, slug, req.Label); err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.SetQRStyleBySlug(r.Context(), actor, id, slug, req.Style)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"code": code, "levels": qr.Levels})
}

// DeleteQRCode removes a named code.
//
// The default code is not reachable here — it has no slug to name it with — and
// that is the whole of why it cannot be deleted by accident: the operation that
// looks like deleting it is `DELETE /links/{id}/qr`, which resets its style.
func (a *LinkAPI) DeleteQRCode(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteQRCode(
		r.Context(), IdentityFrom(r.Context()), id, r.PathValue("slug"),
	); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetQRCodeSVG serves a named code's picture.
func (a *LinkAPI) GetQRCodeSVG(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	svg, err := a.Links.RenderQRBySlug(r.Context(), IdentityFrom(r.Context()), id, r.PathValue("slug"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeQRImage(w, qr.ContentType, "", svg)
}

// GetQRCodePNG serves a named code's picture as a raster image.
func (a *LinkAPI) GetQRCodePNG(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Links.RenderQRPNGBySlug(r.Context(), IdentityFrom(r.Context()), id, r.PathValue("slug"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeQRImage(w, qr.PNGContentType, PNGDisposition, out)
}

// A logo on a code (M50.5).
//
// **This is the first surface in this product that accepts a file**, and every
// choice in it is *do less*. Two operations — replace the image, remove it. No
// download endpoint: nothing in this milestone serves a stored logo back, and
// what one is *for* is M50.6, which composites it into the pictures the `.svg`
// and `.png` paths already serve.
//
// **`links.update`, no new permission, and no session gate.** D75 already
// decided that styling a code is styling the link, and a logo is style; D87's
// limb refuses operations whose subject is the person, and the subject here is
// the link, so a key that may recolour a code may put an image on it.
//
// **Two capabilities, four routes, and that is the shape D133 already ships
// five times.** The owner overruled D136 on 2026-08-07: a link where nobody
// added a second code — nearly every link — could not carry a logo at all,
// because the default code's identity is the *absence* of a slug (D130) and the
// collection addresses codes by one. So the pair answers at `/links/{id}/qr/logo`
// as well as at `/links/{id}/qr/codes/{slug}/logo`, the same way `qr.png` and
// `codes/{slug}/image.png` are one capability at two addresses. The default code
// gains no reserved slug — that would reopen D130 and change what an
// already-printed picture counts as.
//
// The shorthand answers with `qr`, as every other `/links/{id}/qr` operation
// does, and the collection answers with `code`. Two shapes because there are two
// families, not because the operation differs.

// SetQRLogo replaces the image stored against a link's default code.
//
// PUT rather than POST, for the reason SetQR is a PUT: the request replaces the
// resource whole, and sending it twice leaves the same state as sending it once.
func (a *LinkAPI) SetQRLogo(w http.ResponseWriter, r *http.Request) {
	a.setQRLogo(w, r, "", "qr")
}

// SetQRCodeLogo replaces the image stored against one of a link's named codes.
func (a *LinkAPI) SetQRCodeLogo(w http.ResponseWriter, r *http.Request) {
	a.setQRLogo(w, r, r.PathValue("slug"), "code")
}

// setQRLogo is the upload both routes perform. One function, because the only
// difference between them is which code is addressed and which key the answer
// is written under — and a second copy of the read-cap-store sequence is a
// second place for the ordering the milestone is about to come apart.
func (a *LinkAPI) setQRLogo(w http.ResponseWriter, r *http.Request, slug, key string) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	upload, err := readUploadedFile(w, r, "logo")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	code, err := a.Links.SetQRCodeLogo(r.Context(), IdentityFrom(r.Context()), id, slug, upload.File)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{key: code, "levels": qr.Levels})
}

// DeleteQRLogo removes the image from a link's default code, leaving the code.
func (a *LinkAPI) DeleteQRLogo(w http.ResponseWriter, r *http.Request) {
	a.clearQRLogo(w, r, "")
}

// DeleteQRCodeLogo removes the image from one named code, leaving the code.
func (a *LinkAPI) DeleteQRCodeLogo(w http.ResponseWriter, r *http.Request) {
	a.clearQRLogo(w, r, r.PathValue("slug"))
}

func (a *LinkAPI) clearQRLogo(w http.ResponseWriter, r *http.Request, slug string) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.ClearQRCodeLogo(
		r.Context(), IdentityFrom(r.Context()), id, slug,
	); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadedForm is a multipart body read into memory: the one part this product
// treats as a file, and the short text parts beside it.
//
// **Fields exists for the dashboard and is empty for the API.** A browser form
// that uploads a file also carries the two hidden inputs every other form in the
// panel carries — where the save returns to, and which code it is for — and a
// multipart body is the only body it can send them in. The API's own callers
// send the file alone, and nothing reads Fields for them.
type uploadedForm struct {
	File   []byte
	Fields url.Values
}

// maxUploadFieldBytes bounds one text part of a multipart upload.
//
// The whole body is already capped by MaxBytesReader, so this is not what stops
// a large request; it is what stops a *file* arriving under a text field's name
// and being held twice. Two kilobytes is far above the longest value any form
// here sends — a return path and an eight-character slug.
const maxUploadFieldBytes = 2048

// readUploadedFile reads one named part out of a multipart body.
//
// **`MultipartReader`, never `ParseMultipartForm`.** The convenient one spills
// anything past its memory budget into `os.TempDir()`, which would have this
// product writing a stranger's bytes to the filesystem — the exact thing the
// storage decision (D134) chose a column to avoid, arriving through the back
// door of a helper function. The streaming reader holds the part in memory,
// under the same cap as the request.
//
// **The cap is on the request body and it is the first thing that happens**, so
// it applies to the whole envelope — part headers, boundaries and all — and not
// only to whatever the sender labels as the file. That ordering is why a body
// that never ends is refused rather than read: `MaxBytesReader` fails the read
// itself, so the multipart parser cannot be starved into holding a connection
// open while it waits for a boundary.
//
// **Neither the filename nor the part's declared `Content-Type` is read**, and
// that is asserted rather than promised — see
// TestNothingAboutAnUploadsNameOrDeclaredTypeIsRead. Both are strings the sender
// chose: one would be a path component if anything ever treated it as one, and
// the other would be a way to pick this product's decoder from outside it. What
// decides the decoder is the content, in internal/qr.
//
// **Every part is walked, rather than stopping at the file.** A browser puts its
// hidden inputs wherever the markup does, so a reader that returned at the file
// part would make the two fields the dashboard needs depend on where a template
// happens to place them. Walking to the end costs nothing that is not already
// bounded: the envelope is capped, and each text part is capped again at
// maxUploadFieldBytes.
func readUploadedFile(w http.ResponseWriter, r *http.Request, field string) (uploadedForm, error) {
	out := uploadedForm{Fields: url.Values{}}
	tooLarge := domain.ValidationErrors{{
		Field: field, Code: "too_large",
		Message: fmt.Sprintf("an upload is at most %d bytes", qr.MaxLogoUploadBytes),
	}}
	notMultipart := domain.ValidationErrors{{
		Field: field, Code: "invalid",
		Message: "this endpoint takes a multipart/form-data body carrying a " +
			field + " part",
	}}

	r.Body = http.MaxBytesReader(w, r.Body, qr.MaxLogoUploadBytes)
	parts, err := r.MultipartReader()
	if err != nil {
		return out, notMultipart
	}
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if AsError(err, &maxErr) {
				return out, tooLarge
			}
			return out, notMultipart
		}
		// FormName is the *field* name, which this handler chose and the sender
		// merely repeats. It is not the filename, and nothing here asks for one.
		name := part.FormName()
		// A text part is read one byte past its bound, so passing it is
		// detectable rather than silently truncated — a return path cut in half
		// would send the reader somewhere nobody asked for and report success.
		limit := int64(maxUploadFieldBytes) + 1
		if name == field {
			limit = qr.MaxLogoUploadBytes
		}
		body, err := io.ReadAll(io.LimitReader(part, limit))
		_ = part.Close()
		if err != nil {
			var maxErr *http.MaxBytesError
			if AsError(err, &maxErr) {
				return out, tooLarge
			}
			return out, notMultipart
		}
		if name != field {
			if len(body) > maxUploadFieldBytes {
				return out, domain.ValidationErrors{{
					Field: name, Code: "too_large",
					Message: fmt.Sprintf("a form field beside an upload is at most %d bytes",
						maxUploadFieldBytes),
				}}
			}
			out.Fields.Set(name, string(body))
			continue
		}
		if len(body) == 0 {
			return out, domain.ValidationErrors{{
				Field: field, Code: "empty", Message: "the " + field + " part carries no bytes",
			}}
		}
		out.File = body
	}
	if out.File == nil {
		return out, domain.ValidationErrors{{
			Field: field, Code: "required",
			Message: "the body carries no " + field + " part",
		}}
	}
	return out, nil
}

// writeQRImage is the headers every picture response here shares.
//
// One function rather than four copies, because the properties are the same
// four every time and a copy that drifts is a response served without one of
// them: the cache policy that keeps a workspace's own picture out of a shared
// cache, the refusal to sniff, and — for the raster — the disposition that names
// a constant rather than workspace text.
func writeQRImage(w http.ResponseWriter, contentType, disposition string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", SVGMaxAge)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	// The bytes are generated from integers and validated colours — see
	// internal/qr's package comment and TestNothingButAColourReachesTheDrawing.
	_, _ = w.Write(body) //nolint:gosec // G705: the drawing holds integers and parsed colours only
}
