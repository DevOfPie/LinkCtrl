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
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// The webhooks page (M42).
//
// Same shape as the campaigns and folders pages — forms that each submit on
// their own, htmx as the enhancement rather than the mechanism — so everything
// here works with scripting off.
//
// **The page shows the delivery log beside the registration**, and that is the
// part worth arguing for. A webhook is the one feature in this product whose
// failure is entirely invisible from the dashboard otherwise: the events are
// queued, the deliveries fail, and the workspace's own view of the world does not
// change at all. Somebody debugging "we stopped getting events" needs the status,
// the attempt count and the response code, and needs them without a database.
//
// **The secret is shown once**, on the response to the write that produced it,
// exactly as an API key is. Nothing reads it back afterwards.

// webhookRow is one rendered registration.
type webhookRow struct {
	ID          string
	URL         string
	Description string
	Events      []string
	Enabled     bool
	CreatedAt   string
	// EventChoices is the whole vocabulary with this row's subscription ticked,
	// for the inline editor. Computed here rather than with a template helper
	// asking "is this name in that slice": a template that can do set membership
	// is a template that will grow logic, and the answer is one loop in Go.
	EventChoices []webhookEventChoice
	// Editing marks the row whose form is open.
	Editing bool
	// Deliveries is the recent attempt log, newest first. Loaded only for the
	// row whose panel is open, so the page costs one query and not one per
	// registration.
	Deliveries []webhookDeliveryRow
	ShowLog    bool
}

// webhookDeliveryRow is one attempt, as the page reads it.
type webhookDeliveryRow struct {
	Event  string
	Status string
	// Attempts and Code are what somebody debugging actually reads.
	Attempts int32
	Code     string
	Error    string
	When     string
	Next     string
	// OK marks a delivered row, so the list can shade the failures without the
	// template comparing strings.
	OK bool
}

// webhookEventChoice is one checkbox on the subscription form.
type webhookEventChoice struct {
	Name    string
	Checked bool
}

type webhooksPageData struct {
	shell
	Rows  []webhookRow
	Count int
	// Events is the whole vocabulary, for the create form.
	Events      []webhookEventChoice
	MaxWebhooks int
	MaxAttempts int
	Retention   int

	// Secret is shown exactly once, immediately after the write that minted it.
	// It is carried in the response body and never in a URL — a redirect with it
	// in the query string would put it in the browser history and in every
	// access log on the way.
	Secret string
	// SecretFor names the webhook the secret above belongs to.
	SecretFor string

	// The create form's sticky values, so a refusal does not empty the boxes.
	FormURL         string
	FormDescription string

	EditingID string
	// OpenLogID is the registration whose delivery log is expanded.
	OpenLogID string

	CanRead  bool
	CanWrite bool

	Notice string
	Error  string
}

