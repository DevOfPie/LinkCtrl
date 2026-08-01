package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// FeedAPI is the JSON half of the disclosure.
type FeedAPI struct {
	Links *link.Service
}

// Get answers what this instance does with destinations.
//
// One operation, GET, and there will not be a second. The dashboard page it
// mirrors has no controls and accepts no POST (D40), and the inherited rule that
// every UI feature has API support cuts both ways: an API that could write here
// would be the settings surface D38 removed, reachable with a bearer token
// instead of a session.
//
// Ungated beyond authentication, matching the page. What is being disclosed is
// what happens to the caller's own destinations.
func (a *FeedAPI) Get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.Links.FeedDisclosure())
}
