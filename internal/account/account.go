// Package account ends an account's life, and then erases what ending it could
// not reach.
//
// **It is a gap being closed, not a feature being added** (finding F44). Nothing
// in this product deleted a user until M52, while the schema had described
// erasure in the present tense from the first migration — `users.anonymized_at`
// carried the comment *"set by the GDPR erasure routine"* and had no writer, and
// by M45 the count of places asserting a routine that did not exist had reached
// five. Each was corrected in place rather than built. This package is the
// build.
//
// Its own package rather than a method on internal/auth, for the reason
// internal/recovery gives: internal/auth answers *who is this request*, and the
// three services that change an account's existence — signup, recovery, and now
// this — each own their refusals and land beside it instead of inside it. There
// is also a hard reason. internal/audit imports internal/auth, so internal/auth
// cannot record anything without an interface seam, and this operation has to
// write its audit record inside the transaction that performs it.
//
// # The two halves, and why they are two
//
// **Deletion is interactive and immediate.** One transaction: verify the
// password, refuse what must be refused, remove everything that grants access,
// stamp `deleted_at` and `status = 'deleted'`. When it returns, no credential
// reaches the account and the address is free for a new one.
//
// **Erasure is a batched sweep, and it lags.** The identifying residue lives in
// the two tables that deliberately have no foreign key to `users` —
// `audit_logs` and `destination_disputes` — because a record that vanishes with
// its subject is not a record. Those cannot be cascaded away; they are scrubbed
// in place, and `anonymized_at` marks the row whose residue has gone. The gap
// between the two timestamps is the sweep's cadence, it is bounded by an hour,
// and `docs/SECURITY.md` states that as a number because a compliance reader is
// entitled to know erasure is not instantaneous.
//
// # What a soft delete does not do
//
// It fires no foreign key. Eight tables declare `ON DELETE CASCADE` against
// `users` and every one of those clauses triggers on `DELETE`, so under a kept
// row the cascade never runs. `DeleteAccountDependents` is what stands in for
// it, and query/accounts.sql enumerates them and says why four are there beyond
// the four M52 names. Six of them were M52's; `mfa_recovery_codes` and
// `mfa_pending_logins` joined at M53, which is the milestone that created them —
// a recovery code admits somebody to an account with no password, so leaving one
// behind a deleted account is the `password_resets` defect in a new table.
//
// Four other columns reference `users` with `ON DELETE SET NULL` —
// `links.created_by`, `invitations.invited_by`, `invitations.redeemed_by` and
// `instance_grants.granted_by` — and those are left pointing at the erased row
// on purpose. That is D148: the ids survive, the labels become a constant, and
// correlating an erased actor's entries is the id rather than anything derived
// from it. Nulling them would destroy the correlation the decision chose to
// keep, in exchange for hiding a uuid that identifies nobody from inside this
// instance.
package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// TombstoneLabel is what an erased actor's snapshot becomes, everywhere one is
// stored: `audit_logs.actor_label`, `destination_disputes.created_by_label` and
// `destination_disputes.decided_by_label`.
//
// **A constant, and the ids survive.** That is D148, owner-set 2026-08-08 and
// answered before any of this was written. The alternative shapes were a
// pseudonym derived from the account — reversible by anybody holding the input —
// and a random token stored per account with the ids nulled, which is the
// stronger claim and stays available: this migrates to that, where that could
// not migrate back.
//
// The cost the decision accepted rather than removed: a surviving uuid is
// pseudonymous data, so `docs/SECURITY.md` says the residue identifies nobody
// *from inside this instance* rather than that it is anonymous. Anybody holding
// an external id-to-person mapping re-identifies the actor, and that is a
// sentence in the security document, not a defect.
//
// It cannot collide with a live actor's label. `actorLabel` in internal/audit
// prefers the address and falls back to the display name only when the address
// is empty, which no live account's is — the column is NOT NULL and every
// writing path validates one.
const TombstoneLabel = "deleted account"

// Config is what a Service needs.
type Config struct {
	// Auth verifies the account's own password. Required: confirmation is the
	// password and there is no confirmation without it.
	Auth *auth.Service
	// Audit records the deletion, inside the transaction that performs it. Nil
	// records nothing, on the same terms every other service in this tree
	// offers — but see RecordTx's note, because here the record and the change
	// stand or fall together.
	Audit Auditor
	Log   *slog.Logger
}

