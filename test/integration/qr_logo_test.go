//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
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
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
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
// the link's default code has no slug — that absence is its identity (D130) —
// so it is reached through the `/qr` shorthand D133 kept, exactly as `qr.svg`
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
// all: the only path was `/qr/codes/{slug}`, and the default code's identity is
// the *absence* of a slug (D130). The shorthand `/qr/logo` is what reaches it,
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
	// Within half a span of the size it had. The fit snaps to a whole number of
	// modules, so the two need not be equal; with the quiet zone pinned at its
	// floor since the M49 reopening (F213) the sizes it can produce step by one
	// span — symbol plus both quiet zones — per unit of scale, so the nearest
	// can sit half of one away, and anything inside that is the snap rather
	// than a resize nobody asked for. The span is read back off the answer:
	// size ÷ scale is the module count including the quiet zone, which is the
	// one number this test does not otherwise have.
	span := after.Size / after.Style.Scale
	if diff := after.Size - untouched.Size; 2*diff > span || 2*diff < -span {
		t.Errorf("the code was %dpx and is %dpx after the upload (%+v → %+v). Level H "+
			"is a larger symbol and the re-fit is what keeps the size somebody "+
			"chose — half a %d-module span is %dpx", untouched.Size, after.Size,
			untouched.Style, after.Style, span, span/2)
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

	// Deliberately at L to begin with, so H is never the level it already had.
	if resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"level":"L"}}`); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("setting the level to L = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	if got := f.apiQRLevel(t, id); got != qr.LevelL {
		t.Fatalf("the code is at level %q before the upload, want L", got)
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
	// Half a span, which is the whole of what a snap may move since the M49
	// reopening pinned the quiet zone at its floor (F213): the achievable sizes
	// step by one span — the symbol plus both quiet zones — per unit of scale.
	// A fit taken against L's smaller symbol misses by far more than that,
	// which is what this test is looking for.
	span := atH.Size + 2*stored.Margin
	if diff := drawn - want; 2*diff > span || 2*diff < -span {
		t.Errorf("the reader asked for %dpx and the served picture is %dpx, with "+
			"margin %d and scale %d on a %d-module symbol. qr.FitSize snaps to a whole "+
			"number of modules, so anything inside half a %d-module span (%dpx) is the "+
			"snap; this is the fit having been taken against the %d modules the row's "+
			"level encodes to instead", want, drawn, stored.Margin, stored.Scale,
			atH.Size, span, span/2, atL.Size)
	}
	// And the panel's own number is the same number, so the two surfaces do not
	// disagree about a picture they both describe.
	if got := f.qrCode(id).Size; got != drawn {
		t.Errorf("the panel reports %dpx and the endpoint draws %dpx", got, drawn)
	}
}
