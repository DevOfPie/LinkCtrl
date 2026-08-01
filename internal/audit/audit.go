// Package audit records what changed, who changed it, and when.
//
// The table has existed since Phase 1 with nothing writing to it. This package
// is the behavior, and it is built before the five milestones that emit events
// rather than after them: retrofitting emission into a shipped feature means
// touching that feature again, and the first version of the audit trail would
// then be whatever those features happened to record.
//
// Two constraints run through everything here.
//
// An audit record must stay readable after the account it names is gone, so the
// actor is stored twice — actor_user_id for joining while the user exists, and
// actor_label as a snapshot taken at write time, which is what a reader
// actually sees. The column has no foreign key for the same reason.
//
// And it records ip_prefix, never an address. A /24 or /48 answers "was this
// change made from the office network or from somewhere else", which is the
// question an audit trail is read to answer; the remaining bits only identify a
// person. TestEventsRecordAPrefixAndNeverAnAddress holds that line.
package audit

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

// PermRead guards the read API.
//
// Deliberately not delegable to an API key; internal/auth's NonDelegableScopes
// is the one place that enforces it. The audit log is where a network prefix is
// tied to a named person, which is a different exposure from every other read
// scope, and a token in a CI variable is the wrong custodian for it.
const PermRead = "audit.read"

// Actions are the vocabulary of the log. String constants rather than an enum
// type because they are stored verbatim and read by operators, and because
// later milestones add to this list without coordinating with this file.
//
// Dotted noun.verb, matching the permission slugs an operator already reads.
const (
	ActionDomainRootRedirectChanged = "domain.root_redirect_changed"

	// The invitation lifecycle (M27). Three actions rather than one with a
	// state in the metadata: an operator asking "who let this person in" is
	// reading for an action, and a filter on `action` is the query the read API
	// already supports.
	//
	// The redeemed event is the one with a different actor. Issuing and revoking
	// are recorded against the administrator who did them; redeeming is recorded
	// against the person who joined, because that is who took the action, and it
	// is what makes "invited alice@…, alice@… joined" two records that can be
	// read as one fact.
	ActionInvitationCreated  = "invitation.created"
	ActionInvitationRevoked  = "invitation.revoked"
	ActionInvitationRedeemed = "invitation.redeemed"

	// Team management (M28). The membership actions are the counterpart of the
	// invitation ones: an invitation records how somebody was offered a place,
	// these record what happened to it afterwards, and reading the two together
	// is how "who gave this person admin" is answered months later.
	//
	// member.added is the workspace-scoped grant specifically. Joining by
	// redeeming an invitation is already invitation.redeemed, and recording it
	// twice would make one join look like two.
	ActionMemberAdded       = "member.added"
	ActionMemberRemoved     = "member.removed"
	ActionMemberRoleChanged = "member.role_changed"

	// Tenancy (M28). Deleting a workspace cascades every link in it, so its
	// record is the only trace left of what was there — the metadata carries the
	// name, because the row it names is gone.
	ActionWorkspaceCreated = "workspace.created"
	ActionWorkspaceRenamed = "workspace.renamed"
	ActionWorkspaceDeleted = "workspace.deleted"

	// The organization lifecycle (M28, M28.5). The deletion record is the one
	// that outlives its subject: `audit_logs.organization_id` carries no foreign
	// key, so this row survives the organization it describes, and the metadata
	// carries the name and slug because the row that held them is gone. An audit
	// trail erased by the teardown it records would be the one shape this table
	// must not have.
	ActionOrganizationCreated = "organization.created"
	ActionOrganizationDeleted = "organization.deleted"
)

// Event is one thing that happened.
//
// The actor is not a field: it is taken from the *auth.Identity passed to
// Record, so a caller cannot record an action against somebody else by
// accident. Everything here describes the change, not who made it.
type Event struct {
	// Action is what happened, from the constants above.
	Action string
	// TargetType and TargetID name the object, when there is one. A setting
	// that belongs to the instance rather than to a row leaves them empty.
	TargetType string
	TargetID   *uuid.UUID
	// Metadata is the detail that varies by action — old and new values, the
	// reason for a refusal. jsonb, per the rule every Phase 2 milestone
	// inherits: a column per action would be a migration per action.
	//
	// Never put a secret or a full IP address in here. It is returned verbatim
	// by the read API.
	Metadata map[string]any
	// OccurredAt defaults to now. Set explicitly only by tests and by a
	// backfill, and it is the partition key, so a value outside every existing
	// partition lands in the default one.
	OccurredAt time.Time
}

