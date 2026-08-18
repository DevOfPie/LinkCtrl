//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/qr"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The first file this product accepts, end to end (M50.5).
//
// **What is asserted here and nowhere else is what happens to the bytes.**
// internal/qr proves the caps bind and the re-encode strips a passenger;
// internal/httpx proves the request cap applies before anything is read and that
// nothing the sender names is consulted. Those are properties of functions.
// These are properties of the database: that the row holds what the product
// produced rather than what arrived, that replacing leaves nothing behind, that
// removing the code takes the image with it, and that the one deletion the
// cascade does not reach is collected by the sweep.
//
// Every assertion reads the column directly rather than through the API, because
// the API deliberately never returns the bytes — `has_logo` is all it reports,
// and a test that could only see a boolean could not tell "stored the upload"
// from "stored what we encoded".

// --- fixtures ---------------------------------------------------------------

// logoPNG draws a small distinguishable image. `mark` changes the pixels, so two
// calls with different marks produce two images a test can tell apart.
func logoPNG(t *testing.T, side int, mark uint8) []byte {
	t.Helper()
	return logoPNGSize(t, side, side, mark)
}

// logoPNGSize is logoPNG for a rectangle, which a square cannot stand in for:
// the refusal sentence says *wide* or *tall*, and a square makes the two
// indistinguishable.
func logoPNGSize(t *testing.T, w, h int, mark uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{
				R: mark, G: uint8(x), B: uint8(y), A: 0xff,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// logoBody builds a multipart body: the file part, and any text fields beside
// it.
//
// `fields` exists for the dashboard's form, which carries `next` and `code` in
// the only body a file upload has. Nothing the API sends uses it.
func logoBody(
	t *testing.T, filename, declared string, body []byte, fields map[string]string,
) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="logo"; filename="` + filename + `"`},
		"Content-Type":        {declared},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// logoPath is where one code's image is addressed.
//
// **Two shapes, one capability.** A named code is a member of the collection;
// a link's only code has no slug — nothing to be told apart from, and nothing to
// name it with — so it is reached through the `/qr` shorthand D133 kept, exactly as `qr.svg`
// and `qr.png` reach it. The owner's ruling of 2026-08-07 is what put a logo
// there; before it the empty slug had no path at all.
func logoPath(linkID uuid.UUID, slug string) string {
	if slug == "" {
		return "/api/v1/links/" + linkID.String() + "/qr/logo"
	}
	return "/api/v1/links/" + linkID.String() + "/qr/codes/" + slug + "/logo"
}

// uploadLogo PUTs a multipart body at a code's logo path and returns the status.
//
// The filename and the declared content type are arguments because the server
// must ignore both, and every caller here sends values that disagree with the
// content on purpose.
func (f *ruleFixture) uploadLogo(
	t *testing.T, linkID uuid.UUID, slug, filename, declared string, body []byte,
) (int, []byte) {
	t.Helper()
	return f.uploadLogoAs(t, "", linkID, slug, filename, declared, body)
}

// uploadLogoAs is uploadLogo with a credential: an empty bearer sends the
// session, and a token sends the key instead — which wins over the cookie the
// fixture's jar is still carrying.
func (f *ruleFixture) uploadLogoAs(
	t *testing.T, bearer string, linkID uuid.UUID, slug, filename, declared string, body []byte,
) (int, []byte) {
	t.Helper()
	contentType, payload := logoBody(t, filename, declared, body, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		f.server.URL+logoPath(linkID, slug), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// storedLogo reads the column, which is the only place the bytes exist.
func (f *ruleFixture) storedLogo(t *testing.T, linkID uuid.UUID, slug string) []byte {
	t.Helper()
	var out []byte
	err := f.pool.QueryRow(t.Context(),
		`SELECT logo FROM qr_codes WHERE link_id = $1 AND slug = $2`, linkID, slug).Scan(&out)
	if err != nil {
		t.Fatalf("read the stored logo for %s/%q: %v", linkID, slug, err)
	}
	return out
}

// logoRowCount is how many rows anywhere hold bytes, which is what "nothing left
// behind" is a claim about.
func (f *ruleFixture) logoRowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM qr_codes WHERE logo IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- what is stored -----------------------------------------------------------

// TestWhatLandsInTheColumnIsWhatThisProductEncoded is the storage claim against
// the database: the row holds a PNG this server produced, not the file that was
// sent, and the upload's own name and declared type reached nothing.
func TestWhatLandsInTheColumnIsWhatThisProductEncoded(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logostore", "https://example.com/x")
	code := f.createQRCode(t, id, "Autumn poster")

	// A JPEG, sent under a `.png` filename and an `image/svg+xml` declaration —
	// three disagreeing statements about the same bytes, and the content is what
	// decides.
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 4), G: 0x40, B: uint8(y * 4), A: 0xff})
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, src, nil); err != nil {
		t.Fatal(err)
	}
	status, body := f.uploadLogo(t, id, code.Slug, "logo.png", "image/svg+xml", jpg.Bytes())
	if status != http.StatusOK {
		t.Fatalf("uploading a JPEG answered %d: %s", status, body)
	}

	stored := f.storedLogo(t, id, code.Slug)
	if len(stored) == 0 {
		t.Fatal("nothing was stored")
	}
	if bytes.Equal(stored, jpg.Bytes()) {
		t.Error("the column holds the bytes that were uploaded; what is stored is " +
			"supposed to be a PNG this product encoded")
	}
	img, format, err := image.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the stored bytes do not decode: %v", err)
	}
	if format != "png" {
		t.Errorf("the column holds a %s; every stored logo is re-encoded to PNG", format)
	}
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Errorf("the stored image is %dx%d, want 64x64", b.Dx(), b.Dy())
	}
	if len(stored) > qr.MaxLogoStoredBytes {
		t.Errorf("the stored image is %d bytes, past the %d the row is bounded at",
			len(stored), qr.MaxLogoStoredBytes)
	}

	// And an SVG at the same endpoint, refused rather than stored — with the
	// previous logo left exactly as it was, because a refusal must not be a way
	// to delete something.
	status, _ = f.uploadLogo(t, id, code.Slug, "logo.png", "image/png",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x()</script></svg>`))
	if status != http.StatusUnprocessableEntity {
		t.Errorf("an SVG upload answered %d, want 422", status)
	}
	if !bytes.Equal(f.storedLogo(t, id, code.Slug), stored) {
		t.Error("a refused upload changed what was stored")
	}
}

