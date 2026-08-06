package link

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Webhook registration (M42).
//
// **This file is here because of one test.** A webhook URL is a destination
// somebody writes, so it goes through `Service.checkDestination` — the one door
// every destination-writing surface goes through — and that door is unexported
// and lives in this package. `surfaces_test.go` anticipated exactly this
// milestone by name: *"a later milestone adds a surface that writes a
// destination — a routing rule's target (M34), a split-test variant (M36), a
// webhook URL (M42) — and calls ValidateDestination, because that is what every
// existing call site appears to do."* Putting the CRUD anywhere else would have
// meant either exporting the door or reaching past it, and both are the bypass.
//
// **Registration-time validation is necessary and not sufficient.** Everything
// the tiers refuse here is refused for the same reasons M30 gives, plus one this
// surface does not share with any other: a webhook URL is dialled by *this
// process*, not by a visitor's browser. A name that resolves to a public address
// while somebody is typing it into the form can resolve to 169.254.169.254 by
// the time the scheduler opens a socket, and no check made here can see that.
// internal/webhook checks the resolved address again at connect, and the posture
// is stated in decisions.md rather than inherited.
//
// **Delivery is not here.** This package registers and audits; internal/webhook
// signs, dials and retries. The two never import each other — this one hands
// events to an Emitter interface it defines itself, the way it already does for
// the cache, the feed and the notifier.

// Permissions this file enforces.
//
// Their own rather than `links.*`, which is where this parts company with QR
// codes and campaigns (D75). Those are properties of a link, so whoever may edit
// the link may edit them. A webhook is a standing instruction to make this
// server connect somewhere, which is a different power from editing what a
// visitor's browser is sent to — see decisions.md.
const (
	PermWebhooksRead  = "webhooks.read"
	PermWebhooksWrite = "webhooks.write"
)

// webhookSecretBytes is the entropy in a signing secret.
//
// Thirty-two, matching a session token and an invitation token, and matching the
// block size HMAC-SHA256 keys at. A receiver stores it as the hex string this
// returns; a shorter key would be a key somebody could grind against a captured
// payload, and the payload is by definition something they were sent.
const webhookSecretBytes = 32

// CreateWebhookInput is a new registration.
type CreateWebhookInput struct {
	// URL is where events are POSTed. Judged by every tier before the row
	// exists, and judged again at the address the name resolves to before any
	// socket is opened.
	URL string
	// Events is the subscription, from domain.WebhookEvents.
	Events []string
	// Description is what an operator calls this webhook in the list.
	Description string
	// Enabled is whether the webhook receives anything. A disabled webhook is
	// skipped by the fan-out query rather than delivered and discarded, so
	// switching one off stops queueing rows as well as stopping deliveries.
	Enabled bool
}

// UpdateWebhookInput is a partial update; nil fields are left alone.
type UpdateWebhookInput struct {
	URL         *string
	Events      []string
	Description *string
	Enabled     *bool
}

