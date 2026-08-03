package domain

import (
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Webhooks (M42).
//
// A webhook is a standing instruction to make *this server* connect to an
// address a workspace chose, whenever something happens in that workspace. That
// sentence is the whole reason this file is careful: everything else in the
// product that takes a URL from a user hands it to a *visitor's browser*, and a
// browser and a server do not get the same answer from the same name. The
// address checks therefore run twice — once here, when the URL is registered,
// through the same tier check every destination goes through, and again in
// internal/webhook at the moment the socket is opened.
//
// **The event vocabulary is closed and small.** Six events: five for a link's
// lifecycle and one for a destination somebody was refused. It is closed for the
// reason `ClickSource` is closed — a value that reaches storage keyed on it must
// not be a value anybody can invent — and small because a receiver has to be
// able to write a handler for all of it in an afternoon. Adding one is a
// deliberate edit here, a line in the docs, and a row in the test that asserts
// the size.

// The event vocabulary. Every value a `webhooks.events` array may hold.
//
// The names match the audit log's actions where the two describe the same thing
// (`destination.blocked` is `audit.ActionDestinationBlocked` verbatim), so an
// operator reading a delivery beside an audit row is reading one vocabulary
// rather than two spellings of one.
const (
	// EventLinkCreated fires after a link exists, not before: a receiver that
	// fetches the short URL on this event must find it working.
	EventLinkCreated = "link.created"
	// EventLinkUpdated fires on any successful edit, the destination included —
	// and on one that resubmitted the same values, because the service does not
	// diff. A dashboard form posts every field on every save, so a receiver that
	// acts on this event should be idempotent about it rather than assume
	// something moved. Said out loud in docs/usage.md for the same reason.
	EventLinkUpdated = "link.updated"
	// EventLinkArchived and EventLinkRestored are the two halves of the pause
	// switch. Separate from `deleted` because archiving is reversible and a
	// receiver reconciling state needs to know which one happened.
	EventLinkArchived = "link.archived"
	EventLinkRestored = "link.restored"
	// EventLinkDeleted fires on the soft delete a person performs, which starts
	// the recovery window. Not on the purge at the end of it: that is the
	// scheduler tidying up thirty days later, and a receiver told "deleted"
	// twice for one link would double-count.
	EventLinkDeleted = "link.deleted"
	// EventDestinationBlocked is the blocked-attempt event, and it is the one
	// event here that is not about a link that exists. Somebody tried to point
	// something at a destination a tier refused; the payload names the tier, the
	// rule and the surface, and carries the attempted URL **defanged** exactly as
	// the audit record stores it.
	EventDestinationBlocked = "destination.blocked"
)

// WebhookEvents is the vocabulary, in the order a UI should list it.
//
// Ordered rather than a bare set so the checkbox list and the API's advertised
// vocabulary agree without either sorting the other's output — the lifecycle
// first, in the order a link moves through it, then the refusal.
var WebhookEvents = []string{
	EventLinkCreated,
	EventLinkUpdated,
	EventLinkArchived,
	EventLinkRestored,
	EventLinkDeleted,
	EventDestinationBlocked,
}

var webhookEventSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(WebhookEvents))
	for _, e := range WebhookEvents {
		m[e] = struct{}{}
	}
	return m
}()

// IsWebhookEvent reports whether a name is in the vocabulary.
func IsWebhookEvent(name string) bool {
	_, ok := webhookEventSet[name]
	return ok
}

// MaxWebhooksPerWorkspace bounds the list.
//
// Every enabled webhook multiplies one link write into one queued row, so this
// is also the fan-out ceiling: at the maximum, creating a link writes twenty
// delivery rows and the scheduler makes twenty outbound connections. A workspace
// wanting more than that wants a fan-out service, and should be given one URL
// that is theirs.
const MaxWebhooksPerWorkspace = 20

