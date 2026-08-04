package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// feedsPageData is what pages/feeds.html renders.
//
// Note what is not here: no form state, no field errors, no notice, no CSRF
// token. A page that carries none of those is a page nothing was written from,
// and that is the shape D40 asks for rather than a convention this handler
// happens to follow — see FeedsPage.
type feedsPageData struct {
	shell
	Disclosure link.DestinationDisclosure
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
// At the time this product had no instance-level principal — under open signup
// every stranger who registers owns an organization — so there was nobody the
// permission system could name who may move a setting that affects everyone.
// Reading is not changing, and being told what an instance does with your
// destinations is not a privilege that needs a principal.
//
// **D98 introduced a principal, and it does not reach this page** (M45). Its
// scopes are enumerated rather than implied — the dispute queue, the blocklist
// entries those decisions lift, and the instance-wide audit surface — and the
// feed configuration is deliberately not among them, because a principal that
// accumulates scopes because it exists is the thing D38 was avoiding. So the
// risk is unchanged and one degree sharper: it is the toggle somebody adds
// beside the row next year, now that there is a permission it would look
// plausible under, at which point D38 has been reversed by nobody in particular.
// The no-POST test is what makes that an explicit act.
//
// Ungated on purpose. Every signed-in user's destinations are what get sent, so
// every signed-in user may read this — a disclosure only owners can see is a
// disclosure to the people who already configured it.
//
// **Ungated includes the webhook half**, which is the one thing about this page
// that is not a copy of how it shipped. Reading the registry — who a workspace
// posts to — needs `webhooks.read`; being told that *something* receives the
// destinations you type needs nothing, because it is a fact about your own data
// rather than about the workspace's configuration. link.WebhookDisclosure is
// what enforces that distinction: it carries a count and no URL.
func (h *Web) FeedsPage(w http.ResponseWriter, r *http.Request) {
	// From the link service rather than from config, so the page describes the
	// client that actually does the sending and the rows that actually exist.
	disclosure, err := h.Links.DestinationDisclosure(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		// Not swallowed to a zero value the way the shell's badge is. A zero
		// disclosure is the green panel, so failing this read quietly would print
		// the strongest claim on the page at exactly the moment nothing is known.
		h.webError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "feeds", feedsPageData{
		// The menu still calls this entry "Reputation feeds", which is what
		// somebody looking for it is looking for; the page names both channels
		// because both are what it now answers.
		shell:      h.shell(r, "Reputation feeds and webhooks", "feeds"),
		Disclosure: disclosure,
	})
}
