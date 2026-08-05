package httpx

import "testing"

// The workspace switcher stays on the page, except where the page names an
// object the switch leaves behind.
//
// The switcher carries the current path as `next` so that switching workspace
// keeps you where you were — which is the whole reason it carries one. On a
// link's detail page that path names an id belonging to the workspace being
// left, so following it lands on Not found: a control that visibly fails to
// follow a switch, on the one surface where the collection view would have
// worked (F22).
//
// Collapsed to `/links` rather than to `/dashboard`, because the reader was
// looking at links and still wants links.
func TestTheSwitcherLeavesAnObjectBehindAndKeepsThePage(t *testing.T) {
	for path, want := range map[string]string{
		// The defect.
		"/links/019fd0-aaaa":      "/links",
		"/links/019fd0-aaaa/edit": "/links",

		// Everything else stays put, which is the behaviour being preserved.
		"/links":         "/links",
		"/dashboard":     "/dashboard",
		"/notifications": "/notifications",
		"/members":       "/members",
		"/account":       "/account",
		"/campaigns":     "/campaigns",

		// Not a link id: the collection with a query is still the collection,
		// and trimming it would throw away a filter the reader chose.
		"/links/": "/links/",
	} {
		t.Run(path, func(t *testing.T) {
			if got := switchTarget(path); got != want {
				t.Errorf("switchTarget(%q) = %q, want %q", path, got, want)
			}
		})
	}
}
