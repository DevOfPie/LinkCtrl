package httpx

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// M50.8's third reopening, at the two seams where selecting a code stops being a
// journey: where a row in the list points, and where a write on the code it
// selected comes back to.
//
// Both are string-building against a surface the caller names, and both are
// **matched, never followed** — the paths are built here and compared against,
// so neither can be talked into a third destination by a request. That is
// qrReturn's own discipline (D178) and qrSelectPath inherits it rather than
// deciding again.

const qrSelectLinkID = "0198c9c5-0000-7000-8000-000000000001"

// TestARowSelectsOnTheSurfaceItIsDrawnOn is the finding under the report:
// `/links/{id}` passed no code at all, so on the link page *selected* meant
// *default* and moving the flag was the only way to change what the tab drew.
// The list's rows were the other half of it — every one of them pointed at the
// panel route, which is a different page.
//
// Two surfaces, four rows: with a slug and without, because a link nobody has
// touched still has a default code with no slug to name it by, and a `?code=`
// on that row would name nothing.
func TestARowSelectsOnTheSurfaceItIsDrawnOn(t *testing.T) {
	id := uuid.MustParse(qrSelectLinkID)
	page := "/links/" + qrSelectLinkID

	for name, tc := range map[string]struct {
		back, slug, want string
	}{
		"the link page names the code on itself": {
			back: page, slug: "k7m2qh4b", want: page + "?tab=qr&code=k7m2qh4b",
		},
		"the link page's unnamed default is the tab alone": {
			back: page, slug: "", want: page + "?tab=qr",
		},
		"the panel page keeps its own route": {
			back: page + "/qr", slug: "k7m2qh4b", want: page + "/qr?code=k7m2qh4b",
		},
		"the panel page's unnamed default is the route alone": {
			back: page + "/qr", slug: "", want: page + "/qr",
		},
		// A `next` this function did not build is the link page, exactly as
		// qrReturn treats one: the choice is between two surfaces and the
		// fallback is the one a reader is normally standing on.
		"anything else falls to the link page": {
			back: "https://elsewhere.test/links", slug: "k7m2qh4b",
			want: page + "?tab=qr&code=k7m2qh4b",
		},
	} {
		if got := qrSelectPath(id, tc.back, tc.slug); got != tc.want {
			t.Errorf("%s: qrSelectPath(%q, %q) = %q, want %q",
				name, tc.back, tc.slug, got, tc.want)
		}
	}

	// And the slug is escaped rather than concatenated. Slugs are `[a-z0-9]`
	// today, which is exactly why this is worth pinning: nothing would fail if
	// the escaping went, and the row's href is a URL a template writes.
	got := qrSelectPath(id, page, "a b&c=d")
	if strings.Contains(got, " ") || strings.Count(got, "&") != 1 {
		t.Errorf("qrSelectPath did not escape the slug into the query: %q", got)
	}
}

// TestQRCodeViewsPointEveryRowAtTheSurfaceItWasBuiltFor is the same claim one
// level up, because qrSelectPath being right is worth nothing if the rows are
// built without it. The list is the only thing a reader can click.
func TestQRCodeViewsPointEveryRowAtTheSurfaceItWasBuiltFor(t *testing.T) {
	id := uuid.MustParse(qrSelectLinkID)
	page := "/links/" + qrSelectLinkID
	codes := []link.QRCode{
		{Slug: "d3f4u1t0", Default: true},
		{Slug: "k7m2qh4b", Label: "Autumn poster"},
	}

	rows := qrCodeViews(id, codes, "k7m2qh4b", page)
	if len(rows) != len(codes) {
		t.Fatalf("qrCodeViews returned %d rows for %d codes", len(rows), len(codes))
	}
	for _, row := range rows {
		want := page + "?tab=qr&code=" + row.Slug
		if row.Select != want {
			t.Errorf("on the link page the row for %q selects %q, want %q; a row "+
				"pointing at the panel route is the document load F244(b) and "+
				"F246(d) both report", row.Slug, row.Select, want)
		}
	}
	if !rows[1].Selected || rows[0].Selected {
		t.Error("the selected row is not the one the slug names, so the list marks " +
			"a code the form below it is not editing")
	}

	for _, row := range qrCodeViews(id, codes, "", page+"/qr") {
		want := page + "/qr?code=" + row.Slug
		if row.Select != want {
			t.Errorf("on the panel page the row for %q selects %q, want %q; that "+
				"page has no tab strip to swap and this href is its whole "+
				"mechanism", row.Slug, row.Select, want)
		}
	}
}

// TestAQRWriteReturnsToTheCodeItWasEditing is the second half of this
// reopening's *two things move with it*: the link-page branch of qrReturn
// dropped the slug, deliberately and correctly, for as long as the link page
// could draw nothing but the default. Left there, the first save on a selected
// code would return the reader to the default one — the bug moved rather than
// fixed.
//
// The panel branch is asserted beside it because it is the behaviour being
// generalised, and a change that carried the slug to one surface by taking it
// off the other would satisfy the sentence above and break D183's own bullet.
func TestAQRWriteReturnsToTheCodeItWasEditing(t *testing.T) {
	id := uuid.MustParse(qrSelectLinkID)
	page := "/links/" + qrSelectLinkID

	for name, tc := range map[string]struct {
		next, slug string
		wantPath   string
		wantCode   string
	}{
		"the link page": {
			next: page, slug: "k7m2qh4b", wantPath: page, wantCode: "k7m2qh4b",
		},
		"the panel page": {
			next: page + "/qr", slug: "k7m2qh4b",
			wantPath: page + "/qr", wantCode: "k7m2qh4b",
		},
		// A remove returns no slug, because the code it named is gone. Both
		// surfaces then land on the default, which is what the list will draw.
		"a write with no code names none": {
			next: page, slug: "", wantPath: page, wantCode: "",
		},
	} {
		got := qrReturn(tc.next, id, "styled", tc.slug, nil)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: qrReturn built an unparseable URL %q: %v", name, got, err)
		}
		if u.Path != tc.wantPath {
			t.Errorf("%s: a save returned to %q, want the path %q", name, got, tc.wantPath)
		}
		if code := u.Query().Get("code"); code != tc.wantCode {
			t.Errorf("%s: a save on %q returned to code=%q, want %q. Landing on the "+
				"default after styling a named code is the reader losing their "+
				"selection to their own save", name, tc.slug, code, tc.wantCode)
		}
		if u.Query().Get("qr") != "styled" {
			t.Errorf("%s: the marker did not survive: %q", name, got)
		}
	}

	// The link-page branch still derives its tab and still keeps the fragment
	// the script-blocked reader lands on (D178, D195). Carrying the code is an
	// addition to that URL, not a replacement for it.
	got := qrReturn(page, id, "defaulted", "k7m2qh4b", nil)
	if !strings.HasSuffix(got, "#qr") || !strings.Contains(got, "tab=qr") {
		t.Errorf("the link page's return lost its tab or its fragment: %q", got)
	}
}
