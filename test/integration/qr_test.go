//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/png" // registers the PNG decoder for the .png endpoint's assertions
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// QR codes end to end (M41).
//
// Written against ruleFixture for the reason folders_test.go and split_test.go
// give: it already wires the link service, the dashboard, the redirect path and
// the click pipeline against one database, and a QR code is a thing that spans
// all four — the picture is produced by the first two, and the scan it produces
// is only observable through the other two.
//
// The test that matters most here is TestAScanIsCountedAsAClickFromAQRCode. It
// is the milestone's whole attribution claim, and it is the one assertion that
// fails if any link in the chain — the encoded content, the redirect path's
// parameter read, the recorder, the ingester, the rollup — quietly stops
// carrying the source.

func (f *ruleFixture) qrSVG(linkID uuid.UUID) string {
	f.t.Helper()
	resp := f.get("/api/v1/links/"+linkID.String()+"/qr.svg", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET qr.svg = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != qr.ContentType {
		f.t.Errorf("Content-Type = %q, want %q", ct, qr.ContentType)
	}
	return string(readAll(f.t, resp))
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// --- the named claim ----------------------------------------------------------

// TestAScanIsCountedAsAClickFromAQRCode is the milestone's attribution bullet,
// end to end and through no shortcut: the URL is read out of the code the
// product actually serves, followed as a visitor would follow it, and the
// dimension rollup is asked what it made of the result.
//
// **No new analytics schema**, which is the other half of the bullet. The scan
// arrives in `link_dimension_daily` under the existing `referrer` dimension,
// with the value `qr` beside the `direct` sentinel that column already holds.
func TestAScanIsCountedAsAClickFromAQRCode(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("poster", "https://example.com/summer")

	// What the product says the code encodes. Read from the API rather than
	// rebuilt here, so a change to the parameter breaks this test rather than
	// being copied into it.
	var body struct {
		QR struct {
			Content string `json:"content"`
		} `json:"qr"`
	}
	if err := json.Unmarshal([]byte(f.getJSON("/api/v1/links/"+id.String()+"/qr")), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.QR.Content, domain.ClickSourceParam+"="+domain.ClickSourceQR) {
		t.Fatalf("the code encodes %q, which carries no source parameter; every scan "+
			"would be indistinguishable from a typed URL", body.QR.Content)
	}

	// Follow it the way a camera would: the whole URL out of the picture,
	// nothing added.
	scanned, err := url.Parse(body.QR.Content)
	if err != nil {
		t.Fatal(err)
	}
	target := f.location(scanned.RequestURI(), nil)
	if target != "https://example.com/summer" {
		t.Fatalf("scanning the code went to %q", target)
	}
	waitForClicks(t, f.pool, id, 1)

	// The stored click, read straight from the column so the assertion is not
	// routed through the reader that might be wrong about it.
	var host *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT referrer_host FROM click_events WHERE link_id = $1`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host == nil || *host != domain.ClickSourceQR {
		t.Fatalf("the scan was stored with referrer_host %s, want %q",
			deref(host), domain.ClickSourceQR)
	}

	// And through the rollup, which is what the analytics page reads.
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var clicks int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT coalesce(sum(clicks), 0) FROM link_dimension_daily
		  WHERE link_id = $1 AND dimension = 'referrer' AND value = $2`,
		id, domain.ClickSourceQR).Scan(&clicks); err != nil {
		t.Fatal(err)
	}
	if clicks != 1 {
		t.Errorf("the referrer breakdown holds %d clicks for %q, want 1",
			clicks, domain.ClickSourceQR)
	}
}

// TestAnOrdinaryClickIsStillAttributedToItsReferrer is the other side of the
// same branch. The source replaces the referrer host only when there is one, and
// a link followed from a page must not lose where it came from.
func TestAnOrdinaryClickIsStillAttributedToItsReferrer(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("linked", "https://example.com/x")

	f.location("/linked", map[string]string{"Referer": "https://news.example/story"})
	waitForClicks(t, f.pool, id, 1)

	var host *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT referrer_host FROM click_events WHERE link_id = $1`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host == nil || *host != "news.example" {
		t.Fatalf("referrer_host = %s, want %q", deref(host), "news.example")
	}
}

// TestAnUnknownSourceDoesNotReachTheAnalytics is the cardinality guard, at the
// only layer where it can actually be checked: the row that gets written.
//
// `link_dimension_daily` keys on (link_id, day, dimension, value), so a value a
// visitor chooses is a row a visitor creates. A script appending a fresh `?src=`
// to a popular link would otherwise grow that table without bound.
func TestAnUnknownSourceDoesNotReachTheAnalytics(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("open", "https://example.com/x")

	f.location("/open?src=whatever-i-like", nil)
	waitForClicks(t, f.pool, id, 1)

	var host *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT referrer_host FROM click_events WHERE link_id = $1`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host != nil && *host != "" {
		t.Fatalf("an unrecognised source reached the analytics as %q", *host)
	}
}

// TestTheSourceParameterReachesTheDestination records a decision as a test
// (D76). It is deliberately *not* stripped, unlike the signature parameters
// (M35): a signature is a credential and leaking one hands the destination's
// operator a replayable URL, while a source tag is a label.
func TestTheSourceParameterReachesTheDestination(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("fwd", "https://example.com/landing")
	forward := true
	if _, err := f.links.Update(t.Context(), f.owner, id,
		link.UpdateInput{ForwardQuery: &forward}); err != nil {
		t.Fatal(err)
	}

	got := f.location("/fwd?src=qr", nil)
	if !strings.Contains(got, "src=qr") {
		t.Errorf("query forwarding sent %q; the source parameter is a label rather "+
			"than a credential and is not stripped", got)
	}
}

// --- the picture --------------------------------------------------------------

// TestEveryLinkHasACodeWithoutOneBeingCreated. The endpoint answers for any
// link, which is what m41.md asks for; a style row exists only once somebody has
// expressed a preference.
func TestEveryLinkHasACodeWithoutOneBeingCreated(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("fresh", "https://example.com/x")

	svg := f.qrSVG(id)
	if !strings.HasPrefix(svg, "<svg ") || !strings.Contains(svg, "</svg>") {
		t.Fatalf("the endpoint did not answer with an SVG document: %.80s", svg)
	}
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("%d qr_codes rows exist for a link nobody has styled, want 0", n)
	}

	// Default style, in the picture rather than in a struct.
	if !strings.Contains(svg, `fill="`+qr.DefaultBackground+`"`) {
		t.Error("the default code is not drawn on its default background")
	}
}

