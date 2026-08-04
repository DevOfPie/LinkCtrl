// Package instance is the instance-level principal: who administers the box
// rather than a tenant in it, and what they may hand to somebody else.
//
// D38 recorded that this product had no such principal, and three findings
// bottomed out there. D98 introduces one, with two constraints that shape
// everything here:
//
// **Only the instance-owner level may delegate.** The principal confers
// instance-level review; a holder of instance-level review may not confer it
// onwards. That is structural rather than checked — `auth.InstanceGrantable`
// does not contain `instance.admin`, so there is no path by which a grant
// produces another grantor, and the set of people who may delegate cannot grow.
// It is the same argument D43 makes about key-issued invitations: bound what a
// grant may produce, not only who may make one.
//
// **A change requires a person.** Not implemented here, and deliberately: it is
// `destinations.decide` sitting in `auth.NonDelegableScopes`, so a key is refused
// by the map rather than by a check on what kind of credential is calling. This
// package contains no reference to `IsAPIKey`, and that absence is the design.
//
// The scopes are enumerated in internal/auth and nothing inherits from holding
// one. A principal that accumulates scopes because it exists is how the thing
// D38 avoided gets built by accident.
package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// PermAdmin guards conferring and withdrawing instance-level review.
//
// Re-exported from internal/auth rather than declared again, because the
// identity resolver has to know the slug and this package must not be able to
// drift from it.
const PermAdmin = auth.PermInstanceAdmin

// Audit actions for the two writes. Both are instance-wide events: they change
// who may act on every organization's disputes, so they belong to no tenant.
const (
	ActionReviewerGranted = "instance.reviewer_granted"
	ActionReviewerRevoked = "instance.reviewer_revoked"
)

// Reviewer is somebody holding instance-level review, as the principal sees
// them.
type Reviewer struct {
	UserID uuid.UUID `json:"user_id"`
	Email  string    `json:"email"`
	Name   string    `json:"name"`
	// GrantedAt is when they were appointed. GrantedBy is who appointed them,
	// absent for the two grants nobody performed interactively — the bootstrap in
	// migration 03400 and the setup flow that claimed a fresh instance.
	GrantedAt time.Time  `json:"granted_at"`
	GrantedBy *uuid.UUID `json:"granted_by,omitempty"`
	// CanDecide separates the two halves of the dispute permission. A reviewer
	// normally holds both; the field exists because the halves are separately
	// revocable and a list that folded them together would show a state the
	// database can be in as a state it cannot.
	CanDecide bool `json:"can_decide"`
}

// grantableScopes is auth.InstanceGrantable in a stable order.
//
// The set is a map because membership is the question every caller asks of it —
// *may this scope be conferred* — and Go randomises map iteration, so a loop
// over it would write the two grants in a different order on every call. Nothing
// here depends on the order today; what does depend on it is a reader being able
// to reproduce what a failure left behind, and a log line that is the same twice.
func grantableScopes() []string {
	out := make([]string, 0, len(auth.InstanceGrantable))
	for scope := range auth.InstanceGrantable {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

// Service reads and writes instance-level grants.
type Service struct {
	pool  *pgxpool.Pool
	q     *dbgen.Queries
	audit audit.Recorder
	log   *slog.Logger
}

// Config is what a Service needs.
type Config struct {
	// Audit records the two writes. Nil records nothing.
	Audit audit.Recorder
	Log   *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), audit: cfg.Audit, log: log}
}

// Reviewers lists who holds instance-level review.
//
// Gated on PermAdmin rather than on holding review itself. Who else administers
// the instance is the principal's business; a reviewer needs the queue, not the
// roster, and handing them one would disclose the full set of administrators to
// everybody appointed to work through disputes.
func (s *Service) Reviewers(ctx context.Context, actor *auth.Identity) ([]Reviewer, error) {
	if !actor.Can(PermAdmin) {
		return nil, fmt.Errorf("%w: listing instance reviewers requires %s",
			domain.ErrForbidden, PermAdmin)
	}

	rows, err := s.q.ListInstanceGrantHolders(ctx, auth.PermDestinationsReview)
	if err != nil {
		return nil, fmt.Errorf("list instance reviewers: %w", err)
	}
	deciders, err := s.q.ListInstanceGrantHolders(ctx, auth.PermDestinationsDecide)
	if err != nil {
		return nil, fmt.Errorf("list instance deciders: %w", err)
	}
	canDecide := make(map[uuid.UUID]bool, len(deciders))
	for _, d := range deciders {
		canDecide[d.ID] = true
	}

	out := make([]Reviewer, 0, len(rows))
	for _, r := range rows {
		out = append(out, Reviewer{
			UserID: r.ID, Email: r.Email, Name: r.Name,
			GrantedAt: r.GrantedAt, GrantedBy: r.GrantedBy,
			CanDecide: canDecide[r.ID],
		})
	}
	return out, nil
}

