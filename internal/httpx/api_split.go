package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Split testing over the API (M36).
//
// Nested under the link for the reason the rules are: an arm has no meaning
// apart from one, and "which link is this about" belongs in the URL rather than
// in a body somebody has to get right. Thin, like every other handler here —
// every authorization and validation decision is in internal/link, so the
// dashboard forms get identical behaviour by calling the same methods.
//
// One endpoint returns the whole split rather than a list of arms, because a
// weighted arm's share is a fact about the *set*: 40 means nothing until you
// know what the others add up to, and a client assembling that from a paginated
// list would compute it against a partial denominator.

type createVariantRequest struct {
	// Kind is `weighted`, `sequential` or `fallback`.
	Kind string `json:"kind"`
	URL  string `json:"url"`
	// Weight is the arm's share of a weighted split. Ignored by the other kinds.
	Weight int32 `json:"weight"`
	// Enabled defaults to true when omitted, which is what an arm somebody just
	// added should be. A pointer, so "false" and "absent" stay different.
	Enabled *bool `json:"enabled"`
}

func (a *LinkAPI) GetSplit(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	split, err := a.Links.GetSplit(r.Context(), IdentityFrom(r.Context()), linkID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The permitted kinds travel with the answer, the way ListRules advertises
	// the condition vocabulary: a client building an editor discovers from the
	// response that a split is weighted or sequential and that a fallback is a
	// third thing, rather than having to read this file.
	WriteJSON(w, http.StatusOK, map[string]any{
		"split":    split,
		"kinds":    domain.SplitKinds,
		"fallback": domain.RuleKindFallback,
	})
}

func (a *LinkAPI) CreateVariant(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createVariantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	variant, err := a.Links.CreateVariant(r.Context(), IdentityFrom(r.Context()), linkID,
		link.CreateVariantInput{
			Kind: req.Kind, URL: req.URL, Weight: req.Weight, Enabled: enabled,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, variant)
}

type updateVariantRequest struct {
	URL     *string `json:"url"`
	Weight  *int32  `json:"weight"`
	Enabled *bool   `json:"enabled"`
}

func (a *LinkAPI) UpdateVariant(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	variantID, err := pathUUID(r, "variantID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateVariantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	variant, err := a.Links.UpdateVariant(r.Context(), IdentityFrom(r.Context()), linkID, variantID,
		link.UpdateVariantInput{URL: req.URL, Weight: req.Weight, Enabled: req.Enabled})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, variant)
}

func (a *LinkAPI) DeleteVariant(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	variantID, err := pathUUID(r, "variantID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteVariant(r.Context(), IdentityFrom(r.Context()), linkID, variantID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
