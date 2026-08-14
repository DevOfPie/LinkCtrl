//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
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

	// And resetting puts the style back to the default. **The row stays**, which
	// is D183's change to this operation: it used to hold nothing but the
	// preference being withdrawn, and it now holds the code's identity — the
	// flag that says untagged scans resolve through it, and the slug printed in
	// its payload once the link has a second code. Deleting those to clear two
	// colours and a size would retire a printed identity to undo a styling.
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
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("%d qr_codes rows after a reset, want the code's own 1", n)
	}
	if style := f.qrCode(id).Style; style.Foreground != qr.DefaultForeground ||
		style.Background != qr.DefaultBackground {
		t.Errorf("the code reads back at %+v after a reset; the row survives the "+
			"reset now, so the style being cleared is what has to be asserted", style)
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

// TestTheAPIRefusesASizeTheSymbolHasOutgrown is the other door onto the claim
// D182 rests on: *the requested size is the size stored and drawn, exactly.*
//
// **The openapi contract promises this and the schema cannot keep it.** `size`
// is bounded there at 64 to 2048, which is the size *control's* range, and the
// floor that actually binds is `scale × (module count + 6)` — a property of the
// content and of the style together, so no schema and no `Style.Normalize` can
// see it. Without a check at the service the endpoint would accept a size the
// symbol has outgrown, `qr.Code.geometry` would fall back to the older
// margin-and-scale arithmetic, and `GET …/qr.png` would serve a picture
// measuring something else entirely. The dashboard's size control has refused
// this since the reopening; the API is the second surface that stores a size and
// it was the one without it.
//
// Both floors are read off the link's own content rather than written down, so
// an alias length or a `QRContent` change moves the fixture instead of stranding
// it on a stale number.
func TestTheAPIRefusesASizeTheSymbolHasOutgrown(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink(strings.Repeat("a", 48), "https://example.com/x")

	// The level and scale a `size`-only style normalizes to, which is what the
	// requests below are judged against.
	defaults, _ := qr.Style{}.Normalize()
	symbol, err := qr.Encode(f.qrContent(id), defaults.Level)
	if err != nil {
		t.Fatal(err)
	}
	floor := qr.MinSizeForStyle(symbol.Size, defaults)
	if floor <= qr.MinSize {
		t.Fatalf("this link's code is %d modules and its floor at scale %d is %dpx, "+
			"at or under the %dpx range floor — there is no size between the two "+
			"for the endpoint to refuse, so the fixture is measuring nothing",
			symbol.Size, defaults.Scale, floor, qr.MinSize)
	}

	// Inside the schema's range and below this style's floor: the one band only
	// the endpoint can judge.
	body := `{"style":{"size":` + strconv.Itoa(qr.MinSize) + `}}`
	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr", body)
	payload, _ := io.ReadAll(resp.Body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusUnprocessableEntity {
		t.Errorf("PUT %s on a %d-module code = %d, want 422: the openapi contract "+
			"says a size below %dpx is refused rather than squeezed, and a stored "+
			"size the symbol has outgrown draws at some other number of pixels",
			body, symbol.Size, status, floor)
	}
	// The refusal names the floor that bound, the way the dashboard's does. A 422
	// saying only "out of range" would point at 64, which this request satisfies.
	// It names the narrowest-module floor beside it, because lowering `scale` is
	// the other way to be accepted.
	for _, want := range []string{strconv.Itoa(floor), strconv.Itoa(qr.MinSizeFor(symbol.Size))} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("the refusal does not name %spx, so nothing in it says what "+
				"would be accepted:\n%.400s", want, payload)
		}
	}
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("a refused size wrote %d rows", n)
	}

	// And the floor itself is accepted, so the refusal is a floor rather than a
	// band the endpoint simply will not serve.
	ok := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"size":`+strconv.Itoa(floor)+`}}`)
	status = ok.StatusCode
	_ = ok.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("PUT at the floor of %dpx = %d, want 200", floor, status)
	}
	if got := f.qrCode(id).Size; got != floor {
		t.Errorf("the code stores %dpx after a request for %dpx; when size is set it "+
			"is exactly what the picture measures", got, floor)
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

// TestTheDashboardShowsTheCodeInline. The page renders the SVG rather than
// fetching it, so a page with no network round trip still shows a code — and
// the download link points at the API path, which is also the answer to "how
// do I get this from a script".
//
// Two fetches since the F212 reopening: the landing tab carries the heading
// thumbnail, and the QR tab carries the full drawing and the downloads, in
// flow where the page-level popup used to hold them.
func TestTheDashboardShowsTheCodeInline(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("shown", "https://example.com/x")

	page := f.getHTML("/links/" + id.String())
	if !strings.Contains(page, "<svg xmlns=") {
		t.Error("the landing tab draws no thumbnail in the heading row")
	}

	qrTab := f.getHTML("/links/" + id.String() + "?tab=qr")
	if !strings.Contains(qrTab, `id="qr"`) {
		t.Fatal("the QR tab has no QR section")
	}
	if !strings.Contains(qrTab, "/api/v1/links/"+id.String()+"/qr.svg") {
		t.Error("the QR tab offers no download")
	}
	// The picture must not be escaped into text. `&lt;svg` is what a page that
	// forgot template.HTML looks like, and it renders as a wall of angle
	// brackets rather than as a code.
	for name, p := range map[string]string{"landing tab": page, "QR tab": qrTab} {
		if strings.Contains(p, "&lt;svg") {
			t.Errorf("the drawing was escaped into the %s as text", name)
		}
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
	// **Exactly 400, and the allowance is gone** (D182, F221). This asserted a
	// half-span tolerance while the fit could only land on sizes the module grid
	// admitted; the quiet zone is carried in pixels now, so the number the form
	// posted is the number the row draws — end to end, through the handler and
	// the service and back out of the database.
	if code.Size != 400 {
		t.Errorf("the form asked for 400px and stored a style that draws %dpx. The "+
			"size set is where it stays — the owner's sentence, and the whole of "+
			"why this milestone was reopened", code.Size)
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

	// The reset button is a value of the same form rather than a second one. It
	// clears the style and leaves the row, which is what carries the code's
	// identity since D183.
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{"reset": {"1"}})
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("%d rows after the reset button, want the code's own 1", n)
	}
	if style := f.qrCode(id).Style; style.Foreground != qr.DefaultForeground {
		t.Errorf("the reset button left the code at %+v", style)
	}
}

// TestTheSliderAndTheNumberResolveToOneSize is the size control's second input,
// driven through the handler rather than asserted of the function that decides
// (D182, F221).
//
// **The rule lives on the server**: `size_shown` is what the form was rendered
// with, the slider wins when it has moved off that, and the number wins
// otherwise. Both directions are driven, because a rule that only ever took one
// of them would pass a test for either alone — and so is the form with no slider
// at all, which is what an API-shaped post and a page cached from before this
// reopening look like.
//
// **This comment said nothing in the browser keeps the two in step, and M50.8
// made that false**: `static/js/qr-size.js` mirrors whichever input moved. The
// rule is not redundant for it and this test is not weaker — it is the
// script-blocked path, it is what an API-shaped post meets, and it is what
// decides a typed size the slider deliberately did not follow because the value
// is outside its range.
func TestTheSliderAndTheNumberResolveToOneSize(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("sliding", "https://example.com/x")
	path := "/links/" + id.String() + "/qr"
	colours := func(v url.Values) url.Values {
		v.Set("foreground", "#123456")
		v.Set("background", "#fedcba")
		return v
	}

	// The slider moved and the number did not: the slider is what the reader
	// touched, so the number they never looked at must not overrule it.
	f.postQRForm(t, path, colours(url.Values{
		"size_shown": {"300"}, "size_slider": {"512"}, "size": {"300"},
	}))
	if got := f.qrCode(id).Size; got != 512 {
		t.Errorf("the slider was dragged to 512 and the code is %dpx. The number box "+
			"still held the value the page was rendered with, and a rule that lets "+
			"that win is a slider nobody can use", got)
	}

	// The number was typed into and the slider was not: the mirror case.
	f.postQRForm(t, path, colours(url.Values{
		"size_shown": {"512"}, "size_slider": {"512"}, "size": {"640"},
	}))
	if got := f.qrCode(id).Size; got != 640 {
		t.Errorf("640 was typed into the number box and the code is %dpx. The slider "+
			"had not moved, so it had nothing to say", got)
	}

	// No slider on the form at all — the shape M49 shipped, and the shape a
	// cached page still posts.
	f.postQRForm(t, path, colours(url.Values{"size": {"256"}}))
	if got := f.qrCode(id).Size; got != 256 {
		t.Errorf("a form carrying only the number stored %dpx, want 256", got)
	}
}

// TestAQRWriteReturnsToTheQRTab is the reopened M47's panel-return bullet,
// driven against the running stack.
//
// The link page draws one section panel at a time now, so one URL serves seven
// views and M48's `next` field has nothing to tell them apart with. The chosen
// mechanism (D178) is re-derivation: every write this panel routes is QR work,
// so the handlers append tab=qr themselves — to the redirect on success, to the
// rendered page on a refusal — and `next` stays exactly the two-value choice it
// shipped as. Both halves are asserted, because a refusal that landed on the
// strip's landing tab would show an error banner over a form it does not
// describe, with the section that explains it a click away.
func TestAQRWriteReturnsToTheQRTab(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("tabbed", "https://example.com/x")
	detail := "/links/" + id.String()

	// A save from the panel opened over the link page lands on the QR tab.
	resp := f.postForm(detail+"/qr", url.Values{
		"foreground": {"#123456"}, "background": {"#fedcba"}, "size": {"400"},
		"next": {detail},
	})
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a panel save = %d, want 303", resp.StatusCode)
	}
	if !strings.HasPrefix(loc, detail+"?") || !strings.Contains(loc, "tab=qr") {
		t.Errorf("a save from the panel over the link page redirected to %q; want "+
			"that page open on the QR tab", loc)
	}

	// A refusal comes back as the link page open on the same tab, with the QR
	// section — the surface that renders the error — actually drawn.
	bad := f.postForm(detail+"/qr", url.Values{
		"foreground": {"#000000"}, "background": {"#ffffff"}, "size": {"9"},
		"next": {detail},
	})
	body := string(readAll(t, bad))
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a 9px code = %d, want 422", bad.StatusCode)
	}
	if !strings.Contains(body, `?tab=qr" aria-current="page"`) {
		t.Error("the refusal did not land on the QR tab")
	}
	// The section's own heading rather than its opening sentence. That sentence
	// was the marker until M50.7 rewrote it to reach the tab's prose bound
	// (D188), and a marker made of prose is a marker any edit to the prose
	// breaks — which is what happened, on a claim about redirects that has
	// nothing to do with wording.
	if !strings.Contains(body, ">QR code</h2>") {
		t.Error("the refusal's page does not draw the QR section beside the error")
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
	// **The default code's own slug, because it has one now** (D183). Creating
	// the second code is what gave it one — that is the moment it stopped being
	// alone — so its picture carries a tag and its scans record under it. The
	// untagged payload every picture printed before that moment carries is
	// asserted separately, by TestAPrintedPictureWithNoCodeStillLandsOnTheDefault.
	def := f.qrCode(id)
	if def.Slug == "" {
		t.Fatal("the default code has no slug on a link that carries two codes; " +
			"a code gains one when it stops being alone (D183)")
	}
	if got := counts[`"`+domain.ClickSourceCode(def.Slug)+`"`]; got != 2 {
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
	if byLabel[def.Slug] != 2 || byLabel[named.Slug] != 1 {
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
func TestTheDefaultCodesPayloadIsUnchangedUntilItStopsBeingAlone(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("unchanged", "https://example.com/x")

	// A link's only code, styled. **Restyling never changes what a code says**,
	// which is M41's claim and the reason a slug is not handed out here: a
	// preference about colours that silently rewrote a printed payload would be
	// the shape D183 exists to remove rather than one to add.
	styled := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"foreground":"#123a6b","background":"#f5f7fa"}}`)
	if styled.StatusCode != http.StatusOK {
		t.Fatalf("styling the only code = %d", styled.StatusCode)
	}
	_ = styled.Body.Close()
	content := f.codeContent(t, id, "")
	if strings.Contains(content, domain.ClickCodeParam+"=") {
		t.Fatalf("a link's only QR code encodes %q; it has nothing to be told apart "+
			"from, and every picture of it already printed carries the payload "+
			"without a tag", content)
	}
	if !strings.HasSuffix(content, domain.ClickSourceParam+"="+domain.ClickSourceQR) {
		t.Fatalf("the default code encodes %q, which is not what M41 shipped", content)
	}

	// And the moment it stops being alone it gains one, because that is when a
	// tag starts telling something apart (D183). This is the one moment the
	// picture changes, and it is what *"every code should have a qrc tag on its
	// link"* asked for.
	f.createQRCode(t, id, "A second code")
	tagged := f.codeContent(t, id, "")
	if !strings.Contains(tagged, domain.ClickCodeParam+"=") {
		t.Fatalf("the default code still encodes %q on a link that now carries two "+
			"codes; without a tag it cannot be told apart from the one beside it, "+
			"and it is the code the owner could not remove", tagged)
	}
	if def := f.qrCode(id); !strings.HasSuffix(tagged, domain.ClickCodeParam+"="+def.Slug) {
		t.Fatalf("the default code encodes %q, which does not end in its own slug %q",
			tagged, def.Slug)
	}
}

// TestAPrintedPictureWithNoCodeStillLandsOnTheDefault is the assertion D183
// names as the one that matters: *a pre-migration picture still attributes where
// it always did*.
//
// **Two payloads for one code, and the whole of the reopening's safety is that
// they meet.** Every picture this product drew before per-code identity existed
// carries `?src=qr` and nothing else; the same code's picture, downloaded after
// it gained a slug, carries the tag. The first records the bare `qr` it has
// recorded since M41 — nothing on the redirect path changed and nothing already
// recorded was rewritten — and the breakdown counts that bucket against
// whichever code holds the flag. So both land on one row.
//
// The alternative was resolving the flag on the redirect path and storing
// `qr:<slug>` for an untagged scan, which splits every link's existing history
// at the migration: everything printed before under one name and everything
// after under another, for a code nobody touched. That is the split D130 spent a
// milestone avoiding, and it is not reopened here.
func TestAPrintedPictureWithNoCodeStillLandsOnTheDefault(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("printed", "https://example.com/x")

	// The picture as it was printed, before this link had any code identities:
	// the short URL and the source parameter, and nothing else.
	printed := f.codeContent(t, id, "")
	if strings.Contains(printed, domain.ClickCodeParam+"=") {
		t.Fatalf("the fixture is not a pre-identity picture: %q", printed)
	}

	// Now the link grows a second code, which is what gives the first one a slug
	// and a tagged picture of its own.
	f.createQRCode(t, id, "Autumn poster")
	def := f.qrCode(id)
	if def.Slug == "" {
		t.Fatal("the default code gained no slug when a second code appeared")
	}

	// One scan off the printed picture, one off the picture served today.
	f.scan(t, printed)
	f.scan(t, def.Content)
	waitForClicks(t, f.pool, id, 2)

	// They are stored under two values, and that is expected: the redirect path
	// records what the payload says, unchanged by this reopening.
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
	if counts[`"`+domain.ClickSourceQR+`"`] != 1 {
		t.Errorf("the printed picture's scan reads %v; it carries no code parameter, "+
			"so it records the bare %q exactly as it did before this reopening — "+
			"nothing on the redirect path changed", counts, domain.ClickSourceQR)
	}
	if counts[`"`+domain.ClickSourceCode(def.Slug)+`"`] != 1 {
		t.Errorf("the tagged picture's scan reads %v, want one under %q",
			counts, domain.ClickSourceCode(def.Slug))
	}

	// And they meet in the breakdown, on the default code's one row.
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stats := f.linkStats(t, id)
	var onDefault int64
	for _, c := range stats.QRCodes {
		if c.Slug == def.Slug {
			onDefault = c.Clicks
		}
		if c.Slug == "" {
			t.Errorf("the breakdown holds a row with no slug and %d clicks; the "+
				"untagged bucket belongs to the code that is the default, not to a "+
				"row of its own", c.Clicks)
		}
	}
	if onDefault != 2 {
		t.Errorf("the default code's row reads %d, want both scans: the picture "+
			"printed before it had a slug and the one printed after are the same "+
			"code, and a reader who split them in two would see a poster's history "+
			"halve on the day the link grew a second code (%v)", onDefault, stats.QRCodes)
	}
}

// TestAddingACodeReFitsEveryRowItsPayloadChanged is M49's third reopening at the
// one call site that changes a payload (F225, F226, D185).
//
// **The arithmetic is asserted in `internal/link`; what this asserts is that the
// operation performs it, on both rows, through the surfaces a reader uses.** The
// unit test measures `refitForPayload` over a payload it builds itself. Here the
// slug is the one the product generates, the style goes in through the API and
// comes back out of the drawing, and the number the picture measures is read off
// the served SVG rather than recomputed — so a re-fit that stored the right
// number and drew a different one still fails.
//
// The fixture puts the default code at **its own floor**, which is where both
// findings measured the defect and the only place the size cannot be kept. A
// code anywhere else on the slider keeps its size and moves only its scale,
// which is what makes the notice worth reading; that half is the unit test's.
func TestAddingACodeReFitsEveryRowItsPayloadChanged(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("refitted", "https://example.com/x")

	// The floor for this link's untagged payload, at the narrowest module the
	// product admits. Sent with `scale`, because an API caller sets both and the
	// endpoint's floor is the one at the scale it was given.
	before, err := qr.Encode(f.codeContent(t, id, ""), qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	floor := qr.MinSizeFor(before.Size)
	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		fmt.Sprintf(`{"style":{"size":%d,"scale":%d}}`, floor, qr.MinScale))
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("storing the floor size answered %d: %s", resp.StatusCode, raw)
	}
	_ = resp.Body.Close()
	if got := attrOf(t, f.qrSVG(id), `width="(\d+)"`); got != floor {
		t.Fatalf("the code stored at its floor draws %dpx, want %d; the fixture is "+
			"not at the floor and this test measures nothing", got, floor)
	}

	// Adding a code gives the default one a printed identity, which lengthens
	// what it encodes. Both rows are re-fitted, and the response says so because
	// this is the case where the size could not be kept.
	body, err := json.Marshal(map[string]any{"label": "Autumn poster"})
	if err != nil {
		t.Fatal(err)
	}
	created := f.postJSON("/api/v1/links/"+id.String()+"/qr/codes", string(body))
	raw := readAll(t, created)
	_ = created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("creating a QR code answered %d: %s", created.StatusCode, raw)
	}
	var out struct {
		Code  link.QRCode `json:"code"`
		Refit *struct {
			From int `json:"from"`
			To   int `json:"to"`
		} `json:"refit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}

	after, err := qr.Encode(out.Code.Content, qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size <= before.Size {
		t.Fatalf("the tagged payload is %d modules and the untagged one %d; if the "+
			"tag does not grow the symbol there is nothing here to re-fit",
			after.Size, before.Size)
	}
	want := qr.MinSizeFor(after.Size)

	if out.Refit == nil {
		t.Fatalf("the create answered with no `refit` after raising a size from %d "+
			"to %d. A client that configured this code to a size is told when the "+
			"product could not keep it — that is the whole of what it is told",
			floor, want)
	}
	if out.Refit.From != floor || out.Refit.To != want {
		t.Errorf("`refit` reads %+v, want from %d to %d", *out.Refit, floor, want)
	}

	// Both codes: the row says one number and the picture measures the same one.
	// The default is the code the reader already had and never touched, which is
	// the half F226 is about.
	def := f.qrCode(id)
	for _, c := range []struct {
		what  string
		slug  string
		style qr.Style
		size  int
	}{
		{"the default code", def.Slug, def.Style, def.Size},
		{"the code just created", out.Code.Slug, out.Code.Style, out.Code.Size},
	} {
		if c.style.Size != want || c.size != want {
			t.Errorf("%s stores size %d and reports %d, want %d — the smallest size "+
				"its own symbol can be drawn at now that its payload carries a tag",
				c.what, c.style.Size, c.size, want)
		}
		svg := f.getBody("/api/v1/links/" + id.String() + "/qr/codes/" + c.slug + "/image.svg")
		if got := attrOf(t, svg, `width="(\d+)"`); got != c.size {
			t.Errorf("%s says %dpx and its picture is %dpx wide. A stored size that "+
				"is not the drawn size is the defect the second reopening closed and "+
				"this one closes again on the path that reopened it",
				c.what, c.size, got)
		}
	}

	// And nothing already printed changed what it says: the untagged picture is
	// still a payload the redirect path resolves to this link.
	if strings.Contains(f.codeContent(t, id, ""), domain.ClickCodeParam+"=") !=
		(def.Slug != "") {
		t.Errorf("the default code's payload and its slug disagree: %q against %q",
			f.codeContent(t, id, ""), def.Slug)
	}
}

// TestAStaleStoredSizeIsRepairedByTheNextCreate is the case the guard on the
// re-fit exists for, and the one an instance upgrading into this release is
// actually in (D185).
//
// **A row can already be stale**, because the rule this milestone adds did not
// exist when the row was written: every link that grew a second code under the
// previous release carries a default whose `size` was fitted before it had a
// tag. Nothing raises it until something touches the codes, and the create is
// the thing that does — so it re-fits the row it reads whether or not *this*
// call is what changed the payload, and the write is skipped entirely when the
// row is already right.
//
// Without that the code created here would inherit the stale number and be born
// with the defect, which is F225 written exactly as F225 is written.
func TestAStaleStoredSizeIsRepairedByTheNextCreate(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("stale", "https://example.com/x")

	// A link with two codes, the shape the previous release left behind.
	f.createQRCode(t, id, "First")
	def := f.qrCode(id)
	if def.Slug == "" {
		t.Fatal("the default code gained no slug, so this fixture is not the shape")
	}

	// Now put its row back into the state that release wrote: a size fitted
	// against the payload it had *before* the tag, at the narrowest module.
	untagged, err := qr.Encode(link.QRContent(def.Content[:strings.Index(
		def.Content, "&"+domain.ClickCodeParam+"=")], ""), qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	stale := qr.MinSizeFor(untagged.Size)
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE qr_codes SET style = style || $2::jsonb WHERE link_id = $1 AND slug = $3`,
		id, fmt.Sprintf(`{"size":%d,"scale":%d,"margin":%d}`,
			stale, qr.MinScale, qr.DefaultMargin), def.Slug); err != nil {
		t.Fatal(err)
	}
	tagged, err := qr.Encode(def.Content, qr.DefaultLevel)
	if err != nil {
		t.Fatal(err)
	}
	want := qr.MinSizeFor(tagged.Size)
	if want <= stale {
		t.Fatalf("the tagged payload's floor is %d and the untagged one's %d; the "+
			"row this seeds is not stale and the test measures nothing", want, stale)
	}

	// Adding a third code reads that row, finds it stale, and repairs it — and
	// the code it creates is fitted rather than inheriting the number.
	added := f.createQRCode(t, id, "Second")
	if added.Style.Size != want || added.Size != want {
		t.Errorf("the created code stores %d and reports %d, want %d. It copies the "+
			"style the link is already drawing at, and copying a stale one is F225 "+
			"in as many words", added.Style.Size, added.Size, want)
	}
	if now := f.qrCode(id); now.Style.Size != want || now.Size != want {
		t.Errorf("the stale default row still stores %d and reports %d, want %d. The "+
			"re-fit is asked for on every create precisely so a row left stale by an "+
			"earlier release is repaired rather than waiting to be noticed",
			now.Style.Size, now.Size, want)
	}
	for _, slug := range []string{def.Slug, added.Slug} {
		svg := f.getBody("/api/v1/links/" + id.String() + "/qr/codes/" + slug + "/image.svg")
		if got := attrOf(t, svg, `width="(\d+)"`); got != want {
			t.Errorf("code %s draws %dpx against a stored %d", slug, got, want)
		}
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

// --- M50's reopening: the default is a flag (D183, F222) ----------------------

// TestAnyCodeCanBeRemovedAndTheLastCannot is the owner's report, end to end.
//
// *"As long as there are multiple QR codes any of them should be able to be
// removed, currently the first one cannot be removed."* The first one could not
// be removed because the default *was* the row with the empty slug, so removing
// it would have left every already-printed picture resolving to nothing. The
// refusal moves off identity and onto arithmetic: a link always has a code, so
// what cannot go is whichever one is last.
func TestAnyCodeCanBeRemovedAndTheLastCannot(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("removable", "https://example.com/x")
	named := f.createQRCode(t, id, "Autumn poster")
	def := f.qrCode(id)

	// The default, removed — which is the whole of what was asked for.
	if got := f.deleteAPI("/api/v1/links/" + id.String() + "/qr/codes/" + def.Slug); got != http.StatusNoContent {
		t.Fatalf("removing the default code answered %d, want 204: it is the code "+
			"the owner reported could not be removed (F222)", got)
	}
	codes := f.listQRCodes(t, id)
	if len(codes) != 1 || codes[0].Slug != named.Slug {
		t.Fatalf("the link carries %+v after removing its default, want only %q",
			codes, named.Slug)
	}

	// And the one left cannot go, with a reason rather than a 404: the code is
	// there, and a link without one is not a state this product has.
	status, body := f.deleteAPIBody("/api/v1/links/" + id.String() + "/qr/codes/" + named.Slug)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("removing a link's last QR code answered %d, want 422 — the code "+
			"exists, so a 404 would be a lie about why", status)
	}
	if !strings.Contains(body, "only QR code") {
		t.Errorf("the refusal does not say why the last code stays:\n%.400s", body)
	}
	if n := f.qrRowCount(id); n != 1 {
		t.Errorf("%d rows after a refused removal, want the one that was refused", n)
	}
}

// TestRemovingTheDefaultPromotesTheOldestCodeLeft is the milestone's promotion
// bullet, and the decision it left to the build.
//
// **Oldest, and the test is what pins it.** The three candidates were oldest,
// first-in-list and the one the reader was looking at; only the first is a
// property of the data rather than of a surface, so the API and the dashboard
// promote the same code from the same delete. The link here carries three codes
// so that "oldest" and "any code at all" are different answers.
//
// The second half is what makes the promotion matter: untagged scans follow the
// flag, so a picture with no code on it lands on the promoted code afterwards.
func TestRemovingTheDefaultPromotesTheOldestCodeLeft(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("promoted", "https://example.com/x")

	// The picture as printed before any of this link's codes had identities.
	printed := f.codeContent(t, id, "")

	first := f.createQRCode(t, id, "Autumn poster")
	second := f.createQRCode(t, id, "Shop window")
	def := f.qrCode(id)
	if def.Slug == first.Slug || def.Slug == second.Slug {
		t.Fatalf("the default resolved to a created code (%q)", def.Slug)
	}

	if got := f.deleteAPI("/api/v1/links/" + id.String() + "/qr/codes/" + def.Slug); got != http.StatusNoContent {
		t.Fatalf("removing the default answered %d", got)
	}
	promoted := f.qrCode(id)
	if promoted.Slug != first.Slug {
		t.Fatalf("the default is now %q; removing the flag-holder promotes the "+
			"oldest code left, which is %q — the newer one was %q",
			promoted.Slug, first.Slug, second.Slug)
	}
	if !promoted.Default {
		t.Error("the promoted code does not report itself as the default")
	}

	// The printed picture still works and still carries no code, and its scan is
	// now counted against the code that was promoted. That is what makes the
	// promotion worth stating to a reader rather than performing silently.
	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 1)
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.linkStats(t, id).QRCodes {
		if c.Slug == promoted.Slug && c.Clicks != 1 {
			t.Errorf("the promoted code's row reads %d, want the untagged scan's 1", c.Clicks)
		}
	}
}

// TestAnyCodeCanBeMadeTheDefault is D183's first limb: *any code can be set as
// the default, and the default is what an untagged scan resolves through.*
//
// Asserted through where a scan lands rather than through the flag alone,
// because the flag is only worth having for what it decides.
func TestAnyCodeCanBeMadeTheDefault(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("settable", "https://example.com/x")
	printed := f.codeContent(t, id, "")
	named := f.createQRCode(t, id, "Autumn poster")

	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr/codes/"+named.Slug+"/default", "")
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("setting the default answered %d, want 200", status)
	}
	if got := f.qrCode(id); got.Slug != named.Slug {
		t.Fatalf("the /qr shorthand answers for %q, want the code just made the "+
			"default (%q) — the shorthand means the role", got.Slug, named.Slug)
	}

	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 1)
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.linkStats(t, id).QRCodes {
		if c.Slug == named.Slug && c.Clicks != 1 {
			t.Errorf("a picture carrying no code recorded %d scans against the code "+
				"that is now the default, want 1 — that is what the flag decides",
				c.Clicks)
		}
	}
}

// TestRestoreDefaultsActsOnTheCodeThatIsSelected is the second defect the
// reopening carries: *Restore defaults takes no slug, so pressing it while a
// named code is selected clears the default code's style.*
//
// Driven through the form, because the form is where the defect was: the button
// posts the panel's `code` field like every other control on it, and the handler
// had been dropping it.
func TestRestoreDefaultsActsOnTheCodeThatIsSelected(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("scoped", "https://example.com/x")
	named := f.createQRCode(t, id, "Autumn poster")

	// Two codes, both restyled away from the default, so clearing the wrong one
	// is visible in either direction.
	for _, slug := range []string{"", named.Slug} {
		f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
			"code": {slug}, "foreground": {"#123a6b"}, "background": {"#f5f7fa"},
			"size": {"512"},
		})
	}

	// Restore defaults, with the named code selected.
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"code": {named.Slug}, "reset": {"1"},
	})

	if got := f.qrCodeBySlug(t, id, named.Slug); got.Style.Foreground != qr.DefaultForeground {
		t.Errorf("the selected code reads back at %q; Restore defaults is scoped to "+
			"the selection, and it was the control that was not (F222)",
			got.Style.Foreground)
	}
	if got := f.qrCode(id); got.Style.Foreground != "#123a6b" {
		t.Errorf("the default code's style was cleared by a button pressed on "+
			"another code: it reads %q. That is the defect, not the fix",
			got.Style.Foreground)
	}
}

// TestOneSaveWritesTheNameAndTheStyle is M50.7's half of F224(g), owner-set:
// *"the 'Rename' button should be removed with the 'Save the style' button
// taking up all saving functions."*
//
// **Driven through the form, because the form is what changed.** The panel used
// to carry two submits for two halves of one row, so a reader who edited a
// colour and a name together pressed twice — or pressed once and watched one of
// their edits vanish. There is one control now, and this is the assertion that
// it carries both halves rather than the name having simply stopped being
// editable.
//
// The third case is the guard, and it is the one worth the lines: a post that
// carries **no** `label` key at all must not blank a code's name. That is what a
// page cached from before this milestone sends, and what any script shaped like
// the old style call sends; reading an absent field as "clear it" would erase
// names nobody touched. The handler tests the key's presence rather than the
// value's emptiness, and the second case below is what says an *empty* value
// still means what it looks like.
func TestOneSaveWritesTheNameAndTheStyle(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("onesave", "https://example.com/x")
	named := f.createQRCode(t, id, "Autumn poster")
	panel := "/links/" + id.String() + "/qr"

	// One press, both halves.
	f.postQRForm(t, panel, url.Values{
		"code": {named.Slug}, "label": {"Winter poster"},
		"foreground": {"#123a6b"}, "background": {"#f5f7fa"}, "size": {"512"},
	})
	got := f.qrCodeBySlug(t, id, named.Slug)
	if got.Label != "Winter poster" {
		t.Errorf("the code reads back named %q after a save carrying a changed "+
			"name, want %q. One control saves this form now, so a name it did "+
			"not write is a name the reader believes they saved (F224g)",
			got.Label, "Winter poster")
	}
	if got.Style.Foreground != "#123a6b" {
		t.Errorf("the code reads back at %q; the style half of the same save did "+
			"not land", got.Style.Foreground)
	}

	// An empty value is somebody clearing the box, and clearing it is an edit.
	f.postQRForm(t, panel, url.Values{
		"code": {named.Slug}, "label": {""},
		"foreground": {"#123a6b"}, "background": {"#f5f7fa"}, "size": {"512"},
	})
	if got := f.qrCodeBySlug(t, id, named.Slug); got.Label != "" {
		t.Errorf("a save carrying an empty name left %q on the code. The box is "+
			"the control, and emptying it is what a reader means by removing a "+
			"name", got.Label)
	}

	// And a post with no `label` key leaves the name alone, which is the shape
	// every request made before this milestone has.
	f.postQRForm(t, panel, url.Values{
		"code": {named.Slug}, "label": {"Spring poster"},
		"foreground": {"#123a6b"}, "background": {"#f5f7fa"}, "size": {"512"},
	})
	f.postQRForm(t, panel, url.Values{
		"code":       {named.Slug},
		"foreground": {"#0f172a"}, "background": {"#ffffff"}, "size": {"600"},
	})
	got = f.qrCodeBySlug(t, id, named.Slug)
	if got.Label != "Spring poster" {
		t.Errorf("a style post carrying no `label` field blanked the code's name "+
			"(it reads %q). An absent field is a different form, not an "+
			"instruction to clear one", got.Label)
	}
	if got.Style.Foreground != "#0f172a" {
		t.Errorf("that post's style did not land either; it reads %q",
			got.Style.Foreground)
	}
}

// TestARowTheFlagNeverReachedIsStillTheLinksDefault is the rolling deploy, and
// it is the one shape 04400 cannot reach.
//
// The migration flags every row carrying the empty slug at the moment it runs.
// A `qr_codes` row written *after* that by an instance still serving the
// previous release carries the empty slug and `is_default = false`, because the
// column is one that release does not know about — which is exactly the case
// GetDefaultQRCode's fallback exists for, and 04400 reasons about the deploy
// window in as many words.
//
// **What must not happen is the link losing its default silently.** The empty
// slug and the flag are two spellings of one fact, so a create that takes the
// slug off such a row without putting the flag on leaves the link matching
// neither: the read path then synthesises a code the link does not have, the
// untagged `qr` bucket stops folding onto the code every printed picture
// resolves through and appears as its own unlabelled row, and the next style
// write inserts a third row beside the two. None of it raises an error, which is
// why it is asserted here rather than left to the shape of the code.
func TestARowTheFlagNeverReachedIsStillTheLinksDefault(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("rolling", "https://example.com/x")

	// The picture as printed, carrying no tag — the payload every copy of this
	// link's code in the world has.
	printed := f.codeContent(t, id, "")

	// A style, which is what writes the default code's row down.
	styled := f.putJSON("/api/v1/links/"+id.String()+"/qr",
		`{"style":{"foreground":"#123a6b","background":"#f5f7fa"}}`)
	if styled.StatusCode != http.StatusOK {
		t.Fatalf("styling the default answered %d", styled.StatusCode)
	}
	_ = styled.Body.Close()

	// And this is the fixture: the same row as the previous release would have
	// left it, identified by its empty slug and by nothing else.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE qr_codes SET is_default = false WHERE link_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var slug string
	var flagged bool
	if err := f.pool.QueryRow(t.Context(),
		`SELECT slug, is_default FROM qr_codes WHERE link_id = $1`, id,
	).Scan(&slug, &flagged); err != nil {
		t.Fatal(err)
	}
	if slug != "" || flagged {
		t.Fatalf("the fixture is not a pre-flag row: slug %q, is_default %v", slug, flagged)
	}

	// The second code appears, which is the moment the first one is named.
	named := f.createQRCode(t, id, "Autumn poster")

	if n := f.qrRowCount(id); n != 2 {
		t.Fatalf("%d rows after adding a second code, want the two the link has", n)
	}
	var defSlug string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT slug FROM qr_codes WHERE link_id = $1 AND is_default`, id,
	).Scan(&defSlug); err != nil {
		t.Fatalf("the link has no row holding the flag after its default was named, "+
			"so it has no default at all: %v", err)
	}
	if defSlug == named.Slug || defSlug == "" {
		t.Fatalf("the flag is on %q; it belongs on the code that was already there, "+
			"which is neither the new code %q nor a row with no slug", defSlug, named.Slug)
	}

	codes := f.listQRCodes(t, id)
	if len(codes) != 2 {
		t.Errorf("the link lists %d codes, want 2: a link whose default is not "+
			"reachable has one synthesised into every read, and that code has no "+
			"row, no download anybody has printed and nothing to remove (%+v)",
			len(codes), codes)
	}

	// A style write on the default afterwards finds the row rather than inserting
	// a second unnamed one beside it.
	restyled := f.putJSON("/api/v1/links/"+id.String()+"/qr", `{"style":{"foreground":"#0a0a0a"}}`)
	if restyled.StatusCode != http.StatusOK {
		t.Fatalf("restyling the default answered %d", restyled.StatusCode)
	}
	_ = restyled.Body.Close()
	if n := f.qrRowCount(id); n != 2 {
		t.Errorf("%d rows after restyling the default, want 2: a style write that "+
			"cannot find the default inserts a row for it, and the link then has "+
			"two codes nobody named", n)
	}

	// The whole of why it matters: the picture printed before any of this still
	// counts against the code it always did.
	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 1)
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var onDefault int64
	for _, c := range f.linkStats(t, id).QRCodes {
		if c.Slug == defSlug {
			onDefault = c.Clicks
		}
		if c.Slug == "" {
			t.Errorf("the breakdown holds an unlabelled row with %d clicks; the "+
				"untagged bucket belongs to whichever code holds the flag, and a row "+
				"of its own is what a reader sees when no code does", c.Clicks)
		}
	}
	if onDefault != 1 {
		t.Errorf("the default code's row reads %d, want the printed picture's scan", onDefault)
	}
}

// TestACodeIsRemovableWhenTheDefaultHasNoRowOfItsOwn is the other half of F222,
// and it is about the two counts agreeing.
//
// A link can hold one named row while its default has none — the previous
// release wrote that shape every time somebody added a code, and
// docs/slo.md's k6 fixture writes it too. The list the reader is looking at
// shows two codes, because a link's default exists whether or not a row holds
// it. Counting rows instead would put a Remove control on both and refuse both,
// and refuse the named one by saying it is the link's only code while two are
// on screen.
func TestACodeIsRemovableWhenTheDefaultHasNoRowOfItsOwn(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("rowless", "https://example.com/x")
	named := f.createQRCode(t, id, "Autumn poster")

	// The fixture: the default's row taken away, leaving the named code and a
	// default that only the read path knows about.
	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM qr_codes WHERE link_id = $1 AND id <> $2`, id, named.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.qrRowCount(id); n != 1 {
		t.Fatalf("the fixture holds %d rows, want the one named code", n)
	}
	if codes := f.listQRCodes(t, id); len(codes) != 2 {
		t.Fatalf("the link lists %+v; the fixture is two codes on screen and one "+
			"row behind them", codes)
	}

	// The named code goes. The refusal that used to fire here counted rows, and
	// the reader was looking at two codes.
	status, body := f.deleteAPIBody("/api/v1/links/" + id.String() + "/qr/codes/" + named.Slug)
	if status != http.StatusNoContent {
		t.Fatalf("removing a named code from a link whose default has no row "+
			"answered %d, want 204: %s", status, body)
	}
	if codes := f.listQRCodes(t, id); len(codes) != 1 || !codes[0].Default {
		t.Errorf("the link lists %+v after the removal, want only its default", codes)
	}

	// And the same state from the other side: the default is the row that does
	// not exist, and removing it is the flag moving to the code that does.
	second := f.createLink("rowless2", "https://example.com/y")
	keep := f.createQRCode(t, second, "Shop window")
	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM qr_codes WHERE link_id = $1 AND id <> $2`, second, keep.ID); err != nil {
		t.Fatal(err)
	}
	f.postQRForm(t, "/links/"+second.String()+"/qr", url.Values{
		"code": {""}, "remove": {"1"},
	})
	codes := f.listQRCodes(t, second)
	if len(codes) != 1 || codes[0].Slug != keep.Slug || !codes[0].Default {
		t.Fatalf("the link lists %+v after removing the default that had no row; "+
			"there was nothing to delete, so what removing it means is that %q "+
			"becomes the code an untagged scan resolves through", codes, keep.Slug)
	}
	if n := f.qrRowCount(second); n != 1 {
		t.Errorf("%d rows after the removal, want the one code that is left", n)
	}
}

