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

// More than one code per link, told apart in the analytics (M50).
//
// **The chain this exercises is the milestone.** A named code prints a slug, the
// redirect resolves that slug against the codes the link actually has, the click
// is stored under a value derived from it, the rollup carries it, and the reader
// turns it back into the code's label. Every one of those has to hold or the
// feature is two identical pictures with two names.
//
// The tests below are ordered by what they cost if they break: attribution
// first, then the boundary a hostile parameter is bounded by, then the shape of
// the surfaces.

// TestTwoCodesOnOneLinkAreCountedApart is the milestone's whole claim.
func TestTwoCodesOnOneLinkAreCountedApart(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("twocodes", "https://example.com/summer")

	named := f.createQRCode(t, id, "Autumn poster")
	if named.Slug == "" {
		t.Fatal("a created code carries no slug; nothing in its payload could say which code it is")
	}

	// Scan the default code twice and the named code once, following exactly
	// what each picture encodes rather than a URL rebuilt here.
	f.scan(t, f.codeContent(t, id, ""))
	f.scan(t, f.codeContent(t, id, ""))
	f.scan(t, f.codeContent(t, id, named.Slug))
	waitForClicks(t, f.pool, id, 3)

	// The stored column, read directly so the assertion does not go through the
	// reader that might be wrong about it.
	counts := map[string]int{}
	rows, err := f.pool.Query(t.Context(),
		`SELECT referrer_host, count(*) FROM click_events WHERE link_id = $1 GROUP BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var host *string
		var n int
		if err := rows.Scan(&host, &n); err != nil {
			t.Fatal(err)
		}
		counts[deref(host)] = n
	}
	rows.Close()
	if got := counts[`"`+domain.ClickSourceQR+`"`]; got != 2 {
		t.Errorf("the default code recorded %d scans, want 2 (all of them: %v)", got, counts)
	}
	if got := counts[`"`+domain.ClickSourceCode(named.Slug)+`"`]; got != 1 {
		t.Errorf("the named code recorded %d scans, want 1 (all of them: %v)", got, counts)
	}

	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// And through the reader, which is what the dashboard and the API show. The
	// label comes back with the numbers, because a breakdown keyed on an
	// eight-character slug is a breakdown nobody can read.
	stats := f.linkStats(t, id)
	byLabel := map[string]int64{}
	for _, c := range stats.QRCodes {
		byLabel[c.Slug] = c.Clicks
		if c.Slug == named.Slug && c.Label != "Autumn poster" {
			t.Errorf("the named code came back labelled %q, want %q", c.Label, "Autumn poster")
		}
	}
	if byLabel[""] != 2 || byLabel[named.Slug] != 1 {
		t.Errorf("the per-code breakdown reads %v; want 2 for the default code and 1 for %q",
			byLabel, named.Slug)
	}

	// **The referrers breakdown is unchanged**, which is M41's shipped claim and
	// D76's: every scan is one value beside `direct`, however many codes the
	// link carries. A panel that grew a row per code would be a surface changing
	// shape because a feature the reader is not looking at was used.
	var qrRows, qrClicks int64
	for _, v := range stats.Dimensions["referrer"] {
		if v.Value == domain.ClickSourceQR {
			qrRows++
			qrClicks = v.Clicks
		}
		if strings.HasPrefix(v.Value, domain.ClickSourceQR+":") {
			t.Errorf("the referrers breakdown holds %q; a code's slug is not a referrer, "+
				"and M41 promised one value for every scan", v.Value)
		}
	}
	if qrRows != 1 || qrClicks != 3 {
		t.Errorf("the referrers breakdown holds %d %q rows totalling %d clicks; want one "+
			"row of 3", qrRows, domain.ClickSourceQR, qrClicks)
	}
}

// TestAnUnknownCodeIsAttributedToTheDefaultRatherThanStored is the write-surface
// bound, end to end.
//
// `link_dimension_daily`'s primary key includes the value, so a code parameter a
// visitor could choose would let anybody grow that table a row at a time. The
// closed vocabulary that bounds `src` cannot bound this one — the whole point is
// that the value is workspace data — so the bound is resolution against the
// link's own codes, and this is the assertion that it happens.
func TestAnUnknownCodeIsAttributedToTheDefaultRatherThanStored(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("hostilecode", "https://example.com/x")
	base := f.codeContent(t, id, "")

	for _, raw := range []string{"zzzzzzzz", "evil", strings.Repeat("a", 40), "%2e%2e"} {
		f.scan(t, base+"&"+domain.ClickCodeParam+"="+raw)
	}
	waitForClicks(t, f.pool, id, 4)

	var distinct int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(DISTINCT referrer_host) FROM click_events WHERE link_id = $1`,
		id).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Fatalf("four invented code parameters produced %d distinct stored values; every "+
			"one of them is a permanent row in the dimension rollup", distinct)
	}
	var host *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT DISTINCT referrer_host FROM click_events WHERE link_id = $1`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host == nil || *host != domain.ClickSourceQR {
		t.Fatalf("an unrecognised code was stored as %s, want the default code %q",
			deref(host), domain.ClickSourceQR)
	}
}

// TestADeletedCodeStopsAccumulatingRatherThanBeingReassigned is the risk m50.md
// names and says belongs in the documentation.
//
// A printed code outlives the row that describes it. What must not happen is the
// old picture's scans landing on some *other* code, which would rewrite one
// campaign's numbers with another's traffic.
func TestADeletedCodeStopsAccumulatingRatherThanBeingReassigned(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("retired", "https://example.com/x")
	named := f.createQRCode(t, id, "Spring flyer")
	printed := f.codeContent(t, id, named.Slug)

	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 1)

	resp := f.deleteAPI("/api/v1/links/" + id.String() + "/qr/codes/" + named.Slug)
	if resp != http.StatusNoContent {
		t.Fatalf("deleting the code answered %d, want 204", resp)
	}

	// The same printed picture, scanned after the code is gone.
	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 2)

	counts := map[string]int{}
	rows, err := f.pool.Query(t.Context(),
		`SELECT referrer_host, count(*) FROM click_events WHERE link_id = $1 GROUP BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var host *string
		var n int
		if err := rows.Scan(&host, &n); err != nil {
			t.Fatal(err)
		}
		counts[deref(host)] = n
	}
	rows.Close()
	if counts[`"`+domain.ClickSourceCode(named.Slug)+`"`] != 1 {
		t.Errorf("the deleted code's history reads %v; the scan it earned while it existed "+
			"must still be there", counts)
	}
	if counts[`"`+domain.ClickSourceQR+`"`] != 1 {
		t.Errorf("the scan after the deletion reads %v; it should be recorded as no code, "+
			"which is the link's default", counts)
	}
}