// TestAStyleChangesTheDrawingAndNeverTheContent is the promise the QR panel
// makes in so many words. A style that could change what the code says would be
// a control that silently repoints a printed poster.
func TestAStyleChangesTheDrawingAndNeverTheContent(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("styled", "https://example.com/x")

	before := f.qrSVG(id)
	beforeContent := f.qrContent(id)

	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"foreground":"#102030","background":"#f0f0f0","level":"H","margin":6,"scale":12}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT qr = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	after := f.qrSVG(id)
	if after == before {
		t.Fatal("the style was stored and the drawing did not change")
	}
	if !strings.Contains(after, `fill="#102030"`) || !strings.Contains(after, `fill="#f0f0f0"`) {
		t.Error("the stored colours are not in the drawing")
	}
	if got := f.qrContent(id); got != beforeContent {
		t.Errorf("restyling changed what the code says: %q became %q", beforeContent, got)
	}
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("%d qr_codes rows after styling, want exactly 1", n)
	}

	// Styling twice is an update, not a second row — which is what the unique
	// index added by 02700 is for.
	resp2 := f.putJSON("/api/v1/links/"+id.String()+"/qr", `{"style":{"level":"L"}}`)
	_ = resp2.Body.Close()
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("%d qr_codes rows after styling twice, want 1", n)
	}

	// And resetting puts it back to a link with no stored preference.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete,
		f.server.URL+"/api/v1/links/"+id.String()+"/qr", nil)
	del, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE qr = %d", del.StatusCode)
	}
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("%d qr_codes rows after a reset, want 0", n)
	}
}

// TestAnImpossibleStyleIsRefusedRatherThanDrawn. Every value here would produce
// a picture nothing can read, or markup this product did not write.
func TestAnImpossibleStyleIsRefusedRatherThanDrawn(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("refused", "https://example.com/x")

	for _, body := range []string{
		`{"style":{"foreground":"red"}}`,
		`{"style":{"background":"\"/><script>alert(1)</script>"}}`,
		`{"style":{"foreground":"#000000","background":"#000000"}}`,
		`{"style":{"level":"Z"}}`,
		`{"style":{"scale":9000}}`,
		`{"style":{"margin":-1}}`,
	} {
		resp := f.putJSON("/api/v1/links/"+id.String()+"/qr", body)
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s = %d, want 422", body, code)
		}
	}
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("a refused style wrote %d rows", n)
	}
}

// TestACodeIsScopedToItsWorkspace. A link somebody cannot see is a code they
// cannot draw, and the refusal must not confirm the link exists.
func TestACodeIsScopedToItsWorkspace(t *testing.T) {
	f := newRules(t)
	f.claim()

	resp := f.get("/api/v1/links/"+uuid.NewString()+"/qr.svg", nil)
	code := resp.StatusCode
	_ = resp.Body.Close()
	if code != http.StatusNotFound {
		t.Errorf("a code for a link that does not exist = %d, want 404", code)
	}
}

