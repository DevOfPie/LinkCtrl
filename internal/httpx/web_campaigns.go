package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The campaigns page (M41).
//
// A flat list rather than the tree the folders page renders, because a campaign
// has no parent: a link belongs to one campaign or to none. Everything else
// follows the folder page's shape — three forms, no JavaScript, htmx as the
// enhancement — so creating, editing and deleting each work with scripting off.
//
// **Every row links to its own slice of the links list.** That is the whole of
// what a campaign does, so a page listing campaigns without a way through to
// their links would be a page listing names. The "no campaign" link beside the
// count is the same question asked the other way, and it is on this page for the
// reason the folders page carries "unfiled": a list of campaigns whose counts do
// not add up to the catalogue raises "where are the rest" immediately.

// campaignRow is one rendered campaign.
type campaignRow struct {
	ID          string
	Name        string
	Slug        string
	Description string
	LinkCount   int64
	LinksURL    string
	// Schedule is the campaign's dates as a sentence, empty when it has none.
	Schedule string
	// Active is whether now falls inside the schedule. Presentation only —
	// nothing about a link changes when a campaign ends, and the page says so.
	Active bool
	// Editing marks the row whose form is open.
	Editing  bool
	StartsAt string
	EndsAt   string
}

// campaignOption is one entry of a campaign `<select>`, on the link forms.
type campaignOption struct {
	ID       string
	Label    string
	Selected bool
}

type campaignsPageData struct {
	shell
	Rows  []campaignRow
	Count int
	// NoCampaignURL is the links list filtered to the links carrying none.
	NoCampaignURL string
	MaxCampaigns  int

	// The create form's sticky values, so a refusal does not empty the boxes.
	FormName        string
	FormSlug        string
	FormDescription string
	FormStartsAt    string
	FormEndsAt      string

	EditingID string

	CanCreate bool
	CanUpdate bool
	CanDelete bool

	Notice string
	Error  string
}

func (h *Web) loadCampaignsPage(w http.ResponseWriter, r *http.Request) (campaignsPageData, bool) {
	actor := IdentityFrom(r.Context())

	campaigns, err := h.Links.Campaigns(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return campaignsPageData{}, false
	}

	data := campaignsPageData{
		shell:           h.shell(r, "Campaigns", "links"),
		Count:           len(campaigns),
		NoCampaignURL:   "/links?campaign=" + campaignFilterNone,
		MaxCampaigns:    domain.MaxCampaignsPerWorkspace,
		FormName:        r.URL.Query().Get("name"),
		FormSlug:        r.URL.Query().Get("slug"),
		FormDescription: r.URL.Query().Get("description"),
		EditingID:       strings.TrimSpace(r.URL.Query().Get("edit")),
		CanCreate:       actor.Can(link.PermCreate),
		CanUpdate:       actor.Can(link.PermUpdate),
		CanDelete:       actor.Can(link.PermDelete),
	}
	data.Notice = campaignNotice(r.URL.Query().Get("campaign"))

	now := time.Now()
	data.Rows = make([]campaignRow, 0, len(campaigns))
	for _, c := range campaigns {
		row := campaignRow{
			ID: c.ID.String(), Name: c.Name, Slug: c.Slug,
			Description: c.Description, LinkCount: c.LinkCount,
			LinksURL: "/links?campaign=" + c.ID.String(),
			Schedule: campaignSchedule(c),
			Active:   c.Active(now),
			Editing:  data.EditingID == c.ID.String(),
		}
		if c.StartsAt != nil {
			row.StartsAt = c.StartsAt.UTC().Format("2006-01-02")
		}
		if c.EndsAt != nil {
			row.EndsAt = c.EndsAt.UTC().Format("2006-01-02")
		}
		data.Rows = append(data.Rows, row)
	}
	return data, true
}

// campaignSchedule turns the two bounds into a sentence, or nothing.
func campaignSchedule(c domain.Campaign) string {
	const day = "2 Jan 2006"
	switch {
	case c.StartsAt != nil && c.EndsAt != nil:
		return c.StartsAt.UTC().Format(day) + " to " + c.EndsAt.UTC().Format(day)
	case c.StartsAt != nil:
		return "from " + c.StartsAt.UTC().Format(day)
	case c.EndsAt != nil:
		return "until " + c.EndsAt.UTC().Format(day)
	default:
		return ""
	}
}

func (h *Web) CampaignsPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadCampaignsPage(w, r)
	if !ok {
		return
	}
	h.renderCampaigns(w, r, http.StatusOK, data)
}

func (h *Web) renderCampaigns(
	w http.ResponseWriter, r *http.Request, status int, data campaignsPageData,
) {
	if isHTMX(r) {
		h.renderPartial(w, r, "campaigns", "campaign_panel", data)
		return
	}
	h.render(w, r, status, "campaigns", data)
}