func (h *Web) loadWebhooksPage(w http.ResponseWriter, r *http.Request) (webhooksPageData, bool) {
	actor := IdentityFrom(r.Context())

	data := webhooksPageData{
		shell:           h.shell(r, "Webhooks", "webhooks"),
		MaxWebhooks:     domain.MaxWebhooksPerWorkspace,
		MaxAttempts:     webhook.MaxAttempts,
		Retention:       h.webhookRetentionDays(),
		FormURL:         r.URL.Query().Get("url"),
		FormDescription: r.URL.Query().Get("description"),
		EditingID:       strings.TrimSpace(r.URL.Query().Get("edit")),
		OpenLogID:       strings.TrimSpace(r.URL.Query().Get("log")),
		CanRead:         actor.Can(link.PermWebhooksRead),
		CanWrite:        actor.Can(link.PermWebhooksWrite),
	}
	data.Notice = webhookNotice(r.URL.Query().Get("webhook"))

	// Selected on the create form when the query string carries them, so a
	// refusal keeps the boxes the person ticked.
	chosen := map[string]bool{}
	for _, e := range r.URL.Query()["events"] {
		chosen[e] = true
	}
	for _, e := range domain.WebhookEvents {
		data.Events = append(data.Events, webhookEventChoice{Name: e, Checked: chosen[e]})
	}

	if !data.CanRead {
		// A reader who cannot read gets the page with no list rather than a 403,
		// so the nav does not lead somewhere that refuses. The forms are hidden
		// by CanWrite and the service refuses anyway.
		return data, true
	}

	hooks, err := h.Links.Webhooks(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return webhooksPageData{}, false
	}
	data.Count = len(hooks)
	data.Rows = make([]webhookRow, 0, len(hooks))
	for _, hook := range hooks {
		row := webhookRow{
			ID: hook.ID.String(), URL: hook.URL, Description: hook.Description,
			Events: hook.Events, Enabled: hook.Enabled,
			CreatedAt: hook.CreatedAt.UTC().Format("2006-01-02"),
			Editing:   data.EditingID == hook.ID.String(),
			ShowLog:   data.OpenLogID == hook.ID.String(),
		}
		subscribed := make(map[string]bool, len(hook.Events))
		for _, e := range hook.Events {
			subscribed[e] = true
		}
		for _, e := range domain.WebhookEvents {
			row.EventChoices = append(row.EventChoices,
				webhookEventChoice{Name: e, Checked: subscribed[e]})
		}
		if row.ShowLog {
			deliveries, derr := h.Links.WebhookDeliveries(r.Context(), actor, hook.ID, webhookLogRows)
			if derr != nil {
				h.webError(w, r, derr)
				return webhooksPageData{}, false
			}
			row.Deliveries = renderDeliveries(deliveries)
		}
		data.Rows = append(data.Rows, row)
	}
	return data, true
}

// webhookLogRows is how many attempts the expanded panel shows. Twenty: enough
// to see a pattern, short enough that the page stays a page.
const webhookLogRows = 20

func (h *Web) webhookRetentionDays() int {
	if h.Config.Webhooks.RetentionDays > 0 {
		return h.Config.Webhooks.RetentionDays
	}
	return webhook.DefaultRetentionDays
}

func renderDeliveries(deliveries []domain.WebhookDelivery) []webhookDeliveryRow {
	out := make([]webhookDeliveryRow, 0, len(deliveries))
	for _, d := range deliveries {
		row := webhookDeliveryRow{
			Event: d.Event, Status: d.Status, Attempts: d.Attempts,
			Error: d.LastError, OK: d.Status == domain.DeliveryDelivered,
			When: d.CreatedAt.UTC().Format("2006-01-02 15:04"),
		}
		if d.ResponseCode != nil {
			row.Code = strconv.FormatInt(int64(*d.ResponseCode), 10)
		} else {
			// The most informative state, and the one a blank cell hides: the
			// receiver said nothing at all.
			row.Code = "no answer"
		}
		if d.Status == domain.DeliveryPending && d.NextAttemptAt != nil {
			row.Next = d.NextAttemptAt.UTC().Format("15:04")
		}
		out = append(out, row)
	}
	return out
}

func (h *Web) WebhooksPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadWebhooksPage(w, r)
	if !ok {
		return
	}
	h.renderWebhooks(w, r, http.StatusOK, data)
}

func (h *Web) renderWebhooks(
	w http.ResponseWriter, r *http.Request, status int, data webhooksPageData,
) {
	if isHTMX(r) {
		h.renderPartial(w, r, "webhooks", "webhook_panel", data)
		return
	}
	h.render(w, r, status, "webhooks", data)
}

func (h *Web) WebhookCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	hook, err := h.Links.CreateWebhook(r.Context(), IdentityFrom(r.Context()),
		link.CreateWebhookInput{
			URL:         r.PostFormValue("url"),
			Events:      r.PostForm["events"],
			Description: r.PostFormValue("description"),
			Enabled:     true,
		})
	if err != nil {
		h.finishWebhookAction(w, r, "created", "", "", err)
		return
	}
	// The secret travels in the rendered response, never in the redirect. See
	// webhooksPageData.Secret.
	h.finishWebhookAction(w, r, "created", hook.Secret, hook.ID.String(), nil)
}