// TestReplacingALogoLeavesNothingBehind is one of the milestone's four removal
// claims, and the one a filesystem or an object store would have needed code
// for. Under a column it is a single UPDATE, and this is what says so.
func TestReplacingALogoLeavesNothingBehind(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logoreplace", "https://example.com/x")
	code := f.createQRCode(t, id, "Poster")

	first := logoPNG(t, 48, 0x11)
	if status, body := f.uploadLogo(t, id, code.Slug, "a.png", "image/png", first); status != http.StatusOK {
		t.Fatalf("first upload answered %d: %s", status, body)
	}
	stored := f.storedLogo(t, id, code.Slug)

	second := logoPNG(t, 48, 0xee)
	if status, body := f.uploadLogo(t, id, code.Slug, "b.png", "image/png", second); status != http.StatusOK {
		t.Fatalf("second upload answered %d: %s", status, body)
	}
	replaced := f.storedLogo(t, id, code.Slug)

	if bytes.Equal(replaced, stored) {
		t.Error("the second upload did not replace the first")
	}
	if n := f.logoRowCount(t); n != 1 {
		t.Errorf("%d rows hold a logo after two uploads to one code, want 1", n)
	}
	// The pixels are the second image's, which is the check that "replaced"
	// means replaced rather than "a second row somewhere".
	img, err := png.Decode(bytes.NewReader(replaced))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := img.At(0, 0).RGBA(); uint8(r>>8) != 0xee {
		t.Errorf("the stored image's first pixel is %02x, want the second upload's ee", r>>8)
	}
}