// GrantReviewer confers instance-level review on the account with an address.
//
// By address rather than by id, because the principal appointing somebody knows
// who they are and not what their uuid is, and because the address is what both
// surfaces already collect.
//
// Idempotent, so appointing somebody who is already a reviewer succeeds. Two
// administrators doing the same obvious thing must not produce something that
// reads like a refusal.
func (s *Service) GrantReviewer(
	ctx context.Context, actor *auth.Identity, email string,
) (*Reviewer, error) {
	if !actor.Can(PermAdmin) {
		return nil, fmt.Errorf("%w: conferring instance-level review requires %s",
			domain.ErrForbidden, PermAdmin)
	}

	user, err := s.q.GetUserByEmail(ctx, auth.NormalizeEmail(email))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Named plainly. This endpoint is reachable only by the instance
		// principal, who can already read the member list of any organization
		// they are in and is being asked to confer authority over the whole box;
		// an ambiguous answer here would cost them a real mistake — appointing
		// nobody and believing they had — to protect an address they are not the
		// stranger to.
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "unknown",
			Message: "no account on this instance has that address",
		}}
	case err != nil:
		return nil, fmt.Errorf("look up account: %w", err)
	}

	// Both halves, taken from the enumerated set rather than spelled out here: a
	// reviewer who could read but not decide would be watching a queue they
	// cannot work, and a second list would be a second thing to keep in step.
	//
	// In one transaction, because the pair is the grant. Half of it is a state
	// the database can hold — Reviewer.CanDecide exists to render it honestly if
	// a revoke ever produces one — but it is not a state an appointment should be
	// able to leave behind, and a partial failure here would be an administrator
	// told the operation failed while it half happened.
	//
	// The row count is deliberately ignored: zero means they already held it, and
	// re-conferring is a success. The slug-typo case the count exists for is
	// checked where it matters, on the setup path, in internal/auth.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	granter := actor.UserID
	for _, scope := range grantableScopes() {
		if _, err := q.GrantInstancePermission(ctx, dbgen.GrantInstancePermissionParams{
			UserID: user.ID, Permission: scope, GrantedBy: &granter,
		}); err != nil {
			return nil, fmt.Errorf("confer %s: %w", scope, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, ActionReviewerGranted, user.ID, user.Email)
	return &Reviewer{
		UserID: user.ID, Email: user.Email, Name: user.Name,
		GrantedAt: time.Now().UTC(), GrantedBy: &actor.UserID, CanDecide: true,
	}, nil
}

// RevokeReviewer withdraws instance-level review from an account.
//
// It cannot withdraw `instance.admin`, because that scope is not in
// InstanceGrantable and this loop is over that set: the principal is not
// removable through the surface it uses to appoint people, which is the same
// reason it is not conferrable through it. An instance that could revoke its own
// last principal would be an instance with nobody able to appoint one, and the
// dispute queue would be stranded exactly as it is stranded today by the finding
// this whole change closes.
func (s *Service) RevokeReviewer(
	ctx context.Context, actor *auth.Identity, userID uuid.UUID,
) error {
	if !actor.Can(PermAdmin) {
		return fmt.Errorf("%w: withdrawing instance-level review requires %s",
			domain.ErrForbidden, PermAdmin)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	var removed int64
	for _, scope := range grantableScopes() {
		n, err := q.RevokeInstancePermission(ctx, dbgen.RevokeInstancePermissionParams{
			UserID: userID, Permission: scope,
		})
		if err != nil {
			return fmt.Errorf("withdraw %s: %w", scope, err)
		}
		removed += n
	}
	// Before the commit, so "they held nothing" leaves nothing behind and writes
	// no audit record about a withdrawal that did not happen.
	if removed == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, ActionReviewerRevoked, userID, "")
	return nil
}

// record writes the audit event, instance-wide.
//
// Instance-wide because that is what the change is: it decides who may act on
// every organization's disputes. Filing it under whichever organization the
// principal happened to be standing in is the misattribution F36 names, and this
// is the one write in the product created *after* that finding, so getting it
// wrong here would be a new instance of a defect being closed in the same commit.
//
// Logged rather than returned on failure, like every other post-write record:
// the grant has already happened, and failing the request now would tell the
// caller nothing changed when something did.
func (s *Service) record(
	ctx context.Context, actor *auth.Identity, action string, subject uuid.UUID, email string,
) {
	if s.audit == nil {
		return
	}
	target := subject
	meta := map[string]any{"scopes": []string{
		auth.PermDestinationsReview, auth.PermDestinationsDecide,
	}}
	if email != "" {
		// The address, because the account may be gone by the time somebody reads
		// this and a bare uuid answers nothing then. Same reasoning as
		// actor_label, one column over.
		meta["email"] = email
	}
	if err := s.audit.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "user", TargetID: &target,
		Metadata: meta, InstanceWide: true,
	}); err != nil {
		s.log.Warn("instance review changed but the audit record was not written",
			slog.String("action", action), slog.String("subject", subject.String()),
			slog.Any("error", err))
	}
}
