//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
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
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(body)
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
func TestTheStyleFormOnThePageStoresWhatTheAPIWouldStore(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("formed", "https://example.com/x")

	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{
		"foreground": {"#123456"}, "background": {"#fedcba"},
		"level": {"Q"}, "margin": {"5"}, "scale": {"10"},
	})
	if got := f.storedQRStyle(id); got.Foreground != "#123456" || got.Level != qr.LevelQ {
		t.Fatalf("the form stored %+v", got)
	}
	if !strings.Contains(f.qrSVG(id), `fill="#123456"`) {
		t.Error("the form's colour is not in the served picture")
	}

	// The reset button is a value of the same form rather than a second one.
	f.postQRForm(t, "/links/"+id.String()+"/qr", url.Values{"reset": {"1"}})
	if n := f.qrRowCount(id); n != 0 {
		t.Errorf("%d rows after the reset button, want 0", n)
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
	code, err := f.links.QRCode(f.t.Context(), f.owner, linkID)
	if err != nil {
		f.t.Fatalf("read qr: %v", err)
	}
	return code.Style
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
