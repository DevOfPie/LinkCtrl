package httpx

import (
	"math"
	"net/http"
	"strconv"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// Webhooks over the API (M42).
//
// A sibling collection rather than a subresource of a link, exactly as campaigns
// and folders are: a webhook belongs to the workspace and hears about every link
// in it, so nesting it under one link would be a lie about what it subscribes to.
//
// Thin, like every other handler here — every authorization and validation
// decision is in internal/link, so the dashboard forms get identical behaviour
// by calling the same methods.
//
// **The secret appears in exactly two responses**: the 201 from creating a
// webhook, and the 200 from rotating its secret. It is not stored anywhere it can
// be read back, so a client that discards it rotates rather than re-fetches. That
// is the same shape an API key has, and for the same reason.

type createWebhookRequest struct {
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Description string   `json:"description"`
	// Enabled defaults to true when omitted, which is what a webhook somebody
	// just registered should be. A pointer, so "false" and "absent" stay
	// different.
	Enabled *bool `json:"enabled"`
}

type updateWebhookRequest struct {
	URL         *string  `json:"url"`
	Events      []string `json:"events"`
	Description *string  `json:"description"`
	Enabled     *bool    `json:"enabled"`
}

func (a *LinkAPI) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := a.Links.Webhooks(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The vocabulary travels with the answer, the way ListRules advertises the
	// condition vocabulary and GetSplit the kinds: a client building a
	// subscription editor discovers what it may subscribe to from the response
	// rather than from this file, and a vocabulary that grew would reach it
	// without a release note.
	WriteJSON(w, http.StatusOK, map[string]any{
		"webhooks": hooks,
		"events":   domain.WebhookEvents,
		"signature": map[string]any{
			"version":           webhook.SignatureVersion,
			"algorithm":         "HMAC-SHA256",
			"signature_header":  webhook.HeaderSignature,
			"timestamp_header":  webhook.HeaderTimestamp,
			"event_header":      webhook.HeaderEvent,
			"delivery_header":   webhook.HeaderDelivery,
			"signed_payload":    "<timestamp> + \".\" + <raw request body>",
			"key":               "the secret string exactly as it was shown to you",
			"max_attempts":      webhook.MaxAttempts,
			"documentation_url": "/docs/usage.md#webhooks",
		},
	})
}

func (a *LinkAPI) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req createWebhookRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	hook, err := a.Links.CreateWebhook(r.Context(), IdentityFrom(r.Context()),
		link.CreateWebhookInput{
			URL: req.URL, Events: req.Events,
			Description: req.Description, Enabled: enabled,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, hook)
}

func (a *LinkAPI) GetWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	hook, err := a.Links.Webhook(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, hook)
}

func (a *LinkAPI) UpdateWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateWebhookRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	hook, err := a.Links.UpdateWebhook(r.Context(), IdentityFrom(r.Context()), id,
		link.UpdateWebhookInput{
			URL: req.URL, Events: req.Events,
			Description: req.Description, Enabled: req.Enabled,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, hook)
}

func (a *LinkAPI) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteWebhook(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RotateWebhookSecret mints a new signing secret and returns it once.
//
// A POST because it changes something, and because the new secret is the
// response body: a GET that minted a credential would be one a browser could be
// made to fetch.
func (a *LinkAPI) RotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	hook, err := a.Links.RotateWebhookSecret(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, hook)
}

// ListWebhookDeliveries answers what a webhook's recent attempts did.
//
// The payload is not returned. It is the event body, it is already whatever the
// receiver was sent, and returning it would make this endpoint a way to read
// every link change in the workspace through a permission granted for managing
// integrations.
func (a *LinkAPI) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "webhookID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var limit int32
	if raw := r.URL.Query().Get("limit"); raw != "" {
		// Range-checked before narrowing, the same trap as the link list:
		// ?limit=4294967297 would otherwise truncate to int32(1) and answer one
		// delivery, and ?limit=2147483648 would wrap to a negative int32.
		// Out-of-range values fall through to the service clamp against
		// domain.MaxWebhookDeliveryPage rather than erroring.
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= math.MaxInt32 {
			limit = int32(n) //nolint:gosec // G109: range-checked on the line above
		}

	}
	deliveries, err := a.Links.WebhookDeliveries(r.Context(), IdentityFrom(r.Context()), id, limit)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}