func (h *Web) CampaignCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	in := link.CreateCampaignInput{
		Name:        r.PostFormValue("name"),
		Slug:        r.PostFormValue("slug"),
		Description: r.PostFormValue("description"),
	}
	starts, ends, err := formSchedule(r)
	if err != nil {
		h.finishCampaignAction(w, r, "created", err)
		return
	}
	in.StartsAt, in.EndsAt = starts, ends
	_, err = h.Links.CreateCampaign(r.Context(), IdentityFrom(r.Context()), in)
	h.finishCampaignAction(w, r, "created", err)
}

func (h *Web) CampaignUpdate(w http.ResponseWriter, r *http.Request) {
	h.campaignAction(w, r, "updated", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		name := r.PostFormValue("name")
		slug := r.PostFormValue("slug")
		desc := r.PostFormValue("description")
		in := link.UpdateCampaignInput{Name: &name, Slug: &slug, Description: &desc}
		starts, ends, err := formSchedule(r)
		if err != nil {
			return err
		}
		// A form always posts both date boxes, so an empty one is a request to
		// remove the bound rather than a field that was left out. That is the
		// opposite of the API's default and it is why the clear flags exist: the
		// two surfaces express the same three states in the dialect each has.
		in.StartsAt, in.ClearStartsAt = starts, starts == nil
		in.EndsAt, in.ClearEndsAt = ends, ends == nil
		_, err = h.Links.UpdateCampaign(ctx, IdentityFrom(ctx), id, in)
		return err
	})
}

func (h *Web) CampaignDelete(w http.ResponseWriter, r *http.Request) {
	h.campaignAction(w, r, "deleted", func(ctx context.Context, id uuid.UUID) error {
		return h.Links.DeleteCampaign(ctx, IdentityFrom(ctx), id)
	})
}

func (h *Web) campaignAction(
	w http.ResponseWriter, r *http.Request, marker string,
	do func(ctx context.Context, id uuid.UUID) error,
) {
	id, err := pathUUID(r, "campaignID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	h.finishCampaignAction(w, r, marker, do(r.Context(), id))
}

// finishCampaignAction is the one place a campaign write turns into a response,
// on exactly the shape finishFolderAction uses: a refusal comes back on the page
// it was made from, and anything that is not a validation error is a genuine
// failure.
func (h *Web) finishCampaignAction(w http.ResponseWriter, r *http.Request, marker string, err error) {
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadCampaignsPage(w, r)
		if !ok {
			return
		}
		data.Error = ve[0].Message
		data.Notice = ""
		data.FormName = r.PostFormValue("name")
		data.FormSlug = r.PostFormValue("slug")
		data.FormDescription = r.PostFormValue("description")
		data.FormStartsAt = r.PostFormValue("starts_at")
		data.FormEndsAt = r.PostFormValue("ends_at")
		h.renderCampaigns(w, r, http.StatusUnprocessableEntity, data)
		return
	}
	if isHTMX(r) {
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = ""
		data, ok := h.loadCampaignsPage(w, r2)
		if !ok {
			return
		}
		data.Notice = campaignNotice(marker)
		h.renderPartial(w, r, "campaigns", "campaign_panel", data)
		return
	}
	seeOther(w, r, "/campaigns?campaign="+marker)
}

// formSchedule reads the two date boxes. Empty is no bound; anything that is not
// a date is a refusal the reader can act on rather than a silently ignored box.
func formSchedule(r *http.Request) (starts, ends *time.Time, err error) {
	parse := func(field string) (*time.Time, error) {
		raw := strings.TrimSpace(r.PostFormValue(field))
		if raw == "" {
			return nil, nil
		}
		// `<input type="date">` posts YYYY-MM-DD and nothing else. Read as UTC,
		// which is what every other date in this product means.
		t, perr := time.Parse("2006-01-02", raw)
		if perr != nil {
			return nil, domain.ValidationErrors{{
				Field: field, Code: "invalid", Message: "that is not a date",
			}}
		}
		return &t, nil
	}
	if starts, err = parse("starts_at"); err != nil {
		return nil, nil, err
	}
	if ends, err = parse("ends_at"); err != nil {
		return nil, nil, err
	}
	return starts, ends, nil
}

// campaignOptions renders the workspace's campaigns as `<select>` options for
// the link forms, marking the one a link already carries.
func campaignOptions(campaigns []domain.Campaign, selected *uuid.UUID) []campaignOption {
	out := make([]campaignOption, 0, len(campaigns))
	for _, c := range campaigns {
		out = append(out, campaignOption{
			ID:       c.ID.String(),
			Label:    c.Name,
			Selected: selected != nil && *selected == c.ID,
		})
	}
	return out
}

func campaignNotice(marker string) string {
	switch marker {
	case "created":
		return "Campaign created."
	case "updated":
		return "Campaign updated. The links carrying it did not change."
	case "deleted":
		return "Campaign deleted. Every link that carried it is still here — it now " +
			"carries no campaign."
	default:
		return ""
	}
}
