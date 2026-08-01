package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/feed"
)

// feedsPageData is what pages/feeds.html renders.
//
// Note what is not here: no form state, no field errors, no notice, no CSRF
// token. A page that carries none of those is a page nothing was written from,
// and that is the shape D40 asks for rather than a convention this handler
// happens to follow — see FeedsPage.
type feedsPageData struct {
	shell
	Disclosure feed.Disclosure
}

// FeedsPage discloses what this instance does with destinations.
//
// **It is read-only, it has no controls, and it accepts no POST.** That is
// decision D40 and it is asserted by TestTheDisclosurePageAcceptsNoWrite rather
// than left to whoever edits the template next. The reason is worth carrying
// here, because the next person to want a row on this page will not read the
// decision log first:
//
// D38 removed the ability to change instance-wide settings from the dashboard.
// This product has no instance-level principal — under open signup every
// stranger who registers owns an organization — so there is nobody the
// permission system can name who may move a setting that affects everyone.
// Reading is not changing, and being told what an instance does with your
// destinations is not a privilege that needs a principal. The risk is not this
// page; it is the toggle somebody adds beside the row next year, at which point
// D38 has been reversed by nobody in particular. The no-POST test is what makes
// that an explicit act.
//
// Ungated on purpose. Every signed-in user's destinations are what get sent, so
// every signed-in user may read this — a disclosure only owners can see is a
// disclosure to the people who already configured it.
func (h *Web) FeedsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "feeds", feedsPageData{
		shell: h.shell(r, "Reputation feeds", "feeds"),
		// From the link service rather than from config, so the page describes
		// the client that actually does the sending.
		Disclosure: h.Links.FeedDisclosure(),
	})
}