// TestRemovingALogoRemovesTheArtefact covers the clear operation and the two
// container deletions a cascade reaches.
func TestRemovingALogoRemovesTheArtefact(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logoclear", "https://example.com/x")

	// Clearing.
	cleared := f.createQRCode(t, id, "Cleared")
	if status, body := f.uploadLogo(t, id, cleared.Slug, "a.png", "image/png",
		logoPNG(t, 32, 0x22)); status != http.StatusOK {
		t.Fatalf("upload answered %d: %s", status, body)
	}
	path := "/api/v1/links/" + id.String() + "/qr/codes/" + cleared.Slug + "/logo"
	if got := f.deleteAPI(path); got != http.StatusNoContent {
		t.Fatalf("clearing answered %d, want 204", got)
	}
	if got := f.storedLogo(t, id, cleared.Slug); got != nil {
		t.Errorf("clearing left %d bytes in the column; the artefact goes, not just "+
			"the reference", len(got))
	}
	// Idempotent, which is what makes the sweep and a retried client both safe.
	if got := f.deleteAPI(path); got != http.StatusNoContent {
		t.Errorf("clearing a second time answered %d, want 204", got)
	}

	// Deleting the code. No statement removes the bytes — the row goes and they
	// go with it, which is the whole of what D134 bought.
	removed := f.createQRCode(t, id, "Removed")
	if status, body := f.uploadLogo(t, id, removed.Slug, "a.png", "image/png",
		logoPNG(t, 32, 0x33)); status != http.StatusOK {
		t.Fatalf("upload answered %d: %s", status, body)
	}
	if got := f.deleteAPI(
		"/api/v1/links/" + id.String() + "/qr/codes/" + removed.Slug,
	); got != http.StatusNoContent {
		t.Fatalf("deleting the code answered %d, want 204", got)
	}
	if n := f.logoRowCount(t); n != 0 {
		t.Errorf("%d rows still hold a logo after the only code carrying one was "+
			"deleted", n)
	}

	// Deleting the workspace, which is a hard delete and cascades through
	// `links` to `qr_codes`. Driven at the database rather than through the team
	// service, because what is being asserted is the schema's own behaviour.
	survivor := f.createQRCode(t, id, "Survivor")
	if status, body := f.uploadLogo(t, id, survivor.Slug, "a.png", "image/png",
		logoPNG(t, 32, 0x44)); status != http.StatusOK {
		t.Fatalf("upload answered %d: %s", status, body)
	}
	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM workspaces WHERE id = $1`, f.owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if n := f.logoRowCount(t); n != 0 {
		t.Errorf("%d rows still hold a logo after their workspace was deleted; the "+
			"cascade is what makes container deletion free", n)
	}
}

// TestTheOrphanSweepClearsLogosFromDeletedLinks is the one removal path a
// cascade does not reach, and therefore the reason the sweep exists at all under
// a column.
//
// A link is soft-deleted with a purge deadline so its alias stays reserved and it
// can be brought back by hand. Its `qr_codes` rows survive that window holding
// up to a megabyte each, and every read filters them out — so the bytes are
// unreachable through the product and the endpoint that would clear them answers
// 404. The hourly pass is what makes *deleting the link removes its artefacts*
// true rather than merely intended.
func TestTheOrphanSweepClearsLogosFromDeletedLinks(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logoorphan", "https://example.com/x")
	code := f.createQRCode(t, id, "Orphan")
	if status, body := f.uploadLogo(t, id, code.Slug, "a.png", "image/png",
		logoPNG(t, 32, 0x55)); status != http.StatusOK {
		t.Fatalf("upload answered %d: %s", status, body)
	}

	// A second link whose logo must survive, so the sweep is shown to be
	// selective rather than a table-wide clear.
	keep := f.createLink("logokeep", "https://example.com/y")
	kept := f.createQRCode(t, keep, "Kept")
	if status, body := f.uploadLogo(t, keep, kept.Slug, "a.png", "image/png",
		logoPNG(t, 32, 0x66)); status != http.StatusOK {
		t.Fatalf("upload answered %d: %s", status, body)
	}

	if got := f.deleteAPI("/api/v1/links/" + id.String()); got != http.StatusNoContent {
		t.Fatalf("deleting the link answered %d, want 204", got)
	}
	// The row and its bytes are still there, which is the state the sweep
	// exists for. Asserted rather than assumed: if link deletion ever becomes
	// hard, this test should say so instead of quietly testing nothing.
	if n := f.logoRowCount(t); n != 2 {
		t.Fatalf("%d rows hold a logo immediately after a soft delete, want 2; a "+
			"cascade that already fired would leave this sweep with no work", n)
	}
	// And the endpoint cannot reach it, which is what makes it an orphan rather
	// than something a workspace could tidy up itself.
	if got := f.deleteAPI(
		"/api/v1/links/" + id.String() + "/qr/codes/" + code.Slug + "/logo",
	); got != http.StatusNotFound {
		t.Errorf("clearing a deleted link's logo answered %d, want 404", got)
	}

	q := dbgen.New(f.pool)
	n, err := q.ClearOrphanedQRCodeLogos(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the sweep cleared %d rows, want 1", n)
	}
	if got := f.logoRowCount(t); got != 1 {
		t.Errorf("%d rows hold a logo after the sweep, want 1 — the live link's", got)
	}
	if got := f.storedLogo(t, keep, kept.Slug); len(got) == 0 {
		t.Error("the sweep took the live link's logo too")
	}

	// Idempotent: the predicate is `logo IS NOT NULL`, so a second run has
	// nothing to do. That is what lets it run hourly on every instance forever.
	again, err := q.ClearOrphanedQRCodeLogos(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("a second sweep cleared %d rows; it is supposed to find nothing", again)
	}
}

// TestALogoIsScopedToItsWorkspace. A code somebody cannot see is a code they
// cannot upload to, and the refusal must not confirm anything exists.
func TestALogoIsScopedToItsWorkspace(t *testing.T) {
	f := newRules(t)
	f.claim()

	status, _ := f.uploadLogo(t, uuid.New(), "abcdefgh", "a.png", "image/png",
		logoPNG(t, 16, 0x77))
	if status != http.StatusNotFound {
		t.Errorf("uploading to a link that does not exist = %d, want 404", status)
	}

	id := f.createLink("logoscope", "https://example.com/x")
	status, _ = f.uploadLogo(t, id, "zzzzzzzz", "a.png", "image/png", logoPNG(t, 16, 0x88))
	if status != http.StatusNotFound {
		t.Errorf("uploading to a slug this link never issued = %d, want 404", status)
	}
	// And the shorthand against a link that is not there, which must refuse for
	// the link rather than answer for a default code it invented.
	status, _ = f.uploadLogo(t, uuid.New(), "", "a.png", "image/png", logoPNG(t, 16, 0x99))
	if status != http.StatusNotFound {
		t.Errorf("uploading to the default code of a link that does not exist = %d, "+
			"want 404", status)
	}
	if n := f.logoRowCount(t); n != 0 {
		t.Errorf("%d rows hold a logo after three refused uploads", n)
	}
}

// --- the default code (M50.5, D136 overruled) --------------------------------

// TestTheDefaultCodeCarriesALogoToo is the owner's ruling of 2026-08-07 asserted
// against the database.
//
// **The case it covers is the common one.** A link nobody has added a second
// code to is nearly every link, and until the ruling it could carry no logo at
// all: the only path was `/qr/codes/{slug}`, and the default code's identity was
// the *absence* of a slug (D130, since reversed by D183 — the shorthand now
// answers for whichever code holds the flag). The shorthand `/qr/logo` is what reaches it,
// on exactly the shape `qr.svg` and `qr.png` already have.
//
// **And it has to write a row.** A default code with no `qr_codes` row is a real
// code drawn at the default style; under D134 the bytes are a column on the row,
// so there has to be one. That is the half a named code never needed — it exists
// before anything is uploaded to it — and it is why this is not simply the
// slugged test with a different path.
func TestTheDefaultCodeCarriesALogoToo(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logodefault", "https://example.com/x")

	// Nothing has touched this link's code, so it has no row. Asserted, because
	// the whole point of the test is the upload that has to create one.
	if n := f.qrRowCount(id); n != 0 {
		t.Fatalf("the link starts with %d qr_codes rows, want 0; a default code "+
			"nobody has styled is supposed to have none", n)
	}
	// Clearing before there is anything: a code with no row has no logo, so the
	// clear has already happened. 404 here would make removal a way to ask
	// whether a preference had ever been expressed.
	if got := f.deleteAPI(logoPath(id, "")); got != http.StatusNoContent {
		t.Errorf("clearing the default code's logo before one exists = %d, want 204", got)
	}
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("clearing nothing wrote %d rows; it is supposed to write none", n)
	}

	// What the code is drawn as before anything is uploaded, read off the code
	// itself rather than off a second link: since F171 the upload re-fits the
	// size, and the fit is against *this* payload's module count.
	untouched := f.qrCode(id)

	// A JPEG, and the filename and declared type disagree with it — so this
	// upload exercises the sniffing and the re-encode on the shorthand rather
	// than only the routing. It has to be a JPEG: a PNG this test encoded and a
	// PNG the server re-encoded from it are byte-identical, which would make
	// "what is stored is not what was sent" pass for the wrong reason.
	src := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := range 40 {
		for x := range 40 {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 6), G: 0xa1, B: uint8(y * 6), A: 0xff})
		}
	}
	var upload bytes.Buffer
	if err := jpeg.Encode(&upload, src, nil); err != nil {
		t.Fatal(err)
	}
	if status, body := f.uploadLogo(t, id, "", "../../etc/passwd", "image/svg+xml",
		upload.Bytes()); status != http.StatusOK {
		t.Fatalf("uploading to the default code answered %d: %s", status, body)
	}
	if n := f.qrRowCount(id); n != 1 {
		t.Fatalf("the link holds %d qr_codes rows after the upload, want 1", n)
	}

	stored := f.storedLogo(t, id, "")
	if len(stored) == 0 {
		t.Fatal("nothing was stored against the default code")
	}
	if bytes.Equal(stored, upload.Bytes()) {
		t.Error("the column holds the bytes that were uploaded; what is stored is a " +
			"PNG this product encoded, on the shorthand exactly as on a named code")
	}
	if _, format, err := image.Decode(bytes.NewReader(stored)); err != nil || format != "png" {
		t.Errorf("the stored bytes decode as %q (%v); every stored logo is a PNG",
			format, err)
	}

	// The style is the one the code was already being drawn at, **except for the
	// error-correction level and the two fields that carry the size** (M50.6,
	// D141; F171, D174). Materialising the row still changes no picture — that is
	// D139's claim and it is unchanged — but an upload does three things now:
	// stores the image, raises the code to H, and re-fits the quiet zone and the
	// scale so that H's larger symbol still draws at the size the code already
	// had. Without the re-fit a style near the raster ceiling grows past it and
	// the PNG download starts answering 422, which is F171.
	//
	// So what is compared is what the reader can name — the colours, and how big
	// the picture is — rather than the arithmetic underneath it.
	//
	// *(Until M50.6 this compared the whole style against a second, untouched
	// link and nothing drew a logo, so there was nothing to raise the level for.
	// M50.6 excepted the level. F171 is why margin and scale left too, and why
	// the comparison is now against this code's own before-state: the fit is
	// against this payload's module count, and another link's is another
	// number.)*
	after := f.qrCode(id)
	if after.Style.Level != qr.LevelH {
		t.Errorf("the code carries a logo and is stored at level %q, want H",
			after.Style.Level)
	}
	if after.Style.Foreground != untouched.Style.Foreground ||
		after.Style.Background != untouched.Style.Background {
		t.Errorf("the upload repainted the code: %+v became %+v; an image is not a "+
			"colour change", untouched.Style, after.Style)
	}
	// **Exactly the size it had, since D182.** This allowed half a span of drift
	// while qr.FitSize could only land on sizes a whole module grid admitted; the
	// quiet zone carries the remainder in pixels now, so the fit lands on the
	// number and the assertion is equality — the same one its siblings in
	// internal/link make of the very same re-fit.
	//
	// **One case legitimately moves and it is not this one.** Where the level-H
	// symbol needs more pixels than the code is already drawn at, qr.FitSize
	// refuses on qr.MinSizeFor's floor and refitForLogo leaves the style alone
	// rather than costing a logo that has already been written — which draws a
	// *larger* picture. That escape is checked rather than assumed, because a
	// fixture that drifted into it would leave the equality below measuring
	// nothing.
	atH, err := qr.Encode(f.qrContent(id), qr.LevelH)
	if err != nil {
		t.Fatal(err)
	}
	if floor := qr.MinSizeFor(atH.Size); untouched.Size < floor {
		t.Fatalf("this code is drawn at %dpx and its %d-module level-H symbol needs "+
			"%dpx, so the re-fit takes its escape and the picture is supposed to grow; "+
			"what follows measures the ordinary path", untouched.Size, atH.Size, floor)
	}
	if after.Size != untouched.Size {
		t.Errorf("the code was %dpx and is %dpx after the upload (%+v → %+v). Level H "+
			"is a larger symbol and the re-fit is what keeps the size somebody "+
			"chose — to the pixel, since D182", untouched.Size, after.Size,
			untouched.Style, after.Style)
	}
	if after.Size > qr.MaxSize {
		t.Errorf("the upload left the code drawing %dpx, past the %dpx raster bound; "+
			"GET …/qr.png answers 422 from here", after.Size, qr.MaxSize)
	}
	// `stored` turning true is the one visible consequence, and it is honest:
	// there is a row now.
	if code := f.qrCode(id); !code.Stored || !code.HasLogo || code.Slug != "" {
		t.Errorf("the default code reads back stored=%v has_logo=%v slug=%q; want "+
			"true, true and the empty slug", code.Stored, code.HasLogo, code.Slug)
	}

	// Replacing, and then clearing, on the same terms a named code gets.
	if status, body := f.uploadLogo(t, id, "", "b.png", "image/png",
		logoPNG(t, 40, 0xb2)); status != http.StatusOK {
		t.Fatalf("replacing the default code's logo answered %d: %s", status, body)
	}
	if bytes.Equal(f.storedLogo(t, id, ""), stored) {
		t.Error("the second upload did not replace the first")
	}
	if got := f.deleteAPI(logoPath(id, "")); got != http.StatusNoContent {
		t.Fatalf("clearing the default code's logo = %d, want 204", got)
	}
	if got := f.storedLogo(t, id, ""); got != nil {
		t.Errorf("clearing left %d bytes in the column", len(got))
	}
	// The row stays, because the style preference it also carries is not what
	// was being removed — the same trade the named-code clear makes.
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("clearing the logo left %d rows, want the code's own 1", n)
	}
}

// TestTheDashboardReachesTheLogoOnTheDefaultCode is the other half of the
// ruling: the capability is on the dashboard and not only on the API.
//
// The panel posts a `multipart/form-data` body because a file needs one, which
// is why `next` and `code` travel as parts rather than as an encoded form — and
// `code` is the empty string for the default code, which is what makes the
// common case reachable without a script and without the API.
func TestTheDashboardReachesTheLogoOnTheDefaultCode(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logopanel", "https://example.com/x")
	panelPath := "/links/" + id.String() + "/qr"

	// The panel offers the control before anything is uploaded, and offers no
	// removal — there is nothing to remove.
	page := f.getHTML(panelPath)
	if !strings.Contains(page, `action="`+panelPath+`/logo"`) {
		t.Fatal("the QR panel has no logo upload form; the capability is supposed " +
			"to be on the dashboard and not only on the API")
	}
	if strings.Contains(page, `name="remove_logo"`) {
		t.Error("the panel offers to remove a logo that has not been uploaded")
	}

	contentType, body := logoBody(t, "brand.png", "image/png", logoPNG(t, 48, 0xc3),
		map[string]string{"next": panelPath, "code": ""})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.server.URL+panelPath+"/logo", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.StatusCode
	where := resp.Header.Get("Location")
	_ = resp.Body.Close()
	if got != http.StatusSeeOther {
		t.Fatalf("the panel's upload answered %d, want 303", got)
	}
	// Back to the panel it was posted from, carrying the marker that produces
	// the sentence about what the upload changed (M50.6 rewrote it; before that
	// it said nothing drew the logo yet).
	if !strings.HasPrefix(where, panelPath+"?") || !strings.Contains(where, "qr=logo") {
		t.Errorf("the upload redirected to %q; want the panel it was posted from, "+
			"marked qr=logo", where)
	}
	if len(f.storedLogo(t, id, "")) == 0 {
		t.Fatal("the panel's upload stored nothing")
	}

	// And now the removal control is drawn, and it works — on the style form's
	// own route, under its own button name, like reset and remove already are.
	if page = f.getHTML(panelPath); !strings.Contains(page, `name="remove_logo"`) {
		t.Fatal("the panel does not offer to remove a logo that is there")
	}
	f.postQRForm(t, panelPath, url.Values{
		"next": {panelPath}, "code": {""}, "remove_logo": {"1"},
	})
	if got := f.storedLogo(t, id, ""); got != nil {
		t.Errorf("the panel's removal left %d bytes in the column", len(got))
	}
}

// --- resizing rather than refusing (M50.5, reopened 2026-08-12) --------------

// postLogo posts a multipart upload at the dashboard's route and hands back the
// response, so a test can read a redirect or a rendered refusal.
//
// `headers` is what carries `HX-Request`, which is the whole difference between
// the two paths this file asserts: the form submits itself over htmx now, and
// htmx never swaps a 4xx.
func (f *ruleFixture) postLogo(
	t *testing.T, path string, fields map[string]string, body []byte,
	filename, declared string, headers map[string]string,
) *http.Response {
	t.Helper()
	contentType, payload := logoBody(t, filename, declared, body, fields)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.server.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestAnOversizedLogoIsStoredSmallerAndSaysSo is F214(a), end to end and on the
// surface the owner met it on.
//
// **813×813 is the reported measurement, not a derived one.** It sits inside the
// side bound and four times past what a row holds, which is the shape that used
// to produce a refusal naming two caps and no verdict. What it must produce now
// is a stored image, a redirect carrying both sizes, and a sentence on the page
// that names them — the column is read directly, because a redirect saying
// 512×512 over a row holding 813×813 is exactly the drift worth catching.
func TestAnOversizedLogoIsStoredSmallerAndSaysSo(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logofit", "https://example.com/fit")
	panelPath := "/links/" + id.String() + "/qr"

	resp := f.postLogo(t, panelPath+"/logo",
		map[string]string{"next": panelPath, "code": ""},
		logoPNG(t, 813, 0x5a), "big.png", "image/png", nil)
	where := resp.Header.Get("Location")
	status := resp.StatusCode
	_ = resp.Body.Close()

	if status != http.StatusSeeOther {
		t.Fatalf("an 813x813 upload answered %d, want 303. Since F214 an image inside "+
			"the side bound is resized to fit rather than refused", status)
	}
	for _, want := range []string{"from=813x813", "to=512x512"} {
		if !strings.Contains(where, want) {
			t.Errorf("the redirect %q does not carry %q; the page it lands on is a "+
				"fresh request that never saw the file, so this is the only way the "+
				"warning gets there", where, want)
		}
	}

	stored := f.storedLogo(t, id, "")
	cfg, err := qr.DecodeLogoConfig(stored)
	if err != nil {
		t.Fatalf("the column does not hold a decodable image: %v", err)
	}
	if cfg.Width != 512 || cfg.Height != 512 {
		t.Errorf("the column holds %dx%d and the redirect promised 512x512",
			cfg.Width, cfg.Height)
	}
	if cfg.Width*cfg.Height > qr.MaxLogoPixels {
		t.Errorf("the stored image is %d pixels and a row holds %d",
			cfg.Width*cfg.Height, qr.MaxLogoPixels)
	}

	// And the sentence is on the page, with both numbers in it.
	page := f.getHTML(where)
	for _, want := range []string{"813×813", "512×512"} {
		if !strings.Contains(page, want) {
			t.Errorf("the panel does not say %q after resizing the upload; a product "+
				"that silently shrinks artwork and reports success has told nobody", want)
		}
	}
}

// TestARefusalReachesAnHTMXUpload is F214(c)'s cost, paid.
//
// The form applies the file the moment it is chosen, which makes it the first
// form in this panel to post over htmx — and htmx's default response handling
// swaps nothing at all for a 4xx. A refusal answered 422 would therefore leave
// the page exactly as it was, which is worse than the two-step it replaced. The
// native post is asserted alongside it, because that status must not move.
func TestARefusalReachesAnHTMXUpload(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logohx", "https://example.com/hx")
	panelPath := "/links/" + id.String() + "/qr"
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="4" height="4"/></svg>`)
	fields := map[string]string{"next": panelPath, "code": ""}

	resp := f.postLogo(t, panelPath+"/logo", fields, svg, "brand.png", "image/png",
		map[string]string{"HX-Request": "true"})
	body, err := io.ReadAll(resp.Body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("an htmx upload refused with %d; htmx swaps no 4xx, so the reason "+
			"would never reach the page", status)
	}
	if !strings.Contains(string(body), "SVG") {
		t.Error("the htmx refusal carries no reason; the swap would replace the panel " +
			"with a panel saying nothing went wrong")
	}
	if !strings.Contains(string(body), `id="qr"`) {
		t.Error("the htmx refusal renders no id=\"qr\"; that is what the form selects " +
			"out of the response, so the swap would find nothing to put in")
	}

	// A browser posting the same form without htmx is unchanged.
	resp = f.postLogo(t, panelPath+"/logo", fields, svg, "brand.png", "image/png", nil)
	status = resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusUnprocessableEntity {
		t.Errorf("a native post was refused with %d, want 422; only the htmx path "+
			"trades the status for a swap that happens", status)
	}
}

