package httpx

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The split-testing half of the link detail page (M36).
//
// Three actions rather than one form, for the reason the rule section has three:
// adding an arm and parking one mid-test are different enough operations that
// making the second go through the first would mean opening an editor to stop a
// misbehaving destination. The toggle is the feature flag — it is the control
// somebody reaches for when an arm starts 500ing, and it must be one click.
//
// The form offers `weighted` and `sequential` and, separately, a fallback. It
// does not offer a way to change an existing split's kind, and that absence is
// deliberate rather than missing: switching a running test from percentages to a
// rotation changes what its own history means, so the answer is to remove the
// arms and start a test that is honestly a new one.

// VariantCreate adds an arm from the link detail page.
func (h *Web) VariantCreate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	in := link.CreateVariantInput{
		Kind:    strings.TrimSpace(r.PostFormValue("variant_kind")),
		URL:     strings.TrimSpace(r.PostFormValue("variant_url")),
		Enabled: r.PostFormValue("variant_enabled") != "",
	}
	if raw := strings.TrimSpace(r.PostFormValue("variant_weight")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 || n > domain.MaxDestinationWeight {
			h.rerenderWithSplitError(w, r, "A weight must be a whole number between 0 and "+
				strconv.Itoa(domain.MaxDestinationWeight)+".")
			return
		}
		in.Weight = int32(n) //nolint:gosec // G109: range-checked on the line above
	} else if in.Kind == domain.RuleKindWeighted {
		// A weighted arm with no weight typed gets the column's default rather
		// than zero. Zero would mean "receives nothing", which is the opposite of
		// what somebody adding an arm and leaving the box alone intends.
		in.Weight = 100
	}

	if _, err := h.Links.CreateVariant(r.Context(), IdentityFrom(r.Context()), id, in); err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			h.rerenderWithSplitError(w, r, ruleErrorText(ve))
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links/"+id.String()+"?split=added")
}

// VariantToggle switches an arm on or off. This is the feature flag.
func (h *Web) VariantToggle(w http.ResponseWriter, r *http.Request) {
	h.variantAction(w, r, "toggled", func(ctx context.Context, linkID, variantID uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		enabled := r.PostFormValue("enabled") == "1"
		_, err := h.Links.UpdateVariant(ctx, IdentityFrom(ctx),
			linkID, variantID, link.UpdateVariantInput{Enabled: &enabled})
		return err
	})
}

// VariantDelete removes an arm.
func (h *Web) VariantDelete(w http.ResponseWriter, r *http.Request) {
	h.variantAction(w, r, "deleted", func(ctx context.Context, linkID, variantID uuid.UUID) error {
		return h.Links.DeleteVariant(ctx, IdentityFrom(ctx), linkID, variantID)
	})
}

func (h *Web) variantAction(
	w http.ResponseWriter, r *http.Request, notice string,
	do func(ctx context.Context, linkID, variantID uuid.UUID) error,
) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	variantID, err := pathUUID(r, "variantID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := do(r.Context(), id, variantID); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links/"+id.String()+"?split="+notice)
}

// rerenderWithSplitError puts the page back with the refusal above the split
// form.
func (h *Web) rerenderWithSplitError(w http.ResponseWriter, r *http.Request, message string) {
	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	data.Error = message
	h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
}

// splitNotice turns the ?split= marker into a sentence.
func splitNotice(marker string) string {
	switch marker {
	case "added":
		return "Split-test arm added. It takes effect on the next request; cached " +
			"snapshots for this link were cleared on every replica."
	case "deleted":
		return "Split-test arm removed. Clicks already attributed to it are kept, " +
			"and the breakdown reports them as a destination that no longer exists."
	case "toggled":
		return "Split-test arm updated. A disabled arm receives nothing and the " +
			"others re-share its traffic."
	default:
		return ""
	}
}

// splitHelp is what the page says about the two kinds and about what a split
// deliberately does not do.
const splitHelp = "A weighted split sends each visitor to one arm at random, in " +
	"proportion to its weight. A sequential split visits the arms strictly in " +
	"turn, using a durable counter shared by every replica. There is no " +
	"stickiness: the same person following the link twice may see two arms, " +
	"because each click is an independent trial and which arm converted is " +
	"answered by the per-destination breakdown below."
