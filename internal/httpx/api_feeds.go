package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// FeedAPI is the JSON half of the disclosure.
type FeedAPI struct {
	Links *link.Service
}

// Get answers what happens to the destinations the caller submits.
//
// One operation, GET, and there will not be a second. The dashboard page it
// mirrors has no controls and accepts no POST (D40), and the inherited rule that
// every UI feature has API support cuts both ways: an API that could write here
// would be the settings surface D38 removed, reachable with a bearer token
// instead of a session.
//
// Ungated beyond authentication, matching the page. What is being disclosed is
// what happens to the caller's own destinations — including, since M45, the
// workspace's own webhooks, which are the channel this answered nothing about
// while saying `{"enabled": false}` meant nothing leaves (F135). A key with no
// webhook scope still gets the answer, because the answer is about the caller's
// data rather than about the registry: it carries a count and no URL.
func (a *FeedAPI) Get(w http.ResponseWriter, r *http.Request) {
	disclosure, err := a.Links.DestinationDisclosure(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, disclosure)
}