// TestALoneDefaultCodeStaysOutOfTheRedirectSnapshot is what keeps M50's
// no-version-bump argument resting on a fact rather than on a comment.
//
// `Snapshot.Codes` promises that a link with no named codes carries exactly the
// payload it carried before this milestone, and that promise is the stated
// reason CacheKeyVersion did not move. A link's only code keeps the empty slug,
// so the lateral has to leave it out: `CodeSlug` returns before it scans when
// the parameter is empty, which makes an empty string in that array a value no
// request could ever match — serialized out of Postgres on every cold resolve
// and thrown away.
//
// The second half is the reopening's: the default code's own slug *does* ride
// home once it has one, because a picture of it now carries a tag and a tag that
// the snapshot does not know is a tag the redirect records as no code at all.
func TestALoneDefaultCodeStaysOutOfTheRedirectSnapshot(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("snapshot", "https://example.com/x")

	// Styled, so the link's only code has a row — which is the case that could
	// put an empty slug in the array. A link with no rows at all never could.
	styled := f.putJSON("/api/v1/links/"+id.String()+"/qr", `{"style":{"foreground":"#123a6b"}}`)
	if styled.StatusCode != http.StatusOK {
		t.Fatalf("styling the default answered %d", styled.StatusCode)
	}
	_ = styled.Body.Close()
	if slugs := f.redirectCodeSlugs(t, id); len(slugs) != 0 {
		t.Errorf("the redirect snapshot carries %q for a link whose only code has "+
			"no slug; nothing can match an empty string, and Snapshot.Codes "+
			"promises this link's payload is the one it had before M50", slugs)
	}

	// And once it stops being alone, both codes are matchable and both ride home.
	named := f.createQRCode(t, id, "Autumn poster")
	def := f.qrCode(id)
	got := f.redirectCodeSlugs(t, id)
	want := map[string]bool{named.Slug: true, def.Slug: true}
	if len(got) != 2 {
		t.Fatalf("the snapshot carries %q, want the link's two codes %q and %q",
			got, def.Slug, named.Slug)
	}
	for _, slug := range got {
		if !want[slug] {
			t.Errorf("the snapshot carries %q, which is not one of this link's codes; "+
				"a value the redirect path cannot match is a scan attributed to the "+
				"default that was printed off a code of its own", slug)
		}
	}
}