// Auditor is the seam onto internal/audit.
//
// **RecordTx and not Record**, which is the whole reason this interface is
// narrower than audit.Recorder. The actor of an account deletion is the account
// being deleted, so a record written after the commit sits in the window between
// deletion and erasure carrying an address — and if the hourly sweep lands in
// that window, `anonymized_at` is set before the record exists and the address
// stays in `audit_logs` for good. Joining the transaction makes the ordering a
// fact instead of a race nobody would ever reproduce.
type Auditor interface {
	RecordTx(ctx context.Context, q *dbgen.Queries, actor *auth.Identity, e audit.Event) error
}

// Service deletes accounts and erases what deletion leaves behind.
type Service struct {
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	auth    *auth.Service
	auditor Auditor
	log     *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Auth == nil {
		return nil, errors.New("account: no auth service")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{
		pool:    pool,
		q:       dbgen.New(pool),
		auth:    cfg.Auth,
		auditor: cfg.Audit,
		log:     cfg.Log,
	}, nil
}

// SoleOwnerError refuses a deletion and names the organizations blocking it.
//
// A type rather than a formatted string because both surfaces need the list:
// the API puts the sentence in a problem document and the dashboard puts it
// beside the button, and "which ones" is the only part of the refusal anybody
// can act on. It unwraps to domain.ErrConflict, so every caller that already
// handles a conflict handles this without knowing the type exists.
type SoleOwnerError struct {
	// Organizations is the display names, in the order the query returned them.
	Organizations []string
}

func (e *SoleOwnerError) Error() string {
	return fmt.Sprintf(
		"%s: you are the only owner of %s, and an organization cannot be left "+
			"with none; make somebody else an owner, or delete it, first",
		domain.ErrConflict, strings.Join(e.Organizations, ", "))
}

func (e *SoleOwnerError) Unwrap() error { return domain.ErrConflict }

// Delete removes the acting account, and only the acting account.
//
// **There is no administrative delete-somebody-else here**, and its absence is a
// decision rather than an omission. Who may end another person's account is a
// permission-model question, and D38's precedent is that inventing one inside
// another milestone is the wrong place to answer it.
//
// # What it refuses
//
// **The instance principal** (D98). Deleting the account that administers the
// box leaves no path back that does not involve SQL: `instance_grants` is how
// every instance-level permission reaches a person, `ListInstanceGrantHolders`
// hides a soft-deleted holder, and nothing in the product can confer the
// principal on somebody new. `lctl instance principal move --to <email>` is the
// route, and the refusal names it.
//
// **The sole owner of an organization that still exists.** M28.5 refuses
// removing an organization's last owner; this is the same rule approached from
// the other side, because a rule you can step around by leaving through a
// different door is not a rule. Every account has an auto-provisioned personal
// organization it owns alone, so in practice this is the refusal most people
// meet first, and the message says which organizations are blocking so the
// remedy is obvious.
//
// # What it does not refuse
//
// **An account that would be left belonging to nothing.** That state is D36's
// and it is already real: `identityWithoutOrganization` resolves it, the session
// path treats it as an empty state, and handing over every organization on the
// way out is exactly how somebody arrives at it. Refusing here would mean the
// only way to leave is to be the last person out.
//
// # What it removes, and what it deliberately does not
//
// Everything that grants access or points a live surface at the account goes in
// this transaction: memberships, sessions, API keys, notifications, outstanding
// password-reset tokens and instance-level grants. What stays is what the schema
// keeps on purpose — the audit trail, disputes, and the links, invitations and
// grants the account touched — because those are records of the past. Their
// identifying residue is the erasure sweep's, not this transaction's.
//
// Uploaded QR logos are reached by none of this, and saying so is the point.
// They hang off `qr_codes` → `links` → workspaces and organizations, never off
// `users`, and the sole-owner refusal above means every organization the account
// belongs to outlives it. A deletion that removed their logos would be
// destroying a surviving tenant's data.
func (s *Service) Delete(ctx context.Context, actor *auth.Identity, password string) error {
	if actor == nil {
		return domain.ErrUnauthorized
	}
	// D87's limb: the subject is the person, not the credential. A leaked key
	// must not be able to delete the account that owns it, and the key is not
	// the person whose password confirms this.
	if actor.IsAPIKey() {
		return fmt.Errorf(
			"%w: deleting an account requires a signed-in session, not an API key",
			domain.ErrForbidden)
	}
	// Read from the identity rather than from the database. Instance grants are
	// folded into the permission set by addInstanceGrants on every resolution,
	// so this is the same fact one query earlier, and Identity.Can is the one
	// evaluator the inherited Permissions rule keeps it to.
	if actor.Can(auth.PermInstanceAdmin) {
		return fmt.Errorf(
			"%w: this account administers the instance, and deleting it would leave "+
				"nobody who can; move the principal first with "+
				"`lctl instance principal move --to <email>`",
			domain.ErrConflict)
	}

	// Before the transaction, and outside it, exactly as ChangePassword does.
	// Argon2 is deliberately ~100ms of work and holding a row lock across it
	// would block every write to this account for that long, to close a window
	// in which the only person who could change the password is the person
	// already signed in and pressing this button.
	if err := s.auth.VerifyPassword(ctx, actor.UserID, password); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// The target row first, so everything below decides against a state nothing
	// else can move. A second deletion of the same account — two browsers, one
	// button — finds no row and is not-found rather than a second pass.
	user, err := q.LockUserForDeletion(ctx, actor.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("lock account: %w", err)
	}

	// Then the owner sets, locked before they are counted, so two administrators
	// acting at once cannot each pass a check the other invalidates.
	blocking, err := q.LockOrganizationsSolelyOwnedBy(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("count sole ownerships: %w", err)
	}
	if len(blocking) > 0 {
		names := make([]string, 0, len(blocking))
		for _, org := range blocking {
			names = append(names, org.Name)
		}
		return &SoleOwnerError{Organizations: names}
	}

	removed, err := q.DeleteAccountDependents(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("remove account dependents: %w", err)
	}

	n, err := q.SoftDeleteUser(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}

	// Inside the transaction, which no other audit emission in this tree is.
	// Auditor's own comment carries the reason; the short version is that the
	// erasure sweep reads `anonymized_at`, and a record written after the commit
	// can arrive after the sweep has already been past it.
	//
	// No metadata naming the account. The actor columns already carry the id and
	// the address snapshot, and the snapshot is what erasure exists to remove —
	// a second copy in `metadata` would be a second copy the sweep does not
	// know about.
	if s.auditor != nil {
		userID := user.ID
		if err := s.auditor.RecordTx(ctx, q, actor, audit.Event{
			Action:       audit.ActionAccountDeleted,
			TargetType:   "user",
			TargetID:     &userID,
			InstanceWide: true,
		}); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Info, not Debug: this is irreversible removal of somebody's own data, and
	// with the account gone from every list this line and the audit record are
	// what remain. The address is deliberately absent — it is the thing being
	// erased, and writing it to a log the sweep cannot reach would put it
	// somewhere erasure does not go.
	s.log.Info("account deleted",
		slog.String("user_id", user.ID.String()),
		slog.Int64("memberships", removed.Memberships),
		slog.Int64("sessions", removed.Sessions),
		slog.Int64("api_keys", removed.ApiKeys),
		slog.Int64("notifications", removed.Notifications),
		slog.Int64("password_resets", removed.PasswordResets),
		slog.Int64("instance_grants", removed.InstanceGrants),
		slog.Int64("mfa_recovery_codes", removed.MfaRecoveryCodes),
		slog.Int64("mfa_pending_logins", removed.MfaPendingLogins))
	return nil
}

// ErasePending scrubs one batch of deleted accounts and returns how many it
// took.
//
// Called by the hourly `housekeeping` pass under `advisoryLockKeyMaintenance`,
// and **not** by a job family of its own: a new advisory key and a new goroutine
// for a sweep that finds nothing on almost every run is cost without a reason,
// which is the same answer M51's reset purge got.
//
// Idempotent and re-entrant, because the two-leader window during a rolling
// deploy is a stated property of this scheduler rather than something this
// milestone may assume away. `FOR UPDATE SKIP LOCKED` gives a second leader a
// disjoint batch instead of a wait, and every label update is guarded on the
// label not already being the tombstone, so a second pass over the same row
// writes nothing. The integration test runs the pass twice and diffs.
func (s *Service) ErasePending(ctx context.Context, batch int32) (int64, error) {
	erased, err := s.q.EraseDeletedAccounts(ctx, dbgen.EraseDeletedAccountsParams{
		Batch:     batch,
		Tombstone: TombstoneLabel,
	})
	if err != nil {
		return 0, fmt.Errorf("erase deleted accounts: %w", err)
	}
	return int64(len(erased)), nil
}