// MaxWebhookDescriptionLength bounds the description, in runes.
const MaxWebhookDescriptionLength = 200

// MaxWebhookDeliveryPage bounds one read of the delivery log, on the page and on
// the API. A log, not a list: the interesting rows are the recent ones, and a
// caller wanting history has the database.
const MaxWebhookDeliveryPage = 50

// Webhook is one registration as the product understands it.
//
// The secret is absent by construction. It is returned exactly once, from the
// call that generated it, and after that nothing can read it back out of this
// type — so no handler, template or log line can leak it by accident.
type Webhook struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	URL         string    `json:"url"`
	Events      []string  `json:"events"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Secret is the signing key, hex-encoded, and it is set on exactly two
	// responses: the one that created the webhook and the one that rotated its
	// secret. Empty everywhere else, and `omitempty` so a client cannot tell a
	// listing apart from a creation by the field being present and blank.
	Secret string `json:"secret,omitempty"`
}

// WebhookDelivery is one attempt to hand one event to one receiver.
//
// It is a log entry rather than a queue item as far as any reader is concerned:
// the fields that make it a queue — the payload, the lease — are not here,
// because the only questions a person asks of this row are "did it arrive",
// "what did the receiver say", and "when will it be tried again".
type WebhookDelivery struct {
	ID    uuid.UUID `json:"id"`
	Event string    `json:"event"`
	// Status is pending, delivered, failed or abandoned, as 00600's CHECK
	// spells them.
	Status   string `json:"status"`
	Attempts int32  `json:"attempts"`
	// ResponseCode is what the receiver answered, or null when there was no
	// response at all — a refused connection, a timeout, or this instance
	// declining to connect because the name resolved somewhere private.
	ResponseCode *int32 `json:"response_code"`
	// LastError is the failure in words. Empty on a delivery that succeeded
	// first time.
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

// Delivery statuses, as 00600's CHECK constraint spells them.
//
// DeliveryFailed is in the constraint and **nothing in this product writes it**.
// A delivery that has spent its attempts becomes `abandoned`, which says the same
// thing more precisely — the queue gave up, rather than one attempt failing. The
// constant is here so the vocabulary a reader meets in the schema is the
// vocabulary they meet in the code; a client should treat `failed` as it treats
// `abandoned` if it ever sees one.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
	DeliveryAbandoned = "abandoned"
)

// ValidateWebhookEvents checks a subscription against the vocabulary.
//
// An unknown name is refused rather than dropped. Silently ignoring one would
// leave somebody with a webhook they believe is subscribed to something and a
// receiver that never fires, which is the failure mode a closed vocabulary
// exists to prevent rather than to cause.
//
// The result is deduplicated and sorted, so the stored array is canonical and
// two subscriptions that mean the same thing compare equal.
func ValidateWebhookEvents(events []string) ([]string, ValidationErrors) {
	var errs ValidationErrors
	if len(events) == 0 {
		return nil, append(errs, FieldError{
			Field: "events", Code: "required",
			Message: "choose at least one event; a webhook subscribed to nothing " +
				"is a URL this server would never call",
		})
	}

	seen := make(map[string]struct{}, len(events))
	out := make([]string, 0, len(events))
	for _, e := range events {
		if !IsWebhookEvent(e) {
			errs = append(errs, FieldError{
				Field: "events", Code: "unknown_event",
				Message: fmt.Sprintf("%q is not an event this product sends; the "+
					"whole vocabulary is %v", e, WebhookEvents),
			})
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	sort.Strings(out)
	return out, nil
}

// ValidateWebhookDescription bounds the label.
func ValidateWebhookDescription(s string) ValidationErrors {
	if utf8.RuneCountInString(s) > MaxWebhookDescriptionLength {
		return ValidationErrors{{
			Field: "description", Code: "too_long",
			Message: fmt.Sprintf("description must be at most %d characters",
				MaxWebhookDescriptionLength),
		}}
	}
	return nil
}
