package httpx

import (
	"net/http"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Campaigns over the API (M41).
//
// Thin, like every other handler here: the slug rule, the schedule check and the
// unlabelling that follows a delete all live in internal/link, so the dashboard
// forms get identical behaviour by calling the same methods.
//
// **A sibling collection rather than a subresource of a link**, exactly as
// folders are. A campaign exists whether or not anything carries it, and which
// links do is a question the links list answers with `?campaign=`.

// campaignFilterNone is what `?campaign=` carries to mean "the links in no
// campaign", matching folderFilterNone and shared by both surfaces so a filter
// URL from one can be pasted into the other.
const campaignFilterNone = folderFilterNone

type createCampaignRequest struct {
	Name string `json:"name"`
	// Slug is optional. An absent or empty one is derived from the name, so the
	// common case is one field.
	Slug        string     `json:"slug"`
	Description string     `json:"description"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
}

// updateCampaignRequest is a partial update. The two schedule bounds are
// explicitly nullable, and null means "remove this bound" rather than
// "unchanged" — the only two fields on this API where it does, because a
// campaign that has stopped having an end date is a real request and `omitempty`
// cannot express it.
type updateCampaignRequest struct {
	Name        *string    `json:"name"`
	Slug        *string    `json:"slug"`
	Description *string    `json:"description"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	// ClearStartsAt and ClearEndsAt are how a JSON client removes a bound. A
	// separate flag rather than a null, because `starts_at: null` is
	// indistinguishable from an omitted field once it has been decoded into a
	// pointer, and guessing wrong either drops a schedule nobody asked to drop
	// or refuses to drop one somebody did.
	ClearStartsAt bool `json:"clear_starts_at"`
	ClearEndsAt   bool `json:"clear_ends_at"`
}

func (a *LinkAPI) ListCampaigns(w http.ResponseWriter, r *http.Request) {
	campaigns, err := a.Links.Campaigns(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"campaigns": campaigns})
}

func (a *LinkAPI) GetCampaign(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "campaignID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	campaign, err := a.Links.Campaign(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, campaign)
}

func (a *LinkAPI) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	campaign, err := a.Links.CreateCampaign(r.Context(), IdentityFrom(r.Context()),
		link.CreateCampaignInput{
			Name: req.Name, Slug: req.Slug, Description: req.Description,
			StartsAt: req.StartsAt, EndsAt: req.EndsAt,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, campaign)
}

func (a *LinkAPI) UpdateCampaign(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "campaignID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateCampaignRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	campaign, err := a.Links.UpdateCampaign(r.Context(), IdentityFrom(r.Context()), id,
		link.UpdateCampaignInput{
			Name: req.Name, Slug: req.Slug, Description: req.Description,
			StartsAt: req.StartsAt, ClearStartsAt: req.ClearStartsAt,
			EndsAt: req.EndsAt, ClearEndsAt: req.ClearEndsAt,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, campaign)
}

func (a *LinkAPI) DeleteCampaign(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "campaignID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteCampaign(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
