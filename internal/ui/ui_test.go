package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// identityStub satisfies what the layout and pages ask of an identity — Can,
// IsAPIKey, and the display fields — without importing the auth package, which
// this package must not depend on even in tests.
type identityStub struct {
	Email string
	Name  string
	Role  string
	perms map[string]bool
}

func (s *identityStub) Can(p string) bool { return s.perms[p] }
func (s *identityStub) IsAPIKey() bool    { return false }

func owner() *identityStub {
	return &identityStub{
		Email: "o@example.com", Name: "Owner", Role: "owner",
		perms: map[string]bool{
			"links.create": true, "links.update": true, "links.delete": true,
		},
	}
}

// pageData returns representative data for every page, so the test renders
// each one for real. A page added without an entry here fails the test, which
// is the point: an unexercised template is a 500 waiting for a visitor.
func pageData(t *testing.T) map[string]any {
	t.Helper()
	now := time.Now()
	lnk := map[string]any{
		"ID": "0198c9c5-0000-7000-8000-000000000001", "Alias": "demo",
		"ShortURL": "http://links.test/demo", "URL": "https://example.com/x",
		"Title": "A demo", "Description": "", "Status": "active",
		"Tags":       []map[string]any{{"Name": "launch"}},
		"ClickCount": int64(1234), "LastClickAt": &now,
		"CreatedAt": now, "UpdatedAt": now,
	}
	stats := map[string]any{
		"Totals": map[string]int64{"Clicks": 40, "UniqueVisitors": 12, "BotClicks": 3},
		"Caveat": "estimates",
		"Dimensions": map[string]any{
			"device":   []map[string]any{{"Value": "mobile", "Clicks": int64(30)}},
			"browser":  []map[string]any{{"Value": "Chrome", "Clicks": int64(25)}},
			"os":       []map[string]any{{"Value": "Android", "Clicks": int64(20)}},
			"referrer": []map[string]any{},
			"language": []map[string]any{{"Value": "en", "Clicks": int64(35)}},
			"country":  []map[string]any{},
		},
	}
	series := []DayCount{
		{Day: "2026-07-28", Clicks: 10, Visitors: 4},
		{Day: "2026-07-29", Clicks: 0},
		{Day: "2026-07-30", Clicks: 30, Visitors: 8, Bots: 3},
	}
	return map[string]any{
		"login": map[string]any{
			"Title": "Sign in", "Nav": "", "Identity": (*identityStub)(nil),
			"Email": "", "Next": "/links", "Error": "Nope.", "Notice": "",
		},
		"setup": map[string]any{
			"Title": "Set up", "Nav": "", "Identity": (*identityStub)(nil),
			"Name": "", "Email": "", "Error": "",
		},
		"error": map[string]any{
			"Title": "Not found", "Nav": "", "Identity": (*identityStub)(nil),
			"Code": 404, "Heading": "Not found", "Message": "Gone.",
		},
		"dashboard": map[string]any{
			"Title": "Dashboard", "Nav": "dashboard", "Identity": owner(),
			"Overview": map[string]any{
				"Totals": map[string]int64{"Clicks": 100, "UniqueVisitors": 40, "BotClicks": 5},
				"Caveat": "estimates",
			},
			"Series":     series,
			"Recent":     []map[string]any{lnk},
			"TotalLinks": func() *int64 { n := int64(3); return &n }(),
		},
		"links": map[string]any{
			"Title": "Links", "Nav": "links", "Identity": owner(),
			"Links": []map[string]any{lnk}, "HasMore": true,
			"NextURL": "/links?cursor=abc",
			"Total":   func() *int64 { n := int64(1); return &n }(),
			"Search":  "demo", "Status": "", "Sort": "newest", "Filtered": true,
			"Form":        map[string]string{"URL": "", "Alias": ""},
			"FieldErrors": map[string]string{"url": "bad"},
			"Notice":      "Created.", "Error": "",
		},
		"link_detail": map[string]any{
			"Title": "/demo", "Nav": "links", "Identity": owner(),
			"Link": lnk, "Stats": stats, "Series": series,
			"RecentClicks": []map[string]any{{
				"OccurredAt": now, "Device": "mobile", "Browser": "Chrome",
				"OS": "Android", "Referrer": "", "IsBot": true,
			}},
			"Days": 30, "Windows": []int{7, 30, 90},
			"Form": map[string]string{
				"URL": "https://example.com/x", "Alias": "demo", "Title": "A demo",
				"Description": "", "ExpiresAt": "", "Tags": "launch",
			},
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		"keys": map[string]any{
			"Title": "API keys", "Nav": "keys", "Identity": owner(),
			"Keys": []map[string]any{{
				"ID": "0198c9c5-0000-7000-8000-000000000002", "Name": "ci",
				"Prefix": "lk_live_abcdefgh", "Scopes": []string{"links.read"},
				"LastUsedAt": &now, "ExpiresAt": (*time.Time)(nil),
				"RevokedAt": (*time.Time)(nil), "CreatedAt": now, "Expired": false,
			}},
			"ScopeOptions": []string{"links.read", "links.create"},
			"Created": map[string]any{
				"Key": "lk_live_abcdefgh_0123456789012345678901234567890123456789012",
			},
			"Form":        map[string]string{"Name": ""},
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		"account": map[string]any{
			"Title": "Account", "Nav": "account", "Identity": owner(),
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		// Both states in one render: a read notification and an unread one, so
		// the branch that draws the dot and the "mark read" button is exercised
		// alongside the branch that does not. Unread is non-zero so the nav
		// badge renders too — it is drawn from the shell on every page, and a
		// template error there would break every page rather than this one.
		"notifications": map[string]any{
			"Title": "Notifications", "Nav": "notifications", "Identity": owner(),
			"Unread": int64(1),
			"Items": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000003", "Kind": "audit.growth",
					"Title":  "The audit log has passed its size threshold",
					"Body":   "audit_logs now uses 5.2 GiB on disk.",
					"Data":   map[string]any{"bytes": int64(5583457484)},
					"ReadAt": (*time.Time)(nil), "CreatedAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000004", "Kind": "audit.growth",
					"Title": "An older notice", "Body": "",
					"ReadAt": &now, "CreatedAt": now,
				},
			},
			"NextCursor": "abc",
			"Notice":     "", "Error": "",
		},
	}
}

func TestEveryPageRenders(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data := pageData(t)

	for _, page := range r.Pages() {
		t.Run(page, func(t *testing.T) {
			d, ok := data[page]
			if !ok {
				t.Fatalf("no test data for page %q; add an entry to pageData so the template is exercised", page)
			}
			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, d); err != nil {
				t.Fatalf("render %s: %v", page, err)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "<!doctype html>") {
				t.Error("page did not go through the layout")
			}
			if !strings.Contains(body, "</html>") {
				t.Error("page is truncated")
			}
		})
	}
}