// TestTheDashboardShowsTheCodeInline. The panel renders the SVG in the page
// rather than fetching it, so a page with no network round trip still shows a
// code — and the download link points at the API path, which is also the answer
// to "how do I get this from a script".
func TestTheDashboardShowsTheCodeInline(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("shown", "https://example.com/x")

	page := f.getHTML("/links/" + id.String())
	if !strings.Contains(page, `id="qr"`) {
		t.Fatal("the link page has no QR panel")
	}
	if !strings.Contains(page, "<svg xmlns=") {
		t.Error("the QR panel does not hold a drawing")
	}
	if !strings.Contains(page, "/api/v1/links/"+id.String()+"/qr.svg") {
		t.Error("the QR panel offers no download")
	}
	// The picture must not be escaped into text. `&lt;svg` is what a page that
	// forgot template.HTML looks like, and it renders as a wall of angle
	// brackets rather than as a code.
	if strings.Contains(page, "&lt;svg") {
		t.Error("the drawing was escaped into the page as text")
	}
}

// TestTheStyleFormOnThePageStoresWhatTheAPIWouldStore. Both surfaces call one
// service, which is the inherited rule; this is what proves the form reaches it.
//
// **Rewritten at M49, because the form's vocabulary changed.** It posts two
// colours and one output size in pixels; the quiet zone, the module size and the
// error-correction level are no longer fields on it. What the form stores is
// therefore a *derived* margin and scale, and the assertion is on the size those
// come to rather than on the numbers themselves — the panel's claim is about
// pixels, and pinning the pair here would pin qr.FitSize's answer in a place
// that has no business knowing it.
func TestTheStyleFormOnThePageStoresWhatTheAPIWouldStore(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("formed", "https://example.com/x")

	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#123456"}, "background": {"#fedcba"}, "size": {"400"},
	})
	code := f.qrCode(id)
	if code.Style.Foreground != "#123456" || code.Style.Background != "#fedcba" {
		t.Fatalf("the form stored %+v", code.Style)
	}
	if code.Size < 380 || code.Size > 420 {
		t.Errorf("the form asked for 400px and stored a style that draws %dpx. The snap "+
			"moves the size by less than one module of the picture, not by tens of "+
			"pixels", code.Size)
	}
	if !strings.Contains(f.qrSVG(id), `fill="#123456"`) {
		t.Error("the form's colour is not in the served picture")
	}
	if got := attrOf(t, f.qrSVG(id), `width="(\d+)"`); got != code.Size {
		t.Errorf("the served picture is %dpx wide and the panel reports %dpx", got, code.Size)
	}

	// The level is not on the form and must not be reset by a save that does not
	// mention it. A dashboard that quietly put every code back to M would be
	// undoing a choice only the API can now make.
	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr", `{"style":{"level":"H","scale":10}}`)
	_ = resp.Body.Close()
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#000000"}, "background": {"#ffffff"}, "size": {"300"},
	})
	if got := f.storedQRStyle(id).Level; got != qr.LevelH {
		t.Errorf("a save from the form left the level at %q, want H. The control left "+
			"this surface for the API; a form that answers a question it does not ask "+
			"is worse than one that asks it", got)
	}

	// A size nothing can be printed at is refused rather than clamped, and the
	// refusal keeps the reader on a page with the form on it.
	bad := f.postForm("/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#000000"}, "background": {"#ffffff"}, "size": {"9"},
	})
	status := bad.StatusCode
	_ = bad.Body.Close()
	if status != http.StatusUnprocessableEntity {
		t.Errorf("a 9px code = %d, want 422", status)
	}

	// The reset button is a value of the same form rather than a second one.
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{"reset": {"1"}})
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("%d rows after the reset button, want 0", n)
	}
}

// --- M49: the size, the PNG, and the styles that were already stored ----------

// TestThePNGAndTheSVGAreTheSameCodeOverTheWire is the milestone's matching claim
// at the layer a reader meets it.
//
// internal/qr holds the module-by-module comparison — this is the assertion that
// the two endpoints serve *that*: same size, same content, and the raster one
// carrying the headers a download needs. Reversing D11 is only safe if the file
// somebody saves is the picture they were looking at.
func TestThePNGAndTheSVGAreTheSameCodeOverTheWire(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("poster-png", "https://example.com/summer")
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#000000"}, "background": {"#ffffff"}, "size": {"512"},
	})

	resp := f.get("/api/v1/links/"+id.String()+"/qr.png", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET qr.png = %d", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":           qr.PNGContentType,
		"Cache-Control":          httpx.SVGMaxAge,
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    httpx.PNGDisposition,
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	img, format, err := image.Decode(bytes.NewReader(readAll(t, resp)))
	if err != nil {
		t.Fatalf("the PNG endpoint did not serve a decodable image: %v", err)
	}
	if format != "png" {
		t.Fatalf("the .png path served %q", format)
	}

	size := f.qrCode(id).Size
	if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
		t.Errorf("the PNG is %dx%d and the panel reports %dpx", b.Dx(), b.Dy(), size)
	}
	if got := attrOf(t, f.qrSVG(id), `width="(\d+)"`); got != size {
		t.Errorf("the SVG is %dpx wide and the PNG is %dpx", got, size)
	}
}