// TestARefusalNamesTheMeasurementWhereSomebodyReadsIt is F214(a)'s other half,
// asserted on the two surfaces a person meets it on rather than on the string
// internal/qr produces.
//
// **`qr.LogoBoundError.Error()` reaches nobody.** `qrLogoError` in internal/link
// discards it and writes its own `domain.ValidationErrors` message, which is what
// the API renders into a `422` body and what the panel renders into the page. A
// test on the first sentence and none on the second covers the unreachable one of
// the two — which is how the shipped refusal came to name two caps and no
// measurement without anything noticing.
//
// 1600×400 is the fixture because it crosses one bound and only one, and because
// it is a rectangle: the sentence has to say *wide* rather than pick a word a
// square would make true either way.
func TestARefusalNamesTheMeasurementWhereSomebodyReadsIt(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logobig", "https://example.com/big")
	big := logoPNGSize(t, 1600, 400, 0x3c)
	if len(big) > qr.MaxLogoUploadBytes {
		t.Fatalf("the fixture is %d bytes and the body cap is %d; it has to be refused "+
			"for its dimensions, not for its size", len(big), qr.MaxLogoUploadBytes)
	}

	// The API surface.
	status, body := f.uploadLogo(t, id, "", "wide.png", "image/png", big)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 1600x400 upload answered %d, want 422; 1600 is past the 1024 a side "+
			"this product decodes", status)
	}
	var refusal struct {
		Errors []struct {
			Field, Code, Message string
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("the 422 body is not the validation shape: %v — %s", err, body)
	}
	if len(refusal.Errors) != 1 || refusal.Errors[0].Field != "logo" ||
		refusal.Errors[0].Code != "too_large" {
		t.Fatalf("the 422 carries %+v, want one too_large on the logo field", refusal.Errors)
	}
	sentence := refusal.Errors[0].Message
	for _, want := range []string{"1600×400", "1600", "wide", "1024"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the 422 message does not carry %q; a refusal names what the file "+
				"measured and the one bound it crossed: %s", want, sentence)
		}
	}
	// And it does not stand a second cap beside that with no verdict attached,
	// which is the shape F214 was raised about. The page below carries 262,144 in
	// its help text, where it is a description of the resize and not a refusal —
	// so this half is asserted on the sentence rather than on the document.
	for _, unwanted := range []string{"262144", "262,144"} {
		if strings.Contains(sentence, unwanted) {
			t.Errorf("the 422 message names %q, which refuses nothing since F214: %s",
				unwanted, sentence)
		}
	}

	// The dashboard surface, and it has to be the *same* sentence. htmx swaps no
	// 4xx, so the panel's refusal arrives at 200 with the reason in it —
	// TestARefusalReachesAnHTMXUpload holds that status; this holds what the page
	// then says. Comparing against the API's own string is what makes a divergence
	// between the two surfaces a failure rather than two tests drifting apart.
	panelPath := "/links/" + id.String() + "/qr"
	resp := f.postLogo(t, panelPath+"/logo",
		map[string]string{"next": panelPath, "code": ""}, big, "wide.png", "image/png",
		map[string]string{"HX-Request": "true"})
	page, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), sentence) {
		t.Errorf("the panel does not carry the refusal the API gives — %q. A dimension "+
			"refusal that reaches a person through the dashboard has to name the same "+
			"measurement and the same one bound", sentence)
	}
}

