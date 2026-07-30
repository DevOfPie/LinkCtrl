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

	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Approximate: updated in batches with the click events, so it lags by up
	// to one flush interval and can lose a batch on an unclean shutdown.
	// Nothing that must be exact may read it.
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
