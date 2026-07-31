// Package notify delivers in-app notifications.
//
// Scope is deliberately narrow: a per-user inbox its named consumers can write
// to, and nothing else. There is no push transport, no per-event preference
// machinery and no general notification centre — no scope row asks for one, and
// the risk this milestone was flagged for was building one anyway. Email is
// M26's concern and reads from its own outbox, not from this table.
//
// The table shipped dormant in Phase 1 and this package adds no DDL. Anything
// structural a kind needs goes in the `data` jsonb, which is the rule every
// dormant table in this schema follows until the feature that needs a column
// actually arrives.
package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Kinds are the notification vocabulary. Stored verbatim and read by operators,
// and extended by later milestones without coordinating with this file.
const (
	// KindAuditGrowth warns that the audit log has passed its size threshold.
	// The first consumer, and the one that made this milestone urgent: audit
	// retention defaults to keeping everything (D5), which is only a safe
	// default while somebody is told what it costs.
	KindAuditGrowth = "audit.growth"
)

// RoleOwner is the role notified about things that concern the organization
// rather than a person.
const RoleOwner = "owner"

// Notification is one item in a user's inbox.
type Notification struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"`
	Title string    `json:"title"`
	Body  string    `json:"body,omitempty"`
	// Data is per-kind detail. Shape is the kind's business, not this
	// package's, and it is returned verbatim.
	Data      map[string]any `json:"data,omitempty"`
	ReadAt    *time.Time     `json:"read_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Event is a notification about to be written. The recipient is a separate
// argument, so a caller cannot accidentally address one Event at two people
// while sharing its mutable Data map.
type Event struct {
	Kind        string
	Title       string
	Body        string
	Data        map[string]any
	WorkspaceID *uuid.UUID
}

// Filter is a page request.
type Filter struct {
	Cursor     string
	Limit      int32
	UnreadOnly bool
}

// Service writes and reads notifications.
type Service struct {
	q *dbgen.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{q: dbgen.New(pool)}
}

// Notifier is the writing half, as its consumers see it. An interface so a
// consumer takes a nil-able dependency and its tests need no database.
type Notifier interface {
	Notify(ctx context.Context, userID uuid.UUID, e Event) error
}

const (
	defaultPageLimit = 25
	maxPageLimit     = 100
)

// Notify writes one notification to one user's inbox.
//
// No permission check: a notification is a consequence of something that
// already happened, and the recipient is chosen by the consumer rather than
// requested by a caller. Reading is where authorization lives, and there it is
// simply "your own inbox".
func (s *Service) Notify(ctx context.Context, userID uuid.UUID, e Event) error {
	if e.Kind == "" {
		return errors.New("notify: notification has no kind")
	}
	if e.Title == "" {
		return errors.New("notify: notification has no title")
	}

	data := []byte("{}")
	if len(e.Data) > 0 {
		b, err := json.Marshal(e.Data)
		if err != nil {
			return fmt.Errorf("notify: encode data for %s: %w", e.Kind, err)
		}
		data = b
	}

	if err := s.q.InsertNotification(ctx, dbgen.InsertNotificationParams{
		ID:          uuid.Must(uuid.NewV7()),
		UserID:      userID,
		WorkspaceID: e.WorkspaceID,
		Kind:        e.Kind,
		Title:       e.Title,
		Body:        e.Body,
		Data:        data,
	}); err != nil {
		return fmt.Errorf("notify: write %s: %w", e.Kind, err)
	}
	return nil
}

// OwnersOf lists the users to tell about something concerning the organization.
func (s *Service) OwnersOf(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.q.ListUsersWithRoleInOrg(ctx, dbgen.ListUsersWithRoleInOrgParams{
		OrganizationID: orgID,
		RoleSlug:       RoleOwner,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: list owners: %w", err)
	}
	return ids, nil
}

// NotifiedSince reports whether this user already has a notification of this
// kind newer than `since`.
//
// The re-notify guard, and it lives here rather than in the consumer because
// every recurring consumer needs the same thing: a condition that is still true
// on the next run is still true, and re-raising it hourly is how an inbox stops
// being read.
func (s *Service) NotifiedSince(ctx context.Context, userID uuid.UUID, kind string, since time.Time) (bool, error) {
	n, err := s.q.CountRecentNotificationsOfKind(ctx, dbgen.CountRecentNotificationsOfKindParams{
		UserID: userID,
		Kind:   kind,
		Since:  since,
	})
	if err != nil {
		return false, fmt.Errorf("notify: check recent %s: %w", kind, err)
	}
	return n > 0, nil
}

// List returns a page of the actor's own notifications, newest first.
//
// The actor's own, always. There is no permission for reading somebody else's
// inbox because there is no reason to have one, and the query is scoped by
// user_id rather than filtered afterwards.
func (s *Service) List(ctx context.Context, actor *auth.Identity, f Filter) (*domain.Page[Notification], error) {
	if actor == nil {
		return nil, domain.ErrUnauthorized
	}

	limit := f.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	params := dbgen.ListNotificationsParams{
		UserID:     actor.UserID,
		UnreadOnly: f.UnreadOnly,
		PageLimit:  limit + 1,
	}
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		params.CursorCreated = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := s.q.ListNotifications(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}

	page := &domain.Page[Notification]{Items: make([]Notification, 0, limit)}
	if len(rows) > int(limit) {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, toNotification(r))
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// Unread is the count behind the nav badge.
//
// Called on every dashboard page render, which is why the query's predicate is
// written to match the partial index the table ships with rather than merely to
// be correct.
func (s *Service) Unread(ctx context.Context, actor *auth.Identity) (int64, error) {
	if actor == nil {
		return 0, nil
	}
	n, err := s.q.CountUnreadNotifications(ctx, actor.UserID)
	if err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return n, nil
}

// MarkRead marks one notification read, reporting whether it changed anything.
//
// A notification that is not the actor's own is indistinguishable from one that
// does not exist: both are "nothing changed", so an id cannot be probed.
func (s *Service) MarkRead(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if actor == nil {
		return domain.ErrUnauthorized
	}
	if _, err := s.q.MarkNotificationRead(ctx, dbgen.MarkNotificationReadParams{
		ID: id, UserID: actor.UserID,
	}); err != nil {
		return fmt.Errorf("mark notification read: %w", err)
	}
	// Deliberately not ErrNotFound on zero rows: already-read is the common
	// case — two tabs, or a double click — and it is a success, not a 404.
	return nil
}

// MarkAllRead empties the badge, reporting how many it cleared.
func (s *Service) MarkAllRead(ctx context.Context, actor *auth.Identity) (int64, error) {
	if actor == nil {
		return 0, domain.ErrUnauthorized
	}
	n, err := s.q.MarkAllNotificationsRead(ctx, actor.UserID)
	if err != nil {
		return 0, fmt.Errorf("mark all notifications read: %w", err)
	}
	return n, nil
}

func toNotification(r dbgen.Notification) Notification {
	n := Notification{
		ID:        r.ID,
		Kind:      r.Kind,
		Title:     r.Title,
		Body:      r.Body,
		ReadAt:    r.ReadAt,
		CreatedAt: r.CreatedAt,
	}
	// A row whose data does not decode is still returned, without it. Failing
	// the page would let one malformed row hide every notification around it.
	if len(r.Data) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Data, &m); err == nil && len(m) > 0 {
			n.Data = m
		}
	}
	return n
}

// cursor is a position in the (created_at DESC, id DESC) ordering.
// Version-prefixed and fixed-arity, like the link and audit cursors.
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join([]string{
		"1", at.UTC().Format(time.RFC3339Nano), id.String(),
	}, "|")))
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != "1" {
		return cursor{}, errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return cursor{}, err
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return cursor{}, err
	}
	return cursor{CreatedAt: at, ID: id}, nil
}

// AuditGrowthReminderInterval is how long one audit-growth warning suppresses
// the next.
//
// The threshold stays crossed until an operator acts on it, so without this the
// hourly job would file a notification every hour forever — and an inbox filling
// up with the same line is one people stop opening, which would cost exactly the
// warning D5's keep-forever default leans on. A week is long enough to be
// ignorable while somebody plans the work, short enough not to fall out of mind.
const AuditGrowthReminderInterval = 7 * 24 * time.Hour

// WarnAuditGrowth tells every organization's owners that the audit log has
// passed its size threshold, at most once per reminder interval each.
//
// Lives here rather than in the job runner because it is policy, not
// scheduling: what counts as "too big", who hears about it, and how often are
// decisions worth testing, and a job runner in package main cannot be reached
// by a test.
//
// A threshold of zero or less disables the warning entirely — for an operator
// who has already decided and does not want reminding. That is the only way to
// switch it off, and it is deliberately not the default: keep-forever is safe
// only if the instance nobody configured is the one that gets warned (D19).
func (s *Service) WarnAuditGrowth(ctx context.Context, size, threshold int64) error {
	if threshold <= 0 || size < threshold {
		return nil
	}

	orgs, err := s.q.ListOrganizationIDs(ctx)
	if err != nil {
		return fmt.Errorf("notify: list organizations: %w", err)
	}

	since := time.Now().Add(-AuditGrowthReminderInterval)
	var errs []error
	for _, orgID := range orgs {
		owners, err := s.OwnersOf(ctx, orgID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, userID := range owners {
			recent, err := s.NotifiedSince(ctx, userID, KindAuditGrowth, since)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if recent {
				continue
			}
			if err := s.Notify(ctx, userID, Event{
				Kind:  KindAuditGrowth,
				Title: "The audit log has passed its size threshold",
				Body: fmt.Sprintf(
					"audit_logs now uses %s on disk, past the %s threshold. Audit "+
						"history is kept forever until LINKCTRL_AUDIT_RETENTION_DAYS is "+
						"set, so it will keep growing.",
					HumanBytes(size), HumanBytes(threshold)),
				// The numbers go in the jsonb as well as into the sentence, so a
				// later UI can render them without parsing English back out.
				Data: map[string]any{"bytes": size, "threshold": threshold},
			}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// HumanBytes renders a size the way an operator reads one. Binary units,
// because that is what df and every disk-sizing conversation uses.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