// TestTheAPIReportsAResize is the same fact on the other surface.
//
// An API client that sent artwork and got a bare `200` would have no way to know
// the bytes it sent are not the bytes now stored, so the upload response carries
// `resampled` — and carries it only when it happened, which is what makes its
// presence mean anything.
func TestTheAPIReportsAResize(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logoapi", "https://example.com/api")

	var big struct {
		Resampled *struct {
			FromWidth  int `json:"from_width"`
			FromHeight int `json:"from_height"`
			Width      int `json:"width"`
			Height     int `json:"height"`
		} `json:"resampled"`
	}
	status, body := f.uploadLogo(t, id, "", "big.png", "image/png", logoPNG(t, 813, 0x11))
	if status != http.StatusOK {
		t.Fatalf("an 813x813 upload answered %d, want 200", status)
	}
	if err := json.Unmarshal(body, &big); err != nil {
		t.Fatal(err)
	}
	if big.Resampled == nil {
		t.Fatal("the response carries no resampled block for an image that was shrunk")
	}
	if big.Resampled.FromWidth != 813 || big.Resampled.FromHeight != 813 ||
		big.Resampled.Width != 512 || big.Resampled.Height != 512 {
		t.Errorf("resampled says %dx%d became %dx%d; 813x813 became 512x512",
			big.Resampled.FromWidth, big.Resampled.FromHeight,
			big.Resampled.Width, big.Resampled.Height)
	}

	var small struct {
		Resampled *json.RawMessage `json:"resampled"`
	}
	status, body = f.uploadLogo(t, id, "", "small.png", "image/png", logoPNG(t, 48, 0x22))
	if status != http.StatusOK {
		t.Fatalf("a 48x48 upload answered %d, want 200", status)
	}
	if err := json.Unmarshal(body, &small); err != nil {
		t.Fatal(err)
	}
	if small.Resampled != nil {
		t.Error("a 48x48 upload reported a resample; the key's presence is the signal, " +
			"so one that is always there says nothing")
	}
}

// --- the logo in the picture (M50.6) -------------------------------------------