func (h *Web) WebhookUpdate(w http.ResponseWriter, r *http.Request) {
	h.webhookAction(w, r, "updated", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		desc := r.PostFormValue("description")
		url := r.PostFormValue("url")
		in := link.UpdateWebhookInput{Description: &desc}
		if url != "" {
			in.URL = &url
		}
		// A form always posts every checkbox it rendered, so an empty set is a
		// real request to subscribe to nothing — which the service refuses with
		// a message rather than storing.
		events := r.PostForm["events"]
		if events == nil {
			events = []string{}
		}
		in.Events = events
		_, err := h.Links.UpdateWebhook(ctx, IdentityFrom(ctx), id, in)
		return err
	})
}

// WebhookToggle is the pause switch, on its own form.
//
// Its own action rather than a field on the edit form, for the reason the rule
// and split toggles have one: switching a misbehaving receiver off is what
// somebody reaches for first, and it must not require opening an editor.
func (h *Web) WebhookToggle(w http.ResponseWriter, r *http.Request) {
	h.webhookAction(w, r, "toggled", func(ctx context.Context, id uuid.UUID) error {
		existing, err := h.Links.Webhook(ctx, IdentityFrom(ctx), id)
		if err != nil {
			return err
		}
		flipped := !existing.Enabled
		_, err = h.Links.UpdateWebhook(ctx, IdentityFrom(ctx), id,
			link.UpdateWebhookInput{Enabled: &flipped})
		return err
	})
}

func (h *Web) WebhookRotate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	hook, err := h.Links.RotateWebhookSecret(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		h.finishWebhookAction(w, r, "rotated", "", "", err)
		return
	}
	h.finishWebhookAction(w, r, "rotated", hook.Secret, hook.ID.String(), nil)
}

func (h *Web) WebhookDelete(w http.ResponseWriter, r *http.Request) {
	h.webhookAction(w, r, "deleted", func(ctx context.Context, id uuid.UUID) error {
		return h.Links.DeleteWebhook(ctx, IdentityFrom(ctx), id)
	})
}

func (h *Web) webhookAction(
	w http.ResponseWriter, r *http.Request, marker string,
	do func(ctx context.Context, id uuid.UUID) error,
) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	h.finishWebhookAction(w, r, marker, "", "", do(r.Context(), id))
}

// finishWebhookAction is the one place a webhook write turns into a response, on
// the shape finishCampaignAction uses: a refusal comes back on the page it was
// made from, and anything that is not a validation error is a genuine failure.
//
// The one difference is the secret. A write that minted one renders the page
// directly instead of redirecting, because the value exists only in this
// response — a redirect would either lose it or put it in a URL.
func (h *Web) finishWebhookAction(
	w http.ResponseWriter, r *http.Request, marker, secret, secretFor string, err error,
) {
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadWebhooksPage(w, r)
		if !ok {
			return
		}
		data.Error = ve[0].Message
		data.Notice = ""
		data.FormURL = r.PostFormValue("url")
		data.FormDescription = r.PostFormValue("description")
		chosen := map[string]bool{}
		for _, e := range r.PostForm["events"] {
			chosen[e] = true
		}
		for i := range data.Events {
			data.Events[i].Checked = chosen[data.Events[i].Name]
		}
		h.renderWebhooks(w, r, http.StatusUnprocessableEntity, data)
		return
	}

	if secret != "" || isHTMX(r) {
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = ""
		data, ok := h.loadWebhooksPage(w, r2)
		if !ok {
			return
		}
		data.Notice = webhookNotice(marker)
		data.Secret, data.SecretFor = secret, secretFor
		h.renderWebhooks(w, r, http.StatusOK, data)
		return
	}
	seeOther(w, r, "/webhooks?webhook="+marker)
}

func webhookNotice(marker string) string {
	switch marker {
	case "created":
		return "Webhook registered. Copy the signing secret now — it is shown once " +
			"and cannot be read back."
	case "updated":
		return "Webhook updated."
	case "toggled":
		return "Webhook switched. A disabled webhook queues nothing; deliveries " +
			"already queued still go out."
	case "rotated":
		return "Signing secret rotated. The previous secret stops verifying " +
			"immediately — update your receiver now."
	case "deleted":
		return "Webhook removed, with its delivery log."
	default:
		return ""
	}
}
