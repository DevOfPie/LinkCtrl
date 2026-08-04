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
	"github.com/jackc/pgx/v5"
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

	// KindInviteAccepted tells the person who sent an invitation that it was
	// redeemed. The organization gained a member, and the one account that
	// certainly wants to know is the one that chose to add them.
	KindInviteAccepted = "invite.accepted"
)

// MailAuditGrowth names the mail template for the same warning. It is the
// filename in internal/ui/templates/mail, without the extension, and it is also
// what lands in the outbox's `kind` column.
const MailAuditGrowth = "audit-growth"

// MailDisputeDecided names the template for a dispute outcome (D1's addendum to
// M32). Same convention as above: the filename, without the extension.
//
// The template name is here rather than in internal/dispute for one reason —
// this package owns the mailer, and a consumer that names a template it cannot
// render is a send that fails at the relay instead of at boot. internal/ui
// parses every template in that directory at startup, so a name that has no file
// takes the process down before anybody disputes anything.
const MailDisputeDecided = "dispute-decided"

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
	// mailer is nil unless an SMTP relay is configured, which is the whole of
	// the mail-free degradation path: no branch on a flag, no error to swallow,
	// nothing queued. In-app delivery is the baseline and never depends on it.
	mailer Enqueuer
	// appURL is the origin a mail links back to. Empty when no mailer is
	// configured, because nothing reads it then.
	appURL string
}

// Enqueuer is internal/mail's writing half, as this package needs it.
//
// Declared here rather than imported so that notify keeps depending on nothing
// but the store: the consumer owns the interface, and a test satisfies it with
// a slice.
type Enqueuer interface {
	Enqueue(ctx context.Context, to, kind string, data map[string]string) error
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{q: dbgen.New(pool)}
}

// WithMail attaches a mailer, so notifications that have an email form are also
// sent as one.
//
// A setter rather than a constructor argument because the mailer is optional
// and every existing caller passes nothing. Handing a nil Enqueuer here is the
// same as never calling it: a nil interface, checked once at the send site.
func (s *Service) WithMail(m Enqueuer, appURL string) *Service {
	s.mailer = m
	s.appURL = appURL
	return s
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

// Recipient is one person to tell, in both forms: the id an inbox row is keyed
// by, and the address a mail goes to.
//
// One type rather than two lookups. Every consumer that emails also files the
// in-app notification — in-app is the baseline and mail is the addition — so
// fetching the address separately would be a query per recipient for something
// the first query already had in hand.
type Recipient struct {
	UserID uuid.UUID
	Email  string
	// Name is what a mail greets them by. Empty is common — the column defaults
	// to it — so callers use Greeting rather than this.
	Name string
}

// Greeting is the name to address this person by, falling back to the address.
//
// "Hello owner@example.com" is a worse sentence than "Hello Ada" and a better
// one than "Hello ,".
func (r Recipient) Greeting() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Email
}

// EveryOwner lists the users to tell about something concerning the *instance*
// rather than one organization.
//
// The distinction is not decoration. A destination blocklist, and the disputes
// about it, cross every organization on the box (M31), so the people who can act
// are the owners of all of them — and telling only the first organization's
// owners would be silently wrong on any instance with a second one, which is the
// bug WarnAuditGrowth's comment already names for its own loop.
//
// Deduplicated by user, because one account can own two organizations and would
// otherwise receive the same notification twice.
func (s *Service) EveryOwner(ctx context.Context) ([]Recipient, error) {
	orgs, err := s.q.ListOrganizationIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("notify: list organizations: %w", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(orgs))
	var out []Recipient
	for _, orgID := range orgs {
		// nil: a blocklist entry is not in a workspace, so this is the
		// organization-wide owners of each organization and nobody else.
		owners, err := s.OwnersOf(ctx, orgID, nil)
		if err != nil {
			return nil, err
		}
		for _, o := range owners {
			if _, dup := seen[o.UserID]; dup {
				continue
			}
			seen[o.UserID] = struct{}{}
			out = append(out, o)
		}
	}
	return out, nil
}

// RecipientByID resolves one user into the pair a mail needs: their address and
// what to greet them by.
//
// A deleted account resolves to the zero Recipient with a nil error rather than
// to ErrNotFound, because every caller is a notification about something that
// already happened and none of them should fail because the person it concerns
// has since left. A zero Recipient has no address, and Mail below does nothing
// with one.
func (s *Service) RecipientByID(ctx context.Context, userID uuid.UUID) (Recipient, error) {
	row, err := s.q.GetUserByID(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Recipient{}, nil
	case err != nil:
		return Recipient{}, fmt.Errorf("notify: load recipient: %w", err)
	}
	return Recipient{UserID: row.ID, Email: row.Email, Name: row.Name}, nil
}

// Mail queues the email form of a notification, if there is a mailer.
//
// The optionality lives here and nowhere else, which is the whole point of
// routing consumers through this package: a caller writes the inbox row and then
// calls this, and on an instance with no SMTP_HOST the second call returns
// immediately and the outbox stays empty. No consumer branches on whether mail
// is configured, so none of them can get the branch wrong.
//
// AppURL is added to the data here rather than by each caller, for the reason
// mailAuditGrowth reads it from the service: there is no request in scope on the
// paths that send, and an operator with two instances needs to know which one is
// writing to them.
func (s *Service) Mail(ctx context.Context, to Recipient, template string, data map[string]string) error {
	if s.mailer == nil || to.Email == "" {
		return nil
	}
	instance := s.appURL
	if instance == "" {
		instance = "this instance"
	}
	full := make(map[string]string, len(data)+3)
	for k, v := range data {
		full[k] = v
	}
	full["Name"] = to.Greeting()
	full["Instance"] = instance
	full["AppURL"] = s.appURL
	return s.mailer.Enqueue(ctx, to.Email, template, full)
}