// TestTheDefaultCodesPayloadIsUnchanged is the compatibility claim the whole
// design rests on.
//
// Every picture this product printed before M50 carries `?src=qr` and nothing
// else. If the default code's payload gained an identifier, every one of those
// pictures would start recording as *no code* while new prints recorded as the
// default — one code's history split in two, for a code nobody touched.
func TestTheDefaultCodesPayloadIsUnchanged(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("unchanged", "https://example.com/x")
	f.createQRCode(t, id, "A second code, which must not disturb the first")

	content := f.codeContent(t, id, "")
	if strings.Contains(content, domain.ClickCodeParam+"=") {
		t.Fatalf("the default code now encodes %q; every already-printed picture of this "+
			"link carries the payload without it", content)
	}
	if !strings.HasSuffix(content, domain.ClickSourceParam+"="+domain.ClickSourceQR) {
		t.Fatalf("the default code encodes %q, which is not what M41 shipped", content)
	}
}

// TestALinkWillNotCarryMoreCodesThanItsAnalyticsCanDraw is the cap.
func TestALinkWillNotCarryMoreCodesThanItsAnalyticsCanDraw(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("capped", "https://example.com/x")

	// The default code counts, so the cap is reached one create short of it.
	for i := range domain.MaxQRCodesPerLink - 1 {
		f.createQRCode(t, id, "code "+strconv.Itoa(i))
	}
	resp := f.postJSON("/api/v1/links/"+id.String()+"/qr/codes", `{"label":"one too many"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("the %dth code answered %d, want 422: a link with a thousand codes is a "+
			"link whose analytics page cannot be drawn",
			domain.MaxQRCodesPerLink+1, resp.StatusCode)
	}
}

// TestThePanelListsEveryCodeWithItsOwnDownload is the surface m50.md asks for.
func TestThePanelListsEveryCodeWithItsOwnDownload(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("listed", "https://example.com/x")
	named := f.createQRCode(t, id, "Shop window card")

	page := f.getHTML("/links/" + id.String() + "/qr")
	for _, want := range []string{
		"Shop window card",
		named.Slug,
		"/links/" + id.String() + "/qr/codes/" + named.Slug + "/image.png",
		"/links/" + id.String() + "/qr/codes/" + named.Slug + "/image.svg",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the QR panel does not mention %q; m50.md asks for each code with its "+
				"label, its size and its download", want)
		}
	}
}

// createQRCode adds a named code through the API and returns it.
func (f *ruleFixture) createQRCode(t *testing.T, linkID uuid.UUID, label string) link.QRCode {
	t.Helper()
	body, err := json.Marshal(map[string]any{"label": label})
	if err != nil {
		t.Fatal(err)
	}
	resp := f.postJSON("/api/v1/links/"+linkID.String()+"/qr/codes", string(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("creating a QR code answered %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Code link.QRCode `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Code
}

// codeContent is what one of a link's codes encodes, read over the API rather
// than rebuilt here — so a change to the payload breaks these tests instead of
// being copied into them. qrContent above is its service-layer sibling for the
// default code, which predates named codes.
func (f *ruleFixture) codeContent(t *testing.T, linkID uuid.UUID, slug string) string {
	t.Helper()
	path := "/api/v1/links/" + linkID.String() + "/qr"
	if slug != "" {
		path += "/codes/" + slug
	}
	var body struct {
		QR struct {
			Content string `json:"content"`
		} `json:"qr"`
		Code struct {
			Content string `json:"content"`
		} `json:"code"`
	}
	if err := json.Unmarshal([]byte(f.getJSON(path)), &body); err != nil {
		t.Fatal(err)
	}
	content := body.QR.Content
	if slug != "" {
		content = body.Code.Content
	}
	if content == "" {
		t.Fatalf("GET %s reported no content for the code", path)
	}
	return content
}

// scan follows a code's payload the way a camera would: the whole URL out of the
// picture, nothing added.
func (f *ruleFixture) scan(t *testing.T, content string) {
	t.Helper()
	u, err := url.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	f.location(u.RequestURI(), nil)
}

// linkStats reads the analytics the dashboard and the API read.
func (f *ruleFixture) linkStats(t *testing.T, linkID uuid.UUID) analytics.LinkStats {
	t.Helper()
	var out analytics.LinkStats
	if err := json.Unmarshal(
		[]byte(f.getJSON("/api/v1/links/"+linkID.String()+"/stats")), &out,
	); err != nil {
		t.Fatal(err)
	}
	return out
}

// deleteAPI issues a DELETE and returns the status.
func (f *ruleFixture) deleteAPI(path string) int {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodDelete, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