// redirectCodeSlugs is what the redirect path is handed for a link: the slugs
// the lateral in ResolveAliasForRedirect aggregates, read through the generated
// query rather than rebuilt here.
func (f *ruleFixture) redirectCodeSlugs(t *testing.T, linkID uuid.UUID) []string {
	t.Helper()
	var domainID uuid.UUID
	var alias string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT domain_id, alias FROM links WHERE id = $1`, linkID,
	).Scan(&domainID, &alias); err != nil {
		t.Fatal(err)
	}
	row, err := dbgen.New(f.pool).ResolveAliasForRedirect(t.Context(),
		dbgen.ResolveAliasForRedirectParams{DomainID: domainID, Alias: alias})
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	if err := json.Unmarshal(row.CodeSlugs, &slugs); err != nil {
		t.Fatal(err)
	}
	return slugs
}

// listQRCodes reads a link's codes over the API.
func (f *ruleFixture) listQRCodes(t *testing.T, linkID uuid.UUID) []link.QRCode {
	t.Helper()
	var out struct {
		Codes []link.QRCode `json:"codes"`
	}
	body := f.getJSON("/api/v1/links/" + linkID.String() + "/qr/codes")
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out.Codes
}

// qrCodeBySlug reads one code over the API.
func (f *ruleFixture) qrCodeBySlug(t *testing.T, linkID uuid.UUID, slug string) link.QRCode {
	t.Helper()
	var out struct {
		Code link.QRCode `json:"code"`
	}
	body := f.getJSON("/api/v1/links/" + linkID.String() + "/qr/codes/" + slug)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out.Code
}

// deleteAPIBody issues a DELETE and returns the status with the body, for the
// refusals whose sentence is the thing being asserted.
func (f *ruleFixture) deleteAPIBody(path string) (int, string) {
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
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
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

// --- M58: the refusal, and where it lands (F170) -------------------------------

// TestARefusedStyleStaysOnThePageItWasTypedOn is F170.
//
// **The panel is a route, so a refusal has two honest destinations and only the
// form knows which.** The success path has matched `next` against them since
// M48; the refusal path rendered the link page whatever the form said, which
// answered somebody working at /links/{id}/qr with a different page and left
// them to find their way back to the one they were standing on. M49 is what made
// it ordinary rather than rare: the size box is the panel's only free-text field
// and an out-of-range size is now the everyday refusal.
//
// What must not change is the refusal itself — 422, the message beside the form,
// nothing stored — so both halves are asserted on both surfaces.
func TestARefusedStyleStaysOnThePageItWasTypedOn(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("refused", "https://example.com/x")
	named := f.createQRCode(t, id, "Shop window card")
	page := "/links/" + id.String()
	panel := page + "/qr"

	refuse := func(next string) (int, string) {
		t.Helper()
		resp := f.postForm(panel, url.Values{
			"next": {next}, "code": {named.Slug},
			"foreground": {"#000000"}, "background": {"#ffffff"},
			// Nine pixels is nothing that can be printed, and the message that
			// comes back names the range.
			"size": {"9"},
		})
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	const refusal = "is not a size anything can be printed at"

	status, body := refuse(panel)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 9px code posted from the panel's page = %d, want 422", status)
	}
	if !strings.Contains(body, ">QR code</h1>") {
		t.Error("a refusal typed at /links/{id}/qr came back as some other page. The " +
			"form carries `next` precisely so a reader is answered on the surface " +
			"they are standing on, and the success path has honoured it since M48")
	}
	if strings.Contains(body, "← Links") {
		t.Error("the refusal rendered the link page's heading; that is the page " +
			"F170 is about not being sent to")
	}
	if !strings.Contains(body, refusal) {
		t.Errorf("the panel page came back without the reason:\n%.400s", body)
	}
	// The code being worked on survives the refusal, exactly as it survives a
	// save: a refusal that reset the selection would lose the reader's place as
	// well as their number.
	if !strings.Contains(body, named.Slug) {
		t.Errorf("the refusal dropped the reader off code %q and onto another",
			named.Slug)
	}

	// And the link page is still where a refusal from the link page lands.
	status, body = refuse(page)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 9px code posted from the link page = %d, want 422", status)
	}
	if !strings.Contains(body, "← Links") || strings.Contains(body, ">QR code</h1>") {
		t.Error("a refusal typed on the link page came back as the panel's own page; " +
			"`next` is a choice between two surfaces, not a redirect to one")
	}
	if !strings.Contains(body, refusal) {
		t.Errorf("the link page came back without the reason:\n%.400s", body)
	}

	// Nothing was stored by either, which is the half of the refusal that was
	// already correct and has to stay correct.
	// Two rows, and neither is a refusal's doing: adding the named code is what
	// writes the default's row down (D183), so the count was 2 before the first
	// refusal and has to be 2 after the second.
	if n := f.qrRowCount(id); n != 2 {
		t.Errorf("%d qr_codes rows after two refusals, want the named code's and the "+
			"default's, which adding the named one created", n)
	}
}

// TestARefusedLogoStaysOnThePageItWasUploadedFrom is F170 on the upload, which
// is the path where `next` arrives as a part of a multipart body rather than as
// an encoded field — the same two hidden inputs, in the only body a file has.
func TestARefusedLogoStaysOnThePageItWasUploadedFrom(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("refusedlogo", "https://example.com/x")
	panel := "/links/" + id.String() + "/qr"

	// Not an image at all: internal/qr decides by content, so the refusal is the
	// decoder's and it is a 422 with a sentence beside the form.
	contentType, body := logoBody(t, "brand.png", "image/png",
		[]byte("this is not a picture"), map[string]string{"next": panel, "code": ""})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.server.URL+panel+"/logo", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a logo that is not an image = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(page), ">QR code</h1>") {
		t.Errorf("a refused upload from the panel's page came back as some other "+
			"page:\n%.400s", page)
	}
}

// --- M50.8: the list stops moving under the reader ---------------------------

// TestTheCodesListIsAlphabeticalAndStaysPutWhenTheDefaultMoves is F238(a),
// owner-reported: *"Selecting a different default code re-orders the list of
// codes which can make it seem like the selection didn't change. Keeping them in
// Alphabetical order by name is probably the best option."*
//
// **It reverses `ListQRCodes`' stated design and the reversal is the point.**
// The query led with the flag-holder and its comment defended exactly that —
// *"the list re-orders when the reader moves it, which is the visible half of
// what setting a default does"*. That argument is answered rather than
// withdrawn: M50.7 put a filled icon on the row that holds the flag, so the
// change is visible without the list moving, and the movement was what made the
// change look like nothing had happened.
//
// **It is an integration test and not an `internal/link` one.** m50.8.md asks
// for the latter, and the order is produced by `ORDER BY lower(q.label), q.id`
// in campaigns.sql — a Go unit test in that package would have to fake
// `dbgen.Querier` whole to see it, and would then be asserting a fake's
// behaviour rather than the query's. This drives `link.Service.ListQRCodes`,
// which is the `internal/link` surface the bullet names, against the database
// that actually sorts.
//
// The labels are chosen so alphabetical and creation order are **opposite**, and
// so that neither matches the order the flag is moved through: a list that
// happened to satisfy two of the three would prove nothing.
func TestTheCodesListIsAlphabeticalAndStaysPutWhenTheDefaultMoves(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("sorted", "https://example.com/x")

	// Created newest-first alphabetically, so creation order reversed is the
	// answer and any surviving `created_at` in the key shows up immediately.
	zulu := f.createQRCode(t, id, "Zulu poster")
	mike := f.createQRCode(t, id, "Mike poster")
	alpha := f.createQRCode(t, id, "alpha poster")

	// The link's own default has no label at all and therefore sorts first,
	// which is also where it used to be for the other reason entirely.
	def := f.qrCode(id)

	names := func() []string {
		t.Helper()
		codes, err := f.links.ListQRCodes(t.Context(), f.owner, id)
		if err != nil {
			t.Fatalf("list qr codes: %v", err)
		}
		out := make([]string, 0, len(codes))
		for _, c := range codes {
			out = append(out, c.Label)
		}
		return out
	}

	want := []string{"", "alpha poster", "Mike poster", "Zulu poster"}
	got := names()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the codes list reads %q, want %q — alphabetical by name and "+
			"case-insensitively, which is why \"alpha\" leads \"Mike\" (F238a)", got, want)
	}

	// And it does not move when the default does. Three moves, because one is
	// indistinguishable from a list that happens to be right once.
	for _, slug := range []string{zulu.Slug, alpha.Slug, mike.Slug, def.Slug} {
		resp := f.putJSON("/api/v1/links/"+id.String()+"/qr/codes/"+slug+"/default", "")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusOK {
			t.Fatalf("setting %q as the default answered %d", slug, status)
		}
		if got := names(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("after making %q the default the list reads %q, want %q. The "+
				"order moving under the reader is the whole of what was reported",
				slug, got, want)
		}
		// The flag did move — otherwise the assertion above is passing because
		// nothing happened at all.
		if now := f.qrCode(id); now.Slug != slug {
			t.Fatalf("the default is %q after asking for %q", now.Slug, slug)
		}
	}
}

// TestTheUntaggedBucketFollowsTheFlagWhereverItSorts is the hidden edge the
// sort change opens, asserted rather than hoped for.
//
// `link.ListQRCodes` and `analytics.qrCodeSplit` both tested `rows[0]` to decide
// whether any row held the default — which was the whole set's answer only
// because the query put the flag-holder first. Alphabetical order breaks that
// identity: with the flag on a code that sorts last, the old test would have
// synthesised a second default *and* added every untagged scan to the real one,
// reporting the same clicks twice on one page.
//
// So the link here holds the flag on the alphabetically **last** code, which is
// exactly the arrangement the old code got wrong.
func TestTheUntaggedBucketFollowsTheFlagWhereverItSorts(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("bucketed", "https://example.com/x")

	// The picture as printed before this link's codes had identities.
	printed := f.codeContent(t, id, "")

	first := f.createQRCode(t, id, "Alpha poster")
	last := f.createQRCode(t, id, "Zulu poster")
	def := f.qrCode(id)

	// The flag onto the code that sorts last, and off the unlabelled row that
	// sorts first — which is what makes rows[0] the wrong question.
	resp := f.putJSON("/api/v1/links/"+id.String()+"/qr/codes/"+last.Slug+"/default", "")
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("setting the default answered %d", status)
	}

	codes, err := f.links.ListQRCodes(t.Context(), f.owner, id)
	if err != nil {
		t.Fatalf("list qr codes: %v", err)
	}
	if len(codes) != 3 {
		t.Fatalf("the link lists %d codes, want 3 (%q, %q and the one it started "+
			"with) — a fourth is the default being invented a second time because "+
			"the flag-holder does not sort first", len(codes), first.Label, last.Label)
	}
	if codes[len(codes)-1].Slug != last.Slug || !codes[len(codes)-1].Default {
		t.Fatalf("the flag is not on the last row: %+v", codes)
	}

	f.scan(t, printed)
	waitForClicks(t, f.pool, id, 1)
	if err := analytics.NewRoller(f.pool, nil).
		RunRecentDimensions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var total int64
	var onFlag int64
	for _, c := range f.linkStats(t, id).QRCodes {
		total += c.Clicks
		if c.Slug == last.Slug {
			onFlag = c.Clicks
		}
		if c.Slug == def.Slug && c.Clicks != 0 {
			t.Errorf("the row that used to hold the flag reads %d scans; the untagged "+
				"bucket follows the flag, and the flag moved", c.Clicks)
		}
	}
	if onFlag != 1 {
		t.Errorf("the code holding the flag reads %d untagged scans, want 1", onFlag)
	}
	if total != 1 {
		t.Errorf("the breakdown reports %d scans over one scan. A second untagged "+
			"bucket beside the flag-holder's is the same click counted twice, which "+
			"is what reading position 0 as the default would have produced", total)
	}
}