// Entry is an audit record as a reader sees it.
type Entry struct {
	ID         uuid.UUID  `json:"id"`
	OccurredAt time.Time  `json:"occurred_at"`
	ActorLabel string     `json:"actor_label"`
	ActorID    *uuid.UUID `json:"actor_id,omitempty"`
	// ActorAPIKeyID is set when the action was taken with an API key. Present
	// so a reader can tell an interactive change from an automated one, which
	// is most of the value of an audit log during an incident.
	ActorAPIKeyID *uuid.UUID `json:"actor_api_key_id,omitempty"`
	Action        string     `json:"action"`
	TargetType    string     `json:"target_type,omitempty"`
	TargetID      *uuid.UUID `json:"target_id,omitempty"`
	// IPPrefix is a network, never an address: /24 for IPv4, /48 for IPv6.
	IPPrefix string         `json:"ip_prefix,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Filter is a page request. Org scope comes from the actor, not from here:
// letting a caller name the organization would make this an authorization
// decision taken by the caller.
type Filter struct {
	Cursor string
	Limit  int32
}

// Service writes and reads audit records.
type Service struct {
	q *dbgen.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{q: dbgen.New(pool)}
}

// Recorder is the writing half, as its callers see it.
//
// An interface so a service can take a nil-able dependency and so the emission
// tests do not need a database. The link service holds one of these, not the
// concrete type.
type Recorder interface {
	Record(ctx context.Context, actor *auth.Identity, e Event) error
}

// Record writes one event.
//
// No permission check, and that is deliberate: an audit event is a consequence
// of an action that was already authorized, not an action of its own. Gating it
// would mean an actor who can change a setting but not "write audit" could
// change it silently, which is exactly backwards.
//
// The error is returned rather than swallowed so the caller decides. A caller
// inside a transaction should fail the whole operation; a caller outside one
// generally logs and continues, because losing the change is worse than losing
// the record of it.
func (s *Service) Record(ctx context.Context, actor *auth.Identity, e Event) error {
	if e.Action == "" {
		return errors.New("audit: event has no action")
	}

	at := e.OccurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}

	// Marshalled here rather than left to the driver so a metadata value that
	// cannot be encoded fails loudly at the call site instead of writing a
	// record whose detail is missing.
	metadata := []byte("{}")
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("audit: encode metadata for %s: %w", e.Action, err)
		}
		metadata = b
	}

	params := dbgen.InsertAuditLogParams{
		// v7, so the primary key is time-ordered within a partition and the
		// keyset tiebreak on (occurred_at, id) is stable rather than random.
		ID:         uuid.Must(uuid.NewV7()),
		OccurredAt: at,
		ActorLabel: actorLabel(actor),
		Action:     e.Action,
		TargetID:   e.TargetID,
		Metadata:   metadata,
	}
	if e.TargetType != "" {
		params.TargetType = &e.TargetType
	}
	if actor != nil {
		if actor.OrgID != uuid.Nil {
			org := actor.OrgID
			params.OrganizationID = &org
		}
		if actor.WorkspaceID != uuid.Nil {
			ws := actor.WorkspaceID
			params.WorkspaceID = &ws
		}
		if actor.UserID != uuid.Nil {
			uid := actor.UserID
			params.ActorUserID = &uid
		}
		params.ActorApiKeyID = actor.APIKeyID
	}
	// The one place an address becomes a prefix. Taking the address from the
	// context and reducing it here means no caller ever holds a full address
	// destined for this table, so no caller can forget to reduce it.
	if prefix := auth.AnonymizeIP(auth.ClientIPFrom(ctx)); prefix != "" {
		params.IpPrefix = &prefix
	}

	if err := s.q.InsertAuditLog(ctx, params); err != nil {
		return fmt.Errorf("audit: write %s: %w", e.Action, err)
	}
	return nil
}

// actorLabel is the snapshot stored beside the actor's id.
//
// Email, because that is how a person is identified everywhere else in this
// product and it stays meaningful after the account is deleted. Falling back to
// the display name, then to a fixed string: a record with no readable actor is
// still a record that something happened, and dropping the event instead would
// lose more than it protects.
func actorLabel(actor *auth.Identity) string {
	if actor == nil {
		return "system"
	}
	if actor.Email != "" {
		return actor.Email
	}
	if actor.Name != "" {
		return actor.Name
	}
	return "unknown"
}

// defaultPageLimit and maxPageLimit bound a page. The same shape as the link
// list, so a client that already paginates one surface paginates this one.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// List returns a page of the actor's organization's audit records, newest
// first.
func (s *Service) List(ctx context.Context, actor *auth.Identity, f Filter) (*domain.Page[Entry], error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading the audit log requires %s", domain.ErrForbidden, PermRead)
	}

	limit := f.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	org := actor.OrgID
	params := dbgen.ListAuditLogsParams{
		OrganizationID: &org,
		// One extra row answers "is there another page" without a second query
		// and without a count over a table designed to grow forever.
		PageLimit: limit + 1,
	}
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		params.CursorOccurred = &cur.OccurredAt
		params.CursorID = &cur.ID
	}

	rows, err := s.q.ListAuditLogs(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}

	page := &domain.Page[Entry]{Items: make([]Entry, 0, limit)}
	if len(rows) > int(limit) {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, toEntry(r))
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.OccurredAt, last.ID)
	}
	return page, nil
}

func toEntry(r dbgen.AuditLog) Entry {
	e := Entry{
		ID:            r.ID,
		OccurredAt:    r.OccurredAt,
		ActorLabel:    r.ActorLabel,
		ActorID:       r.ActorUserID,
		ActorAPIKeyID: r.ActorApiKeyID,
		Action:        r.Action,
		TargetID:      r.TargetID,
	}
	if r.TargetType != nil {
		e.TargetType = *r.TargetType
	}
	if r.IpPrefix != nil {
		e.IPPrefix = *r.IpPrefix
	}
	// A record whose metadata does not decode is still returned, without it.
	// The alternative — failing the page — would let one malformed row hide
	// every event around it, which is the opposite of what this API is for.
	if len(r.Metadata) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Metadata, &m); err == nil && len(m) > 0 {
			e.Metadata = m
		}
	}
	return e
}

// cursor is a position in the (occurred_at DESC, id DESC) ordering.
//
// Version-prefixed and fixed-arity, like the link cursor: a cursor minted by an
// older build decodes to the wrong number of fields and is refused, rather than
// being read as a position it does not describe. There is only one ordering
// here, so unlike the link cursor it does not carry a sort.
type cursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
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
	return cursor{OccurredAt: at, ID: id}, nil
}