// Webhooks lists a workspace's registrations.
func (s *Service) Webhooks(ctx context.Context, actor *auth.Identity) ([]domain.Webhook, error) {
	if !actor.Can(PermWebhooksRead) {
		return nil, fmt.Errorf("%w: reading webhooks requires %s", domain.ErrForbidden, PermWebhooksRead)
	}
	rows, err := s.q.ListWebhooks(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	out := make([]domain.Webhook, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Webhook{
			ID: r.ID, WorkspaceID: r.WorkspaceID, URL: r.Url, Events: r.Events,
			Description: r.Description, Enabled: r.Enabled,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// Webhook reads one registration.
func (s *Service) Webhook(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*domain.Webhook, error) {
	if !actor.Can(PermWebhooksRead) {
		return nil, fmt.Errorf("%w: reading webhooks requires %s", domain.ErrForbidden, PermWebhooksRead)
	}
	r, err := s.q.GetWebhook(ctx, dbgen.GetWebhookParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load webhook: %w", err)
	}
	return &domain.Webhook{
		ID: r.ID, WorkspaceID: r.WorkspaceID, URL: r.Url, Events: r.Events,
		Description: r.Description, Enabled: r.Enabled,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

// WebhookDeliveries reads one webhook's recent attempts.
//
// Scoped through the webhook's workspace in the query itself, so a delivery
// belonging to somebody else's registration is not readable by guessing an id.
func (s *Service) WebhookDeliveries(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, limit int32,
) ([]domain.WebhookDelivery, error) {
	if !actor.Can(PermWebhooksRead) {
		return nil, fmt.Errorf("%w: reading webhooks requires %s", domain.ErrForbidden, PermWebhooksRead)
	}
	// The webhook is read first, so a delivery list for a registration in
	// another workspace is a 404 rather than an empty list — the same reasoning
	// GetSplit gives, and the same information about somebody else's tenancy at
	// stake.
	if _, err := s.Webhook(ctx, actor, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > domain.MaxWebhookDeliveryPage {
		limit = domain.MaxWebhookDeliveryPage
	}
	rows, err := s.q.ListWebhookDeliveries(ctx, dbgen.ListWebhookDeliveriesParams{
		WebhookID: id, WorkspaceID: actor.WorkspaceID, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list webhook deliveries: %w", err)
	}
	out := make([]domain.WebhookDelivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.WebhookDelivery{
			ID: r.ID, Event: r.Event, Status: r.Status, Attempts: r.Attempts,
			ResponseCode: r.ResponseCode, LastError: r.LastError,
			NextAttemptAt: r.NextAttemptAt, CreatedAt: r.CreatedAt,
			CompletedAt: r.CompletedAt,
		})
	}
	return out, nil
}

// CreateWebhook registers a URL and mints its signing secret.
//
// The secret is returned exactly once, in the value this call produces. Nothing
// reads it back afterwards — ListWebhooks and GetWebhook do not select the
// column — so a receiver that loses it rotates rather than looks it up.
func (s *Service) CreateWebhook(
	ctx context.Context, actor *auth.Identity, in CreateWebhookInput,
) (*domain.Webhook, error) {
	if !actor.Can(PermWebhooksWrite) {
		return nil, fmt.Errorf("%w: managing webhooks requires %s", domain.ErrForbidden, PermWebhooksWrite)
	}

	var errs domain.ValidationErrors
	events, eventErrs := domain.ValidateWebhookEvents(in.Events)
	errs = append(errs, eventErrs...)
	errs = append(errs, domain.ValidateWebhookDescription(in.Description)...)

	// The full M30 tier check, through the one door every destination-writing
	// surface goes through — not ValidateDestination, which would inherit the
	// SSRF refusals and skip the embedded list, the operator's blocklist, the
	// heuristics, the opt-in feed and the `destination.blocked` audit record.
	//
	// The unappealable tier is what refuses http://169.254.169.254/ here, and it
	// has no override switch by construction: TestUnappealableTierHasNoOverrideSwitch
	// walks DestinationPolicy by reflection and fails when it grows a field.
	normalized, err := s.checkDestination(ctx, actor, in.URL, surfaceWebhook)
	if err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			errs = append(errs, ve...)
		} else {
			return nil, err
		}
	}

	count, err := s.q.CountWebhooks(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("count webhooks: %w", err)
	}
	if count >= domain.MaxWebhooksPerWorkspace {
		errs = append(errs, domain.FieldError{
			Field: "url", Code: "too_many",
			Message: fmt.Sprintf("a workspace may register at most %d webhooks; "+
				"every enabled one turns each link write into another outbound "+
				"connection", domain.MaxWebhooksPerWorkspace),
		})
	}

	if len(errs) > 0 {
		return nil, errs
	}

	secret, err := newWebhookSecret()
	if err != nil {
		return nil, err
	}

	row, err := s.q.CreateWebhook(ctx, dbgen.CreateWebhookParams{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: actor.WorkspaceID,
		Url: normalized, Secret: secret, Events: events,
		Description: in.Description, Enabled: in.Enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}

	s.recordWebhookEvent(ctx, actor, audit.ActionWebhookCreated, row.ID, map[string]any{
		"url_defanged": Defang(normalized),
		"events":       events,
		"enabled":      row.Enabled,
	})

	return &domain.Webhook{
		ID: row.ID, WorkspaceID: row.WorkspaceID, URL: row.Url, Events: row.Events,
		Description: row.Description, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Secret: hex.EncodeToString(secret),
	}, nil
}

// UpdateWebhook changes a registration's URL, subscription, label or switch.
func (s *Service) UpdateWebhook(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, in UpdateWebhookInput,
) (*domain.Webhook, error) {
	if !actor.Can(PermWebhooksWrite) {
		return nil, fmt.Errorf("%w: managing webhooks requires %s", domain.ErrForbidden, PermWebhooksWrite)
	}

	var errs domain.ValidationErrors
	var events []string
	if in.Events != nil {
		var eventErrs domain.ValidationErrors
		events, eventErrs = domain.ValidateWebhookEvents(in.Events)
		errs = append(errs, eventErrs...)
	}
	if in.Description != nil {
		errs = append(errs, domain.ValidateWebhookDescription(*in.Description)...)
	}

	// Re-checked on every edit, not only at creation. A URL that was acceptable
	// when it was registered and is now on the operator's blocklist must not
	// survive an edit that happened to change something else.
	var normalized *string
	if in.URL != nil {
		got, err := s.checkDestination(ctx, actor, *in.URL, surfaceWebhook)
		if err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, err
			}
		} else {
			normalized = &got
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	row, err := s.q.UpdateWebhook(ctx, dbgen.UpdateWebhookParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
		Url: normalized, Events: events, Description: in.Description, Enabled: in.Enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("update webhook: %w", err)
	}

	metadata := map[string]any{"events": row.Events, "enabled": row.Enabled}
	if normalized != nil {
		metadata["url_defanged"] = Defang(*normalized)
	}
	s.recordWebhookEvent(ctx, actor, audit.ActionWebhookUpdated, row.ID, metadata)

	return &domain.Webhook{
		ID: row.ID, WorkspaceID: row.WorkspaceID, URL: row.Url, Events: row.Events,
		Description: row.Description, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// RotateWebhookSecret mints a new signing secret and returns it once.
//
// There is no overlap window, and that is deliberate: two valid secrets at once
// would mean a receiver that has been compromised keeps verifying for as long as
// the window lasts, which is the opposite of what somebody rotating a leaked
// secret wants. The cost is that a receiver must be updated promptly, and the
// audit action exists so "our verification broke at 14:03" is findable.
func (s *Service) RotateWebhookSecret(
	ctx context.Context, actor *auth.Identity, id uuid.UUID,
) (*domain.Webhook, error) {
	if !actor.Can(PermWebhooksWrite) {
		return nil, fmt.Errorf("%w: managing webhooks requires %s", domain.ErrForbidden, PermWebhooksWrite)
	}
	existing, err := s.Webhook(ctx, actor, id)
	if err != nil {
		return nil, err
	}

	secret, err := newWebhookSecret()
	if err != nil {
		return nil, err
	}
	if err := s.q.RotateWebhookSecret(ctx, dbgen.RotateWebhookSecretParams{
		ID: id, WorkspaceID: actor.WorkspaceID, Secret: secret,
	}); err != nil {
		return nil, fmt.Errorf("rotate webhook secret: %w", err)
	}

	s.recordWebhookEvent(ctx, actor, audit.ActionWebhookSecretRotated, id, nil)

	rotated := *existing
	rotated.Secret = hex.EncodeToString(secret)
	return &rotated, nil
}

// DeleteWebhook removes a registration and every delivery it recorded.
func (s *Service) DeleteWebhook(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermWebhooksWrite) {
		return fmt.Errorf("%w: managing webhooks requires %s", domain.ErrForbidden, PermWebhooksWrite)
	}
	n, err := s.q.DeleteWebhook(ctx, dbgen.DeleteWebhookParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.recordWebhookEvent(ctx, actor, audit.ActionWebhookDeleted, id, nil)
	return nil
}

// --- emitting ----------------------------------------------------------------

// emitLink queues one link-lifecycle event for every subscribed webhook.
//
// The payload is an explicit map rather than a marshalled domain.Link, and that
// is a promise rather than laziness avoided: this shape is a published interface
// that receivers parse, and a struct that grows a field on some later milestone
// would change every payload in the world without anybody deciding to. Adding a
// field here is a deliberate line, and it is documented in docs/usage.md.
//
// Tags, folders and campaigns are absent for the same reason they are absent
// from the API's link row: they are ids by the time they are useful and names by
// the time they are readable, and a receiver that wants either asks.
func (s *Service) emitLink(ctx context.Context, event string, l *domain.Link) {
	if s.events == nil || l == nil {
		return
	}
	s.events.Emit(ctx, l.WorkspaceID, event, map[string]any{
		"id":        l.ID,
		"alias":     l.Alias,
		"short_url": l.ShortURL,
		"url":       l.URL,
		"title":     l.Title,
		"status":    l.Status,
	})
}

// emitBlocked queues the blocked-attempt event.
//
// The attempted URL travels **defanged**, exactly as the audit record stores it
// and for the same reason: a receiver that pipes this into a chat room, a ticket
// or a console must not be handing somebody a live link to the thing that was
// refused. The tier and the rule travel as themselves, because a receiver
// deciding whether to page somebody needs to know which tier said no.
func (s *Service) emitBlocked(
	ctx context.Context, workspaceID uuid.UUID, raw string,
	surface destinationSurface, tier Tier, rule string,
) {
	if s.events == nil {
		return
	}
	s.events.Emit(ctx, workspaceID, domain.EventDestinationBlocked, map[string]any{
		"tier":         string(tier),
		"rule":         rule,
		"code":         tier.Code(rule),
		"surface":      string(surface),
		"url_defanged": Defang(raw),
	})
}

// newWebhookSecret mints a signing key.
func newWebhookSecret() ([]byte, error) {
	secret := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate webhook secret: %w", err)
	}
	return secret, nil
}

// recordWebhookEvent writes the audit record for a registration change.
//
// The same trade every administrative write in this package makes: the change is
// what the actor asked for, and failing it because the record could not be
// written would swap a missing log line for an action that did not happen.
func (s *Service) recordWebhookEvent(
	ctx context.Context, actor *auth.Identity, action string,
	id uuid.UUID, metadata map[string]any,
) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "webhook", TargetID: &id, Metadata: metadata,
	}); err != nil {
		s.log.Warn("webhook changed but the audit record was not written",
			slog.String("action", action), slog.Any("error", err))
	}
}
