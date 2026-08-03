// Package domain holds the types the services exchange.
//
// These are deliberately separate from the sqlc-generated structs. Generated
// types describe table shape, change whenever a column does, and carry driver
// concerns; domain types describe what the product means and are what handlers
// and templates see. The mapping between them lives in the service layer, so a
// column rename does not ripple into the API.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. Handlers map these to status codes in exactly one place, so
// a service can signal "not found" without knowing about HTTP.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
	ErrValidation   = errors.New("validation failed")
	// ErrNotImplemented marks Phase 2 fields that exist in the schema and are
	// rejected with a clear message rather than silently ignored. Silently
	// accepting a field that does nothing is worse than refusing it.
	ErrNotImplemented = errors.New("not implemented in this version")
)

// FieldError is a per-field validation failure, so a form can highlight the
// offending input rather than showing one opaque message.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationErrors is a collection of field errors. Like config validation,
// every problem is reported at once rather than one per round trip.
type ValidationErrors []FieldError

func (v ValidationErrors) Error() string {
	if len(v) == 0 {
		return "validation failed"
	}
	if len(v) == 1 {
		return v[0].Field + ": " + v[0].Message
	}
	return v[0].Field + ": " + v[0].Message + " (and " +
		itoa(len(v)-1) + " more)"
}

func (v ValidationErrors) Is(target error) bool { return target == ErrValidation }

func (v ValidationErrors) Or(err error) error {
	if len(v) > 0 {
		return v
	}
	return err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// LinkStatus is the lifecycle state of a link.
type LinkStatus string

const (
	StatusActive   LinkStatus = "active"
	StatusArchived LinkStatus = "archived"
	StatusExpired  LinkStatus = "expired"
	StatusDisabled LinkStatus = "disabled"
)

// EffectiveStatus is the status a link presents to the outside world.
//
// Expiry is a timestamp, never a stored status. Nothing writes 'expired' to the
// column, because a written status is stale from the moment the expiry passes
// until whatever job notices — and that window is exactly when somebody is
// looking at the link asking why it stopped working.
//
// The redirect path has always derived it this way, which is how an expired link
// came to answer 410 while every management surface still called it active. The
// rule matches Snapshot.Decide, including that expiry outranks an archived
// status: if the two disagreed, this would be the same bug in a smaller form.
func EffectiveStatus(stored LinkStatus, expiresAt *time.Time, now time.Time) LinkStatus {
	if expiresAt != nil && !now.Before(*expiresAt) {
		return StatusExpired
	}
	return stored
}

// Link is a short link as the product understands it.
type Link struct {
	ID          uuid.UUID  `json:"id"`
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	Alias       string     `json:"alias"`
	ShortURL    string     `json:"short_url"`
	URL         string     `json:"url"`
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
	Status      LinkStatus `json:"status"`
	Tags        []Tag      `json:"tags"`

	// ForwardQuery merges the incoming query string into the destination on
	// redirect. Off by default: destinations were configured deliberately, and
	// most callers do not expect ?utm_source to reach them.
	ForwardQuery bool `json:"forward_query"`

	// ForwardPath appends the visitor's extra path segments to the destination,
	// so /{alias}/reviews reaches the destination's own /reviews. Off by
	// default, and for a sharper reason than ForwardQuery: with it on, one
	// alias answers an unbounded set of URLs rather than one.
	ForwardPath bool `json:"forward_path"`

	// BotBlocking is this link's own setting, not the answer. What is actually
	// in effect depends on the domain above it, and only domain.BlocksBots
	// decides that — reporting the resolved boolean here instead would be a
	// second answer that a reader could compare against the first.
	BotBlocking BotPolicy `json:"bot_blocking"`

	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// The gates (M35). What a link demands before it will redirect anybody.
	//
	// **HasPassword, never the password and never its hash.** A management API
	// that returned either would make every reader of a link a reader of its
	// secret, and the whole point of hashing it is that nothing can hand it back.
	// Setting one is a write-only field on the request types.
	HasPassword bool `json:"has_password"`
	// MaxClicks caps how often the link may be followed; OneTime is the same
	// gate fixed at one. Both are the *limit*, never the remaining budget —
	// that is a durable counter reported separately, because a number this
	// struct carried would be a snapshot of a value that moves on every click.
	MaxClicks *int64 `json:"max_clicks,omitempty"`
	OneTime   bool   `json:"one_time"`
	// RequireSignature refuses any request that does not carry a valid,
	// unexpired HMAC signature for this alias.
	RequireSignature bool `json:"require_signature"`
	// ClicksConsumed is how much of the budget has been spent, and it is exact —
	// unlike ClickCount below. Populated only where the caller asked for one
	// link rather than a page of them, because it is a second query per link.
	ClicksConsumed *int64 `json:"clicks_consumed,omitempty"`

	// Approximate: updated in batches with the click events, so it lags by up
	// to one flush interval and can lose a batch on an unclean shutdown.
	// Nothing that must be exact may read it. **Deliberately not the counter the
	// max-click gate reads** — see internal/gate and migration 02100.
	ClickCount  int64      `json:"click_count"`
	LastClickAt *time.Time `json:"last_click_at,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

// Tag groups links within a workspace.
type Tag struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color,omitempty"`
	LinkCount int64     `json:"link_count,omitempty"`
}

// Page is a keyset-paginated result.
//
// Cursor rather than offset: offset pagination re-scans skipped rows and, more
// importantly, silently duplicates or drops entries when rows are inserted
// while a user is paging. Total is optional because counting costs a scan the
// common page load should not pay for.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
	Total      *int64 `json:"total,omitempty"`
}

// LinkSort is the ordering for a link list.
type LinkSort string

const (
	SortNewest     LinkSort = "newest"
	SortOldest     LinkSort = "oldest"
	SortMostClicks LinkSort = "clicks"
)

// LinkFilter describes a link query.
type LinkFilter struct {
	WorkspaceID  uuid.UUID
	Search       string
	TagIDs       []uuid.UUID
	Status       LinkStatus
	Sort         LinkSort
	Cursor       string
	Limit        int32
	IncludeTotal bool
}