// TestALogoRaisesTheCodeToLevelHAndSaysSo is m50.6.md's contract bullet.
//
// *"what a request setting `level=L` together with a logo gets back"* — the
// milestone required the answer be decided and recorded, and D141 decides
// **accept and override**. What that has to be worth is the second half of the
// same bullet: *"a `GET` after a `PUT` returns what the server actually
// applied"*. So this asks three times — after the upload, after a `PUT` that
// names `L`, and on a fresh `GET` — and the answer is `H` every time.
//
// Silent drift between what was sent and what is stored is the one outcome the
// contract test exists to catch, and the third read is the one that would catch
// it: a handler could echo `H` back and write `L` to the row.
func TestALogoRaisesTheCodeToLevelHAndSaysSo(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("poster-level", "https://example.com/summer")

	// Deliberately naming L to begin with, so H is never the level it already
	// had. **It does not read back as L and cannot, since D187**: the level a
	// request names is a floor, the free level is never below `M`, so `L` is a
	// value this API accepts and nothing draws. What the fixture needs is only
	// that the code is not already at H, and that the level reported is the one
	// the rule gives this floor.
	if resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"level":"L"}}`); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("setting the level to L = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	before := f.apiQRLevel(t, id)
	if before == qr.LevelH {
		t.Fatalf("the code is at H before the upload, so nothing below can tell the " +
			"upload's forcing from the level it already had")
	}
	if want := qr.LevelFor(f.qrCode(id).Content, qr.LevelL); before != want {
		t.Fatalf("the code reports level %q before the upload; a floor of L on this "+
			"payload is drawn at %q", before, want)
	}

	if status, body := f.uploadLogo(
		t, id, "", "brand.png", "image/png", logoPNG(t, 64, 0x5a),
	); status != http.StatusOK {
		t.Fatalf("the upload answered %d: %s", status, body)
	}

	if got := f.apiQRLevel(t, id); got != qr.LevelH {
		t.Errorf("after the upload the code reports level %q, want H. A logo "+
			"occludes modules and the occlusion cap is measured against what H "+
			"recovers; a code drawn at L with a logo is outside the derivation", got)
	}
	// **The column, not the service's view of it.** `qrCodeFrom` reports the
	// level a logo'd code is *drawn* at, so reading through the service would be
	// green for a row that still said `L` — which is the drift this is here to
	// refuse. The row is where the answer has to be.
	if got := f.storedQRLevel(t, id, "").Level; got != qr.LevelH {
		t.Errorf("the `qr_codes` row holds level %q after the upload, want H — the "+
			"response and the row have to agree, or a later read reports something "+
			"the picture is not", got)
	}

	// And a PUT that names L is accepted and overridden rather than refused, so
	// a client restyling a logo'd code does not get a 422 for a field it never
	// thought about.
	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"level":"L","foreground":"#112233"}}`)
	raw := readAll(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with level L on a logo'd code = %d, want 200: %s",
			resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"level":"H"`) {
		t.Errorf("the PUT's own answer does not report level H:\n%s", raw)
	}
	if got := f.apiQRLevel(t, id); got != qr.LevelH {
		t.Errorf("a GET after the PUT reports level %q, want H. The response said "+
			"one thing and the row holds another, which is exactly the drift this "+
			"contract test exists to catch", got)
	}
	if got := f.storedQRLevel(t, id, "").Level; got != qr.LevelH {
		t.Errorf("the `qr_codes` row holds level %q after a PUT naming L, want H", got)
	}
	// The rest of the style did land, so the override is one field and not a
	// refusal wearing a 200.
	if got := f.storedQRStyle(id).Foreground; got != "#112233" {
		t.Errorf("the foreground is %q, want #112233; the override ate the request", got)
	}
}

// TestRemovingALogoRecomputesTheLevelRatherThanKeepingH is F223, and it is the
// sentence this milestone's reopening takes back.
//
// M50.6 left the level at H when the image went, and said why: lowering it
// redraws a picture that may already be printed, and H is the safer of the two
// to be left at. The owner overruled it with the cost measured — H is ~30% more
// modules a side than the code needs, so it scans from proportionally less
// distance — and with the reason it is safe: *"the old QR should still resolve
// as long as the link stays the same, so a change in the new code shouldn't be
// an issue."* The payload is untouched by any of this.
//
// **Recomputed rather than restored**, which is not a distinction without a
// difference: the upload wrote H over whatever the row held, so there is no
// remembered level to put back. What the code gets is the rule's answer for its
// own payload, which is what a code that never carried a logo has.
//
// The size is asserted at every step because it is the other claim in the
// building: M49's *the requested size is the size stored and drawn, exactly*
// runs straight through a level change, the level being what decides the module
// count a size is fitted against.
func TestRemovingALogoRecomputesTheLevelRatherThanKeepingH(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logolevel", "https://example.com/summer")

	// What this code is drawn at with nothing on it — the level the removal has
	// to come back to. Read off the product rather than written down, because it
	// is a property of the payload, and refused if it is H: on a payload where
	// the rule already answers H there is nothing for the removal to change and
	// this test would pass without asserting anything.
	untouched := f.qrCode(id)
	want := qr.LevelFor(untouched.Content, "")
	if want == qr.LevelH {
		t.Fatalf("the rule already answers H for %q, so a logo changes no level and "+
			"this test measures nothing", untouched.Content)
	}
	if untouched.Style.Level != want {
		t.Fatalf("a code nobody has touched reports level %q and the rule says %q",
			untouched.Style.Level, want)
	}

	const size = 600
	if resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		fmt.Sprintf(`{"style":{"size":%d}}`, size)); resp.StatusCode != http.StatusOK {
		body := readAll(t, resp)
		_ = resp.Body.Close()
		t.Fatalf("setting the size = %d: %s", resp.StatusCode, body)
	} else {
		_ = resp.Body.Close()
	}
	if got := f.qrCode(id).Size; got != size {
		t.Fatalf("the code is %dpx after asking for %d", got, size)
	}

	if status, body := f.uploadLogo(
		t, id, "", "brand.png", "image/png", logoPNG(t, 64, 0x5a),
	); status != http.StatusOK {
		t.Fatalf("the upload answered %d: %s", status, body)
	}
	if got := f.qrCode(id).Style.Level; got != qr.LevelH {
		t.Fatalf("the code carries a logo and reports level %q, want H", got)
	}
	if got := f.storedQRLevel(t, id, "").Level; got != qr.LevelH {
		t.Fatalf("the row holds level %q under a logo, want H", got)
	}

	if got := f.deleteAPI(logoPath(id, "")); got != http.StatusNoContent {
		t.Fatalf("clearing the logo = %d, want 204", got)
	}
	after := f.qrCode(id)
	if after.Style.Level != want {
		t.Errorf("the logo is gone and the code still reports level %q, want the "+
			"rule's %q. H is ~30%% more modules a side than this code needs, on a "+
			"code with nothing left covering it", after.Style.Level, want)
	}
	// **The row, not only the view.** A row that still said H with the view
	// resolving something else is the drift the level's contract test refuses in
	// the other direction, and it is what a later style write would carry
	// forward.
	stored := f.storedQRLevel(t, id, "")
	if stored.Level != "" {
		t.Errorf("the row still names level %q after the logo left; an unset level "+
			"is what asks for the rule, and a level nobody chose is the finding",
			stored.Level)
	}
	if after.Size != size || stored.Size != size {
		t.Errorf("the code was %dpx before the logo and is %dpx after it left, with "+
			"%dpx in the row. Recomputing the level changes the symbol and must not "+
			"change the number somebody typed", size, after.Size, stored.Size)
	}
	// Idempotent, and it stays that way: a second clear must not rewrite a style
	// on a code whose logo left long ago, which is what the has_logo guard buys.
	if got := f.deleteAPI(logoPath(id, "")); got != http.StatusNoContent {
		t.Errorf("clearing a second time answered %d, want 204", got)
	}
	if again := f.storedQRLevel(t, id, ""); again != stored {
		t.Errorf("a second clear rewrote the style: %+v became %+v", stored, again)
	}
}

// TestAddingACodeDoesNotInheritTheLogosLevel is F223 through the door it was
// rebuilt in, one milestone after being closed.
//
// A code carrying a logo holds `H` in its row by construction: `refitForLogo`
// writes it on upload and `storeQRStyle` writes it again on every style write,
// so that a `GET` reports what will be drawn (D141). `CreateQRCode` copies the
// default code's style onto the code it creates — that is what makes a second
// code look like the first — and the upsert's insert branch leaves the new row's
// `logo` NULL, because a code that has just come into being has no image. So the
// copy used to arrive at `H` with nothing covering it, **permanently**: the only
// path that recomputes a level is `ClearQRCodeLogo`, and it fires for a code that
// *had* a logo, which this one never did.
//
// Two clicks from the dashboard — upload a logo to a link's default code, then
// *Add another code* — and the cost is F223's own: ~30% more modules a side than
// the code needs, so it scans from proportionally less distance, for nothing.
//
// The size is asserted with it because the two travel together: `H` is a larger
// symbol, so a copy that inherited the level would also have been fitted against
// a symbol it does not draw.
func TestAddingACodeDoesNotInheritTheLogosLevel(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("logocopy", "https://example.com/autumn")

	if status, body := f.uploadLogo(
		t, id, "", "brand.png", "image/png", logoPNG(t, 64, 0x5a),
	); status != http.StatusOK {
		t.Fatalf("the upload answered %d: %s", status, body)
	}
	if got := f.storedQRLevel(t, id, "").Level; got != qr.LevelH {
		t.Fatalf("the row holds level %q under a logo, want H — this test needs the "+
			"H it is about to check is not copied", got)
	}

	added := f.createQRCode(t, id, "Autumn poster")
	// The level the created code's **own** payload gets for free. Its content is
	// not the default's: creating a second code gives both of them a `qrc` tag,
	// so the free level has to be asked of the picture this code actually draws.
	// Refused if it is already H — there would then be nothing for the
	// inheritance to get wrong.
	want := qr.LevelFor(added.Content, "")
	if want == qr.LevelH {
		t.Fatalf("the rule already answers H for %q, so a copied logo level changes "+
			"nothing and this test measures nothing", added.Content)
	}
	if added.Style.Level != want {
		t.Errorf("the created code reports level %q and draws no logo; want the "+
			"rule's %q. H on an uncovered code is ~30%% more modules a side than it "+
			"needs, and nothing recomputes a level for a code that never had an image",
			added.Style.Level, want)
	}
	if stored := f.storedQRLevel(t, id, added.Slug); stored.Level == qr.LevelH {
		t.Errorf("the created code's row names level H, copied off a code whose logo " +
			"forced it. The row is what a later style write carries forward")
	}
	// The picture, not only the report: a code drawn at H is a wider symbol, and
	// a size fitted against the copied one would be fitted against a symbol this
	// code does not draw.
	drawn, err := qr.Encode(added.Content, added.Style.Level)
	if err != nil {
		t.Fatal(err)
	}
	free, err := qr.Encode(added.Content, "")
	if err != nil {
		t.Fatal(err)
	}
	if drawn.Size != free.Size {
		t.Errorf("the created code draws %d modules where its own payload gets %d "+
			"for free", drawn.Size, free.Size)
	}
	if added.Size != qr.OutputSize(free.Size, added.Style) {
		t.Errorf("the created code reports %dpx and its style over its own symbol "+
			"draws %dpx", added.Size, qr.OutputSize(free.Size, added.Style))
	}
}

// storedQRLevel reads `qr_codes.style` out of the database, because the level a
// row *holds* and the level a code is *drawn at* are two claims and only one of
// them is answered by the service.
func (f *ruleFixture) storedQRLevel(t *testing.T, linkID uuid.UUID, slug string) qr.Style {
	t.Helper()
	var blob []byte
	if err := f.pool.QueryRow(t.Context(),
		`SELECT style FROM qr_codes WHERE link_id = $1 AND slug = $2`,
		linkID, slug).Scan(&blob); err != nil {
		t.Fatalf("read the stored style for %s/%q: %v", linkID, slug, err)
	}
	var out qr.Style
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("the stored style is not a style: %v", err)
	}
	return out
}

// apiQRLevel reads the level out of the API's own answer, which is the surface
// the contract is about.
func (f *ruleFixture) apiQRLevel(t *testing.T, linkID uuid.UUID) qr.Level {
	t.Helper()
	resp := f.get("/api/v1/links/"+linkID.String()+"/qr", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET qr = %d", resp.StatusCode)
	}
	var out struct {
		QR struct {
			Style qr.Style `json:"style"`
		} `json:"qr"`
	}
	if err := json.Unmarshal(readAll(t, resp), &out); err != nil {
		t.Fatal(err)
	}
	return out.QR.Style.Level
}

// TestBothServedPicturesCarryTheLogo is the compositing, at the layer a reader
// meets it.
//
// internal/qr holds the module-by-module comparison of the two outputs; what
// this owes is that the two *endpoints* serve them — that the bytes somebody
// downloads are the picture the dashboard is showing, with the image in it.
func TestBothServedPicturesCarryTheLogo(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("poster-drawn", "https://example.com/summer")

	before := f.qrSVG(id)
	if strings.Contains(before, "<image") {
		t.Fatal("a code with no logo is served with an <image> in it")
	}
	beforePNG := f.qrPNGBytes(t, id)

	if status, body := f.uploadLogo(
		t, id, "", "brand.png", "image/png", logoPNG(t, 64, 0x77),
	); status != http.StatusOK {
		t.Fatalf("the upload answered %d: %s", status, body)
	}

	after := f.qrSVG(id)
	if !strings.Contains(after, `<image `) ||
		!strings.Contains(after, `href="data:image/png;base64,`) {
		t.Errorf("the served SVG carries no embedded image:\n%.400s", after)
	}
	// The one directive that makes the embedded form renderable inside the
	// dashboard, checked on a page rather than only as a constant.
	page := f.getHTML("/links/" + id.String() + "/qr")
	if !strings.Contains(page, "<image ") {
		t.Error("the dashboard's own drawing carries no logo, so the panel shows a " +
			"different picture from the one the download produces")
	}

	afterPNG := f.qrPNGBytes(t, id)
	if bytes.Equal(beforePNG, afterPNG) {
		t.Error("the rasterised code is byte-identical with and without a logo")
	}
	img, format, err := image.Decode(bytes.NewReader(afterPNG))
	if err != nil {
		t.Fatalf("the served PNG does not decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("the served bytes decoded as %q", format)
	}
	if _, paletted := img.(*image.Paletted); paletted {
		t.Error("the composited PNG came back paletted; a logo has colours a " +
			"two-entry palette cannot hold, and pngWithLogo's allocation figure is " +
			"calculated from the four-byte form")
	}
}