func TestRenderBuffersFailures(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	// The dashboard's data shape is wrong on purpose. The failure must surface
	// as an error with nothing written, not as a 200 with half a page.
	if err := r.Render(rec, http.StatusOK, "dashboard", struct{}{}); err == nil {
		t.Fatal("rendering with the wrong data shape reported success")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a failed render wrote %d bytes; partial HTML reached the client", rec.Body.Len())
	}
}

func TestRenderPartialProducesFragment(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	err = r.RenderPartial(rec, http.StatusOK, "links", "links_table", pageData(t)["links"])
	if err != nil {
		t.Fatalf("render partial: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("partial rendered the whole layout; htmx would nest a page inside the page")
	}
	if !strings.Contains(body, `id="links-table"`) {
		t.Error("partial did not include the swap target")
	}
}

func TestStaticHandler(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	h := r.StaticHandler("/static/")

	get := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	url := r.AssetURL("js/htmx.min.js")
	if !strings.Contains(url, "?v=") {
		t.Fatalf("asset URL %q carries no fingerprint", url)
	}

	rec := get(url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fingerprinted asset returned %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("fingerprinted URL Cache-Control = %q, want immutable", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		// text/plain here means browsers refuse to run it under nosniff.
		t.Errorf("Content-Type = %q", ct)
	}

	plain := get("/static/js/htmx.min.js", nil)
	if cc := plain.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unfingerprinted URL is immutable (%q); it cannot be cache-busted", cc)
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a static asset")
	}
	if rec := get(url, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("matching If-None-Match returned %d, want 304", rec.Code)
	}

	if rec := get("/static/nope.css", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset returned %d, want 404", rec.Code)
	}
	// The Tailwind entry point is a build input, not something to serve.
	if rec := get("/static/css/input.css", nil); rec.Code != http.StatusNotFound {
		t.Errorf("input.css is being served (%d); only built output belongs here", rec.Code)
	}
}

func TestBarChartGeometry(t *testing.T) {
	points := []DayCount{
		{Day: "2026-07-01", Clicks: 10},
		{Day: "2026-07-02", Clicks: 0},
		{Day: "2026-07-03", Clicks: 7},
	}
	c := BarChart(points, 720, 160)

	if len(c.Bars) != 3 {
		t.Fatalf("bars = %d, want 3", len(c.Bars))
	}
	for _, b := range c.Bars {
		if b.X < 0 || b.X+b.W > c.W {
			t.Errorf("bar for %s overflows the viewBox: x=%d w=%d", b.Day, b.X, b.W)
		}
		if b.Y < 0 || b.Y+b.H > c.PlotH {
			t.Errorf("bar for %s overflows the plot: y=%d h=%d", b.Day, b.Y, b.H)
		}
	}
	if c.Bars[1].H != 0 {
		t.Error("a zero-click day drew a bar")
	}
	if c.Bars[0].H <= c.Bars[2].H {
		t.Error("10 clicks drew a shorter bar than 7")
	}
	if c.MaxY != 10 {
		t.Errorf("axis max = %d, want the nice ceiling 10", c.MaxY)
	}

	// One click against a huge axis must still be visible.
	tiny := BarChart([]DayCount{{Day: "2026-07-01", Clicks: 1}, {Day: "2026-07-02", Clicks: 10000}}, 720, 160)
	if tiny.Bars[0].H < 1 {
		t.Error("a nonzero day rendered invisibly")
	}

	if empty := BarChart(nil, 720, 160); len(empty.Bars) != 0 {
		t.Error("an empty series produced bars")
	}
}

func TestNiceCeil(t *testing.T) {
	cases := map[int64]int64{
		0: 1, 1: 1, 2: 2, 3: 5, 7: 10, 10: 10, 11: 20,
		49: 50, 51: 100, 100: 100, 8347: 10000,
	}
	for in, want := range cases {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestFmtInt(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		1234567: "1,234,567", -4321: "-4,321",
	}
	for in, want := range cases {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPct(t *testing.T) {
	if got := pct(1, 1000); got != 1 {
		t.Errorf("a tiny nonzero share = %d, want the 1%% floor", got)
	}
	if got := pct(500, 1000); got != 50 {
		t.Errorf("pct(500,1000) = %d", got)
	}
	if got := pct(5, 0); got != 0 {
		t.Errorf("division by zero total = %d, want 0", got)
	}
	if got := pct(2000, 1000); got != 100 {
		t.Errorf("over-total share = %d, want clamped 100", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo wörld", 6); got != "héllo…" {
		t.Errorf("truncate = %q; multibyte runes must not be split", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate lengthened a short string: %q", got)
	}
}