// TestAStyleStoredBeforeM49DrawsExactlyWhatItAlwaysDrew is the milestone's
// read-forward bullet, against a row written the way M41 wrote them.
//
// **The blob is inserted by hand rather than through the service**, because the
// claim is about a row that already exists in somebody's database: it holds a
// quiet zone in modules and a scale in pixels per module and no size at all, and
// nothing about M49 may change what it draws.
//
// The strong half is the last one. Opening the panel on such a code shows the
// size its margin and scale come to, and saving without touching the number has
// to be a no-op down to the byte — otherwise every reader who so much as looks
// at an old code resizes it.
func TestAStyleStoredBeforeM49DrawsExactlyWhatItAlwaysDrew(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("inherited", "https://example.com/x")

	// Exactly the JSON M41 marshalled: five keys, and `size` is not one of them.
	const legacy = `{"foreground":"#123a6b","background":"#f5f7fa","level":"Q","margin":4,"scale":10}`
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO qr_codes (id, link_id, workspace_id, style)
		 VALUES ($1, $2, $3, $4::jsonb)`,
		uuid.Must(uuid.NewV7()), id, f.owner.WorkspaceID, legacy); err != nil {
		t.Fatal(err)
	}

	before := f.qrSVG(id)
	code := f.qrCode(id)
	if code.Style.Margin != 4 || code.Style.Scale != 10 || code.Style.Level != qr.LevelQ {
		t.Fatalf("the stored row read back as %+v; a pre-M49 blob is read forward "+
			"unchanged, not normalised into something else", code.Style)
	}
	// The size the panel shows is the one those two numbers already produced.
	if got := attrOf(t, before, `width="(\d+)"`); got != code.Size {
		t.Fatalf("the panel reports %dpx for a code the endpoint draws %dpx wide",
			code.Size, got)
	}

	// And the round trip: the size the panel shows, posted back, changes nothing.
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {code.Style.Foreground}, "background": {code.Style.Background},
		"size": {strconv.Itoa(code.Size)},
	})
	if after := f.qrSVG(id); after != before {
		t.Errorf("saving the size a pre-M49 code already drew changed the picture.\n"+
			"before: %.120s…\nafter:  %.120s…\n\nEvery reader who opens the panel on an "+
			"old code and presses save would resize it", before, after)
	}
	if got := f.storedQRStyle(id); got.Margin != 4 || got.Scale != 10 {
		t.Errorf("the round trip rewrote the stored geometry as margin %d scale %d, "+
			"want 4 and 10", got.Margin, got.Scale)
	}
}

// --- helpers ------------------------------------------------------------------

func (f *ruleFixture) qrRowCount(linkID uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM qr_codes WHERE link_id = $1`, linkID).Scan(&n); err != nil {
		f.t.Fatalf("count qr_codes: %v", err)
	}
	return n
}

func (f *ruleFixture) qrContent(linkID uuid.UUID) string {
	f.t.Helper()
	code, err := f.links.QRCode(f.t.Context(), f.owner, linkID)
	if err != nil {
		f.t.Fatalf("read qr: %v", err)
	}
	return code.Content
}

func (f *ruleFixture) storedQRStyle(linkID uuid.UUID) qr.Style {
	f.t.Helper()
	return f.qrCode(linkID).Style
}

func (f *ruleFixture) qrCode(linkID uuid.UUID) *link.QRCode {
	f.t.Helper()
	code, err := f.links.QRCode(f.t.Context(), f.owner, linkID)
	if err != nil {
		f.t.Fatalf("read qr: %v", err)
	}
	return code
}

// attrOf reads an integer attribute out of a served drawing, for the assertions
// that compare one surface's number against another's.
func attrOf(t *testing.T, svg, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(svg)
	if m == nil {
		t.Fatalf("no match for %s in the drawing:\n%.200s", pattern, svg)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *ruleFixture) putJSON(path, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPut,
		f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *ruleFixture) postQRForm(t *testing.T, path string, vals url.Values) {
	t.Helper()
	resp := f.postForm(path, vals)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303", path, resp.StatusCode)
	}
}

// deref renders a nullable text column for a failure message. Without it a
// pointer prints as an address, which is the one thing a reader cannot act on.
func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return `"` + *s + `"`
}