// qrPNGBytes fetches the rasterised default code.
func (f *ruleFixture) qrPNGBytes(t *testing.T, linkID uuid.UUID) []byte {
	t.Helper()
	resp := f.get("/api/v1/links/"+linkID.String()+"/qr.png", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET qr.png = %d", resp.StatusCode)
	}
	return readAll(t, resp)
}

// TestTheSizeControlFitsAgainstTheLevelALogoIsDrawnAt is F190.
//
// **A size is a pixel arithmetic over a module count, and a logo changes the
// module count.** The size control read the level off the row and fitted a
// margin and scale against it; the renderer forces H whenever there is a logo.
// When the two disagree the reader types 800 and gets a picture that is not
// 800px, silently — which is the class of defect F171 was, one function along.
//
// The disagreement is written by hand here because the product's own writes
// cannot produce it: `SetQRCodeLogo` and `SetQRStyleBySlug` both put H into the
// row whenever a logo is present. That it cannot be produced *today* is not what
// makes it a row — `qrCodeFrom` already applies the same defence on read, and it
// was written for a row from before M50.6, one written by hand, or a style write
// that raced an upload. The defence existed at one of the two sites that need it.
func TestTheSizeControlFitsAgainstTheLevelALogoIsDrawnAt(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("poster-sized", "https://example.com/summer-campaign")

	if status, body := f.uploadLogo(
		t, id, "", "brand.png", "image/png", logoPNG(t, 64, 0x5a),
	); status != http.StatusOK {
		t.Fatalf("the upload answered %d: %s", status, body)
	}

	// Down to L in the column only. The row now says one thing and the renderer
	// will do another, which is the state the whole finding is about.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE qr_codes SET style = jsonb_set(style, '{level}', '"L"')
		  WHERE link_id = $1 AND slug = ''`, id); err != nil {
		t.Fatalf("write the disagreeing row: %v", err)
	}
	if got := f.storedQRLevel(t, id, "").Level; got != qr.LevelL {
		t.Fatalf("the row holds level %q; this test needs one that disagrees with "+
			"the renderer", got)
	}

	// Two module counts, so a failure can say how far apart they are rather than
	// only that the picture is the wrong size.
	content := f.qrContent(id)
	atL, err := qr.Encode(content, qr.LevelL)
	if err != nil {
		t.Fatal(err)
	}
	atH, err := qr.Encode(content, qr.LevelH)
	if err != nil {
		t.Fatal(err)
	}
	if atH.Size <= atL.Size {
		t.Fatalf("this payload encodes to %d modules at H and %d at L; if H is not "+
			"the larger symbol the fit cannot disagree and this test measures nothing",
			atH.Size, atL.Size)
	}

	const want = 800
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#000000"}, "background": {"#ffffff"},
		"size": {strconv.Itoa(want)},
	})

	// The picture the endpoint actually serves, which is drawn at H whatever the
	// row says — qr.Render forces it, and that is what makes the row's level the
	// wrong thing to have fitted against.
	drawn := attrOf(t, f.qrSVG(id), `width="(\d+)"`)
	stored := f.storedQRLevel(t, id, "")
	// **The number that was asked for, exactly** (D182). This allowed half a span
	// while qr.FitSize could only land on sizes a whole module grid admitted; the
	// quiet zone carries the remainder in pixels now, so every value in the range
	// is reachable and equality is what the product promises. Nothing here has the
	// logo re-fit's escape either: the size control *refuses* a request below
	// qr.MinSizeFor rather than drifting past it, and 800px clears that floor at
	// every version this product encodes to. A fit taken against L's smaller
	// symbol lands somewhere else entirely, which is what this test is looking
	// for.
	if drawn != want {
		t.Errorf("the reader asked for %dpx and the served picture is %dpx, with "+
			"margin %d and scale %d on a %d-module symbol. The size set is the size "+
			"drawn; the cause this test was built to catch is a fit taken against the "+
			"%d modules the row's level encodes to instead", want, drawn,
			stored.Margin, stored.Scale, atH.Size, atL.Size)
	}
	// And the panel's own number is the same number, so the two surfaces do not
	// disagree about a picture they both describe.
	if got := f.qrCode(id).Size; got != drawn {
		t.Errorf("the panel reports %dpx and the endpoint draws %dpx", got, drawn)
	}
}

// TestTheShorthandsLogoLandsOnTheDefaultCodesOwnRow is the seam D183 opened
// under the logo operations.
//
// **The shorthand asks for a role and the storage needs a row.** `PUT
// …/qr/logo` carries no slug, and the empty string it stands for stopped being
// an identity when the flag replaced it — so every write and every read behind
// it has to resolve the flag first. Keying the upsert on what was *asked for*
// inserts a second row against the empty slug on any link whose default has one,
// which is every link carrying more than one code: the image lands on a code
// nobody can see, the picture the reader downloads has no logo in it, and the
// link quietly grows a phantom third code.
//
// Three assertions, one per place the resolution has to happen: the write, the
// row count, and the read the renderer makes.
func TestTheShorthandsLogoLandsOnTheDefaultCodesOwnRow(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("shorthandlogo", "https://example.com/x")
	// A second code, which is what gives the default a slug of its own.
	f.createQRCode(t, id, "Autumn poster")
	def := f.qrCode(id)
	if def.Slug == "" {
		t.Fatal("the default code has no slug on a link carrying two codes")
	}

	if status, body := f.uploadLogo(t, id, "", "logo.png", "image/png",
		logoPNG(t, 256, 0x20)); status != http.StatusOK {
		t.Fatalf("uploading through the shorthand = %d: %s", status, body)
	}

	if got := f.qrCode(id); got.Slug != def.Slug || !got.HasLogo {
		t.Errorf("the shorthand answered for %q has_logo=%v, want %q with a logo — "+
			"an empty slug here means the upload made a row of its own",
			got.Slug, got.HasLogo, def.Slug)
	}
	if n := f.qrRowCount(id); n != 2 {
		t.Errorf("%d qr_codes rows after uploading through the shorthand, want the "+
			"link's own 2; a third is a code the reader never made", n)
	}
	// And the picture the renderer draws carries it, which is the half a row
	// count cannot see: the logo read is keyed by slug too.
	if svg := f.qrSVG(id); !strings.Contains(svg, "image") {
		t.Errorf("the default code's picture carries no image after an upload "+
			"through the shorthand; the read is keyed by slug and the shorthand "+
			"does not carry one:\n%.300s", svg)
	}
}