// OwnersOf lists the users to tell about something concerning the organization,
// or concerning one workspace in it.
//
// The workspace is a parameter and not an afterthought: "owner" is a role held
// per membership, and a membership scoped to one workspace owns that workspace
// and not the organization (D44). So an organization-wide owner hears about
// everything, and a workspace-scoped owner hears about their own workspace and
// nothing else. Passing nil is news that belongs to no workspace — the audit log
// growing, the instance-wide blocklist — and reaches the organization-wide
// owners alone.
//
// Callers that hold a workspace pass it. That is the whole correction: the query
// used to ignore the distinction, and a workspace-scoped owner was told about
// every hostname and every automation firing in workspaces they hold no
// membership in.
func (s *Service) OwnersOf(
	ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
) ([]Recipient, error) {
	rows, err := s.q.ListUsersWithRoleInOrg(ctx, dbgen.ListUsersWithRoleInOrgParams{
		OrganizationID: orgID,
		WorkspaceID:    workspaceID,
		RoleSlug:       RoleOwner,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: list owners: %w", err)
	}
	out := make([]Recipient, 0, len(rows))
	for _, r := range rows {
		out = append(out, Recipient{UserID: r.ID, Email: r.Email, Name: r.Name})
	}
	return out, nil
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

// Unread is the count behind the badge, on its own.
//
// The API's counterpart of UnreadPreview: a client polling for a number does
// not want the rows, and the endpoint that answers it renders nothing. The
// dashboard shell uses UnreadPreview instead, because it needs both and one
// query answers both.
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

// PreviewLimit is how many unread notifications the header's bell shows before
// deferring to the full page.
//
// Small on purpose. The bell answers "is there anything, and roughly what" — a
// question a person asks in passing, from whatever page they were reading — and
// /notifications answers "show me everything", with pagination and mark-read.
// A preview long enough to scroll would be a worse version of the page it links
// to, so it is cut well before that and says how many are left.
const PreviewLimit = 5

// UnreadPreview is the header's entire notification lookup: the exact unread
// count for the badge, and the newest unread notifications for the bell.
//
// One call, one query, because this runs on every dashboard page render. The
// count and the preview come back together rather than from a count followed by
// a list — see the query's comment for how the total stays exact while the rows
// stay bounded. Splitting them would double the per-render cost of a decoration.
//
// A limit of zero or less means PreviewLimit; anything larger is clamped to it,
// so a caller cannot turn the header into an unbounded list.
func (s *Service) UnreadPreview(ctx context.Context, actor *auth.Identity, limit int32) (int64, []Notification, error) {
	if actor == nil {
		return 0, nil, nil
	}
	if limit <= 0 || limit > PreviewLimit {
		limit = PreviewLimit
	}

	rows, err := s.q.ListUnreadNotificationPreview(ctx, dbgen.ListUnreadNotificationPreviewParams{
		UserID:    actor.UserID,
		PageLimit: limit,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("list unread notification preview: %w", err)
	}
	if len(rows) == 0 {
		// No rows is no unread, so there is no total to read off one.
		return 0, nil, nil
	}

	items := make([]Notification, 0, len(rows))
	for _, r := range rows {
		items = append(items, toNotification(dbgen.Notification{
			ID:          r.ID,
			UserID:      r.UserID,
			WorkspaceID: r.WorkspaceID,
			Kind:        r.Kind,
			Title:       r.Title,
			Body:        r.Body,
			Data:        r.Data,
			ReadAt:      r.ReadAt,
			CreatedAt:   r.CreatedAt,
		}))
	}
	// Every row carries the same window total; the first is as good as any.
	return rows[0].UnreadTotal, items, nil
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
		// nil: the audit log is the organization's, not a workspace's, and the
		// setting that bounds it is an instance one. Only somebody who owns the
		// organization can act on this.
		owners, err := s.OwnersOf(ctx, orgID, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, owner := range owners {
			recent, err := s.NotifiedSince(ctx, owner.UserID, KindAuditGrowth, since)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if recent {
				continue
			}
			if err := s.Notify(ctx, owner.UserID, Event{
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
				// The mail is the addition, not the delivery. If the inbox row
				// could not be written, the recipient has heard nothing at all
				// and the re-notify guard has nothing to suppress the next run
				// with, so sending a mail here would produce one every hour.
				errs = append(errs, err)
				continue
			}
			if err := s.mailAuditGrowth(ctx, owner, size, threshold); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// mailAuditGrowth queues the email form of the warning, if there is a mailer.
//
// D5 made keep-forever safe by promising the growth would be visible; M21 made
// it a metric, M22 an inbox row, and this is the third leg — an owner who does
// not open the dashboard for a month still hears about it. With no mailer
// configured this returns immediately and the outbox stays empty, which is what
// keeps the mailer optional rather than quietly required.
func (s *Service) mailAuditGrowth(ctx context.Context, to Recipient, size, threshold int64) error {
	if s.mailer == nil {
		return nil
	}
	if to.Email == "" {
		return nil
	}
	// Instance rather than a hostname pulled from the request: this runs on the
	// scheduler, where there is no request, and an owner with two instances
	// needs to know which one is growing.
	instance := s.appURL
	if instance == "" {
		instance = "this instance"
	}
	return s.mailer.Enqueue(ctx, to.Email, MailAuditGrowth, map[string]string{
		"Instance":  instance,
		"Name":      to.Greeting(),
		"Size":      HumanBytes(size),
		"Threshold": HumanBytes(threshold),
		"AppURL":    s.appURL,
	})
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
