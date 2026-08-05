// Package invite issues, revokes and redeems organization invitations.
//
// An invite is a single-use, revocable, expiring grant of one membership in one
// organization, and three decisions shape everything here.
//
// **It is bound to an address, not to whoever holds it** (D27). Redemption
// compares the redeeming account's address against the address invited, so a
// link forwarded into a group chat cannot add a stranger. That comparison is a
// new place that could answer "does this address have an account", so every
// refusal in Redeem is the same refusal, spends argon2 work before returning,
// and says nothing about which check failed. The reason is logged, never
// returned. What that work is and is not equal across is Redeem's own doc.
//
// **It may carry any role at or below the inviter's own rank** (D28), and no
// more than editor when an API key issued it (D43). Those are two axes, not one
// ceiling twice: the first bounds the invitation against whoever sent it, the
// second against the *kind of credential* that sent it. D28 read the first as
// sufficient and it is not, because redemption turns an invitation into an
// interactive account — one that requireSessionActor no longer refuses, that
// holds whatever its role holds, and that revoking the key does not revoke.
//
// **It expires** (D29), from creation rather than from delivery: mail leaves
// through the outbox on the scheduler's tick (D23), so there is no send moment
// to start the clock at.
//
// Redemption creates a membership and nothing else (D6). No personal
// organization, no workspace — the invited person is a colleague in somebody
// else's organization, and the "one personal org per user" invariant is broken
// here deliberately.
package invite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// PermWrite guards issuing, listing and revoking invitations. M27 is its first
// enforcement; the permission itself has been seeded since Phase 1.
//
// Delegable to an API key, and that is a recorded conclusion rather than an
// omission (D28, applying D18): a key may bring collaborators in. What makes it
// safe is not D28's rank ceiling — that argument was wrong, and F29 is what it
// cost — but auth.KeyIssuableRoles, which bounds the role a key may put somebody
// at. auth.NonDelegableScopes governs what a key may *hold*; D43 governs what it
// may *make*, and this permission needs both. The two live side by side in
// internal/auth, because this permission is not the only door D43 has to cover:
// team.ChangeRole and team.Grant assign the same roles to somebody already
// admitted.
const PermWrite = "members.write"

// TokenBytes is the entropy in an invitation token, matching a session token.
// Well beyond guessing range, and short enough that the resulting link fits in
// a chat message without wrapping.
const TokenBytes = 32

// ErrNotRedeemable is every redemption failure.
//
// One error for all of them, deliberately: no such token, expired, revoked,
// already spent, wrong address, unknown address on a closed instance, wrong
// password, already a member. Distinguishing any of them would answer a
// question about somebody else's account (D27, and the milestone's
// no-enumeration bullet). The real cause is logged.
var ErrNotRedeemable = errors.New("invite: this invitation cannot be redeemed")

// DefaultRole is the role an invitation carries when none is named.
//
// The least powerful of the four, because this is the value a caller gets by
// not thinking about it and the failure mode of the alternative is somebody
// admitted with more authority than the request asked for.
const DefaultRole = "viewer"

// Invitation is an invite as an administrator sees it. The token is absent by
// construction — only its hash is stored, so it cannot be listed.
type Invitation struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Role  string    `json:"role"`
	// InvitedBy is the address of whoever sent it, empty once that account is
	// gone. A label rather than an id, for the reason the audit log keeps one.
	InvitedBy string `json:"invited_by"`
	// Status is one of pending, expired, revoked, redeemed. Derived rather than
	// stored, because expiry is a comparison against the clock and a stored copy
	// would need a job to keep it true.
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
}

// Invitation statuses.
const (
	StatusPending  = "pending"
	StatusExpired  = "expired"
	StatusRevoked  = "revoked"
	StatusRedeemed = "redeemed"
)

// Created is a new invitation plus the two things that exist only in the
// response that made it.
type Created struct {
	Invitation
	// URL is the copyable link, and the only time the raw token is available.
	// It exists on every path, mailer or no mailer, because on a default
	// instance the mailer is off (D1) and this is the whole delivery mechanism.
	URL string `json:"url"`
	// Emailed says whether a message was queued for delivery. False on an
	// instance with no relay configured, which is not a failure.
	Emailed bool `json:"emailed"`
}

// Offer is what the redemption page may show somebody holding a link.
//
// Deliberately not the invited address. Showing it would tell whoever picked
// the link up exactly which address to type, which is the one thing D27's
// binding is there to prevent — so the page says which organization, and the
// person redeeming supplies the address they were invited at.
type Offer struct {
	OrganizationName string
	Role             string
	ExpiresAt        time.Time
}

// Redeemed is the outcome of a successful redemption.
type Redeemed struct {
	UserID           uuid.UUID
	Email            string
	OrganizationID   uuid.UUID
	OrganizationName string
	Role             string
	// Created is true when the account did not exist and was made here. False
	// means an existing account gained a membership.
	Created bool
}

// Enqueuer is internal/mail's writing half, as this package needs it.
//
// Declared here rather than imported so a test satisfies it with a slice, and
// so "no mailer configured" is a nil interface rather than a flag every call
// site has to remember to check.
type Enqueuer interface {
	Enqueue(ctx context.Context, to, kind string, data map[string]string) error
}

// MailKind names the mail template, which is also what lands in the outbox's
// `kind` column. It is the filename in internal/ui/templates/mail, without the
// extension.
const MailKind = "invitation"

// Service issues and redeems invitations.
type Service struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	hasher *auth.Hasher
	cfg    Config
	log    *slog.Logger
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree: the package doing the work does
// not read the environment.
type Config struct {
	// AppURL is the origin an invitation link points at.
	AppURL string
	// TTL is how long an invitation stays redeemable, from creation (D29).
	TTL time.Duration
	// NewAccounts says whether redemption may create an account that does not
	// exist yet. False under LINKCTRL_SIGNUP_MODE=closed, where the environment
	// ceiling is absolute and an invite may only add users who already exist
	// (D7).
	NewAccounts bool
	// Hasher verifies the password of an account being joined, and hashes the
	// one for an account being created. The service's own, so the cost
	// parameters an operator configured are the ones that apply here.
	Hasher *auth.Hasher
	// Lockout is the same policy Login applies, and it is here for the same
	// reason Hasher is: redemption verifies a password, so it is a second place
	// an account's password can be guessed at. Without it the lockout an
	// operator configured covered one of the two doors (F51). The zero value
	// disables lockout, which is what a threshold of zero means everywhere else.
	Lockout auth.LockoutPolicy
	// Audit records the three lifecycle events. Nil records nothing.
	Audit audit.Recorder
	// Notify tells the inviter their invitation was accepted. Nil tells nobody.
	Notify notify.Notifier
	// Mail queues the invitation message. Nil is an instance with no relay,
	// where the copyable link is the whole delivery path.
	Mail Enqueuer
	Log  *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Hasher == nil {
		// Refused rather than defaulted. Redemption verifies and creates
		// passwords, and a hasher this package invented for itself would use
		// costs the operator did not configure.
		return nil, errors.New("invite: no hasher")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.TTL <= 0 {
		// Config validation refuses a non-positive TTL, so this only catches a
		// caller that built a Config by hand. A week matches the documented
		// default rather than "never expires", which D29 refused outright.
		cfg.TTL = 168 * time.Hour
	}
	return &Service{
		pool:   pool,
		q:      dbgen.New(pool),
		hasher: cfg.Hasher,
		cfg:    cfg,
		log:    cfg.Log,
	}, nil
}

// orgWideAuthority is the gate every invitation verb passes, and the authority
// the ones with a rank bound then read from.
//
// An invitation is D44's canonical organization-wide object: what redemption
// writes is a membership with no workspace_id, so the invitation belongs to the
// organization and not to any corner of it. Reaching one — issuing, listing,
// choosing a role for, revoking — therefore takes members.write carried by an
// organization-wide membership. `Can` is the wrong question and is checked first
// only so that somebody holding the permission nowhere gets the plain refusal:
// it answers from the union of every membership matching the workspace being
// acted in (D31), which lends a workspace-scoped admin's reach to an
// organization-wide object.
//
// One helper rather than the check written four times, because the four had
// drifted: Create was corrected under M28's reopening and the other three kept
// the old reading, which is how a workspace-scoped admin came to be able to list
// every pending invitee and irreversibly revoke an owner's invitation of a
// co-owner. `doing` names the verb so the refusal still says which one was
// refused.
func (s *Service) orgWideAuthority(
	ctx context.Context, actor *auth.Identity, doing string,
) (auth.Authority, error) {
	none := auth.Authority{Rank: auth.NoRoleRank}
	if !actor.Can(PermWrite) {
		return none, fmt.Errorf("%w: %s requires %s", domain.ErrForbidden, doing, PermWrite)
	}
	authority, err := auth.LoadMembershipAuthority(ctx, s.q, actor.UserID, actor.OrgID, PermWrite)
	if err != nil {
		return none, err
	}
	orgWide := authority.In(nil)
	if !orgWide.Granted {
		return none, fmt.Errorf(
			"%w: an invitation belongs to the whole organization, so %s requires %s from an "+
				"organization-wide membership; yours reaches one workspace",
			domain.ErrForbidden, doing, PermWrite)
	}
	return orgWide, nil
}

// CreateInput describes a new invitation.
type CreateInput struct {
	Email string
	// Role is a built-in role slug. Empty means DefaultRole.
	Role string
}

// Create issues an invitation and returns the link, which is the only time the
// token exists.
//
// Issuing one is an organization-wide act, because what redemption writes is an
// organization-wide membership — Redeem sets no workspace_id, deliberately, so
// somebody who accepts is in the organization rather than in one corner of it.
// The authority to do that therefore has to come from an organization-wide
// membership too (D44, M28's reopening): a workspace-scoped admin resolves
// inside their own workspace holding members.write, and admitting a new
// organization-wide member with it would hand out reach they do not have
// (F27).
func (s *Service) Create(ctx context.Context, actor *auth.Identity, in CreateInput) (*Created, error) {
	orgWide, err := s.orgWideAuthority(ctx, actor, "issuing one")
	if err != nil {
		return nil, err
	}

	email := auth.NormalizeEmail(in.Email)
	if err := auth.ValidateEmail(email); err != nil {
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "invalid", Message: "that does not look like an email address",
		}}
	}

	roleSlug := strings.TrimSpace(in.Role)
	if roleSlug == "" {
		roleSlug = DefaultRole
	}
	role, err := s.q.GetBuiltinRoleBySlug(ctx, roleSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ValidationErrors{{
				Field: "role", Code: "invalid",
				Message: "that is not a role: choose owner, admin, editor or viewer",
			}}
		}
		return nil, fmt.Errorf("look up role %q: %w", roleSlug, err)
	}

	// The D28 ceiling. Rank counts downward in authority, so "at or below the
	// inviter's own rank" is a rank no smaller than theirs. An authority that
	// granted nothing carries auth.NoRoleRank and fails every comparison, which
	// is the direction this must fail in.
	//
	// Read from the organization-wide authority rather than from the identity,
	// for the reason the gate above is: the identity's rank can be the rank of a
	// workspace-scoped membership, and an invitation is not scoped to a
	// workspace.
	if role.Rank < orgWide.Rank {
		return nil, fmt.Errorf(
			"%w: an invitation cannot carry a role above your own (%s)", domain.ErrForbidden, orgWide.Role)
	}

	// The D43 cap, and the reason it sits next to the ceiling above rather than
	// replacing it: they bound different things. The ceiling asks what the
	// issuer outranks; this asks what the issuer *is*. A key that may invite is
	// a key that may create an interactive principal, and a principal is not a
	// credential — nothing revokes it with the key, and requireSessionActor
	// cannot tell it from an account somebody registered (F29).
	//
	// Refused here rather than at redemption because the row stores role_id: a
	// bound applied at creation is a bound the stored invitation already
	// carries, so nothing has to be remembered about the credential that issued
	// it and no column exists to forget.
	if actor.IsAPIKey() {
		if _, ok := auth.KeyIssuableRoles[role.Slug]; !ok {
			return nil, fmt.Errorf(
				"%w: an invitation issued with an API key may carry editor or viewer, not %s",
				domain.ErrForbidden, role.Slug)
		}
	}

	// Asked before issuing, and safe to answer: the actor holds members.write
	// on this organization, so its membership is something they may already
	// read. Redemption asks the same question again and answers it with the
	// generic refusal, because there the asker is a stranger.
	members, err := s.q.CountMembershipsForEmail(ctx, dbgen.CountMembershipsForEmailParams{
		OrganizationID: actor.OrgID, Email: email,
	})
	if err != nil {
		return nil, fmt.Errorf("check membership: %w", err)
	}
	if members > 0 {
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "already_member",
			Message: "that address is already a member of this organization",
		}}
	}

	token, hash, err := auth.NewOpaqueToken(TokenBytes)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// A lapsed invitation still occupies the outstanding slot, because the
	// partial unique index cannot mention the clock. Clearing it here is what
	// makes re-inviting after a lapse work; re-inviting over a *live* one is
	// refused below rather than silently superseding a link somebody holds.
	if _, err := q.RevokeLapsedInvitation(ctx, dbgen.RevokeLapsedInvitationParams{
		OrganizationID: actor.OrgID, Email: email,
	}); err != nil {
		return nil, fmt.Errorf("clear lapsed invitation: %w", err)
	}

	inviter := actor.UserID
	row, err := q.CreateInvitation(ctx, dbgen.CreateInvitationParams{
		ID:             uuid.Must(uuid.NewV7()),
		OrganizationID: actor.OrgID,
		Email:          email,
		RoleID:         role.ID,
		TokenHash:      hash,
		InvitedBy:      &inviter,
		ExpiresAt:      time.Now().Add(s.cfg.TTL),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ValidationErrors{{
				Field: "email", Code: "outstanding",
				Message: "an invitation for that address is already outstanding; revoke it first",
			}}
		}
		return nil, fmt.Errorf("create invitation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	out := &Created{
		Invitation: Invitation{
			ID:        row.ID,
			Email:     row.Email,
			Role:      role.Slug,
			InvitedBy: actor.Email,
			Status:    StatusPending,
			CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt,
		},
		URL: s.linkFor(token),
	}

	// After the write and outside it, like every other audit emission in this
	// tree: the invitation is what the administrator asked for, and failing it
	// because the record could not be written would trade a missing audit line
	// for an invitation that was not issued. Logged at warn so the gap is
	// visible to whoever goes looking.
	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionInvitationCreated,
		TargetType: "invitation",
		TargetID:   &row.ID,
		Metadata: map[string]any{
			"email":      row.Email,
			"role":       role.Slug,
			"expires_at": row.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})

	out.Emailed = s.mail(ctx, out, actor, s.orgName(ctx, actor.OrgID))
	return out, nil
}

// orgName is what an invitation calls the organization, falling back to a
// phrase rather than to an empty string: a mail reading "join  as editor" is
// worse than one that does not name the organization at all.
func (s *Service) orgName(ctx context.Context, orgID uuid.UUID) string {
	name, err := s.q.GetOrganizationName(ctx, orgID)
	if err != nil || strings.TrimSpace(name) == "" {
		return "an organization"
	}
	return name
}

// List returns the organization's invitations, newest first.
//
// Gated on the same organization-wide authority Create is, and not merely
// because symmetry is tidy. Every row discloses an invitee's address, the role
// they were offered and who offered it, for an object nobody scoped to one
// workspace has any reach over. Refusing the page whole is also the only honest
// shape: gating Revoke alone would draw a list of rows whose only button answers
// 403.
func (s *Service) List(ctx context.Context, actor *auth.Identity) ([]Invitation, error) {
	if _, err := s.orgWideAuthority(ctx, actor, "listing invitations"); err != nil {
		return nil, err
	}
	rows, err := s.q.ListInvitations(ctx, actor.OrgID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	now := time.Now()
	out := make([]Invitation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Invitation{
			ID:         r.ID,
			Email:      r.Email,
			Role:       r.RoleSlug,
			InvitedBy:  r.InvitedByLabel,
			Status:     statusOf(r.RevokedAt, r.RedeemedAt, r.ExpiresAt, now),
			CreatedAt:  r.CreatedAt,
			ExpiresAt:  r.ExpiresAt,
			RevokedAt:  r.RevokedAt,
			RedeemedAt: r.RedeemedAt,
		})
	}
	return out, nil
}

// Role is one choice in the invite form.
type Role struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Rank        int32  `json:"rank"`
}

// Roles lists the roles this actor may invite at: their own rank and below
// (D28), most powerful first.
//
// Read from the seeded rows rather than listed in Go, so the four built-in
// roles have one definition and a form cannot offer something the ceiling check
// will then refuse. An actor whose role did not resolve carries auth.NoRoleRank
// and is offered nothing, which is the direction this fails in.
//
// The rank is read from the organization-wide authority, exactly as Create's
// ceiling is. Reading it from the identity was the same mistake one step
// earlier: the identity's rank can be borrowed from a workspace-scoped
// membership, so an organization-wide viewer who is an admin in one workspace
// was offered admin here and refused it at the ceiling.
//
// The D43 cap is applied for the same reason — the list is what a control
// renders, and offering a key a role Create will refuse is the same disagreement
// in the other direction.
func (s *Service) Roles(ctx context.Context, actor *auth.Identity) ([]Role, error) {
	orgWide, err := s.orgWideAuthority(ctx, actor, "choosing an invitation's role")
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListBuiltinRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	out := make([]Role, 0, len(rows))
	for _, r := range rows {
		if r.Rank < orgWide.Rank {
			continue
		}
		if actor.IsAPIKey() {
			if _, ok := auth.KeyIssuableRoles[r.Slug]; !ok {
				continue
			}
		}
		out = append(out, Role{Slug: r.Slug, Name: r.Name, Description: r.Description, Rank: r.Rank})
	}
	return out, nil
}

// Revoke ends an invitation that has not been redeemed.
//
// An id belonging to another organization answers not-found, the same as one
// that never existed, so ids cannot be probed. So does an invitation that was
// already revoked or already redeemed: a redeemed invite has produced a member,
// and reporting "revoked" for it would claim something that did not happen.
//
// The organization-wide gate matters most here of the four, because this verb
// is the irreversible one. A revoked invitation cannot be un-revoked and Create
// refuses a workspace-scoped actor a replacement, so without it somebody with
// reach over one workspace could stop an owner staffing their own organization.
func (s *Service) Revoke(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if _, err := s.orgWideAuthority(ctx, actor, "revoking an invitation"); err != nil {
		return err
	}
	n, err := s.q.RevokeInvitation(ctx, dbgen.RevokeInvitationParams{
		ID: id, OrganizationID: actor.OrgID,
	})
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionInvitationRevoked,
		TargetType: "invitation",
		TargetID:   &id,
	})
	return nil
}

// Offer describes a redeemable invitation to whoever holds its link.
//
// ErrNotRedeemable for anything that is not currently redeemable, so a spent,
// revoked, expired or invented token are one answer. Reads without locking: a
// GET must not let a stranger hold a write lock on a row by opening a page.
func (s *Service) Offer(ctx context.Context, token string) (*Offer, error) {
	if token == "" {
		return nil, ErrNotRedeemable
	}
	row, err := s.q.PeekInvitationByTokenHash(ctx, auth.HashOpaqueToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotRedeemable
		}
		return nil, fmt.Errorf("look up invitation: %w", err)
	}
	if statusOf(row.RevokedAt, row.RedeemedAt, row.ExpiresAt, time.Now()) != StatusPending {
		return nil, ErrNotRedeemable
	}
	return &Offer{
		OrganizationName: row.OrganizationName,
		Role:             row.RoleSlug,
		ExpiresAt:        row.ExpiresAt,
	}, nil
}

// AdmitsNewAccounts reports whether redemption may create an account that does
// not exist yet.
//
// It exists so the question has **one** answer. The redemption page used to
// derive it a second time, from `Config.Auth.SignupMode != SignupClosed`, while
// enforcement read `cfg.NewAccounts` — set from
// `signupSvc.Effective().AdmitsNewAccounts()`, which is the *effective* mode
// rather than the configured one. Those agree today and are not the same
// expression: `open` with no mailer degrades to `invite`, and anything that made
// the effective mode diverge further would move the enforcement and leave the
// page behind, offering a form whose submission is refused (F19).
//
// A nil receiver answers false, which is what a caller with no invite service
// configured should show: no form for an account it cannot create.
func (s *Service) AdmitsNewAccounts() bool {
	return s != nil && s.cfg.NewAccounts
}

// RedeemInput is an attempt to join.
type RedeemInput struct {
	Token string
	// Email must be the address the invitation was issued to (D27). It is asked
	// for rather than taken from a session, because the invitation names an
	// address and the account signed in elsewhere in this browser may not be it.
	Email string
	// Name is used only when the account is being created, and defaults to the
	// local part of the address.
	Name string
	// Password authenticates an existing account, or becomes the password of the
	// account being created.
	Password string
}

// Redeem turns an invitation into a membership.
//
// Every failure is ErrNotRedeemable and spends argon2 work on the way out, so
// nothing here answers "does this address have an account" — not by status
// code, not by message, not by how long it took. The two validation errors it
// can return are about the submitted values alone and reveal nothing: an
// address that is not an address, and a password below the length floor.
//
// The cost is equal where that question can be asked, and is not equal
// everywhere (F128). One run: no invitation with that token, an invitation that
// is not pending, an address that does not match it, a closed instance with no
// account, an unusable account, and a wrong password. **Two** runs: the account
// was created concurrently, the address is already a member, the membership
// insert lost a race, and the invitation was spent concurrently — each reached
// after a real verification or after createUser's hash, with refuse spending a
// dummy on top of it. Every one of those needs a *correct* password for the
// invited address, or is the losing half of two redemptions racing. Somebody who
// can provoke the doubled path can already sign in as that account and read its
// memberships, so the imbalance is stated here rather than equalised: making
// refuse conditional on work already done would trade a comment that is wrong in
// letter for an enumeration oracle whenever the condition is.
//
// One transaction, and single-use is the database's to enforce: the invitation
// row is locked on the way in, and the write that spends it is conditional on
// it still being unspent.
// boolText renders a flag for a mail template.
//
// RenderMail takes map[string]string deliberately — that is what lets it
// neutralize every value on the way in — so a boolean arrives as a word and the
// template compares it. "yes" and "" rather than "true"/"false" because an empty
// string is falsey to text/template's `if`, which keeps the template readable.
func boolText(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func (s *Service) Redeem(ctx context.Context, in RedeemInput) (*Redeemed, error) {
	// Checked before anything is looked up, so the answer cannot depend on
	// whether an account exists. Every stored password already satisfies this
	// floor — Hash refuses below it — so an existing account cannot be locked
	// out of redeeming by it.
	if len(in.Password) < auth.MinPasswordLength {
		return nil, domain.ValidationErrors{{
			Field: "password", Code: "too_short",
			Message: fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength),
		}}
	}
	email := auth.NormalizeEmail(in.Email)
	if err := auth.ValidateEmail(email); err != nil {
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "invalid", Message: "that does not look like an email address",
		}}
	}
	if in.Token == "" {
		return nil, ErrNotRedeemable
	}

	// refuse spends the work a real verification would and returns the one
	// error. Called on every path that fails after the token was looked up, so a
	// refusal that would otherwise return in microseconds costs what verifying a
	// real password costs — which is the pair that has to be equal. The paths
	// where this lands on top of a real run instead of standing in for one are
	// enumerated in Redeem's doc.
	refuse := func(why string, args ...any) error {
		s.hasher.DummyVerify(in.Password)
		s.log.Debug("invitation refused: "+why, args...)
		return ErrNotRedeemable
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	inv, err := q.GetInvitationByTokenHash(ctx, auth.HashOpaqueToken(in.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse("no invitation with that token")
		}
		return nil, fmt.Errorf("look up invitation: %w", err)
	}
	if statusOf(inv.RevokedAt, inv.RedeemedAt, inv.ExpiresAt, time.Now()) != StatusPending {
		return nil, refuse("invitation is not pending", slog.String("invitation", inv.ID.String()))
	}
	// The D27 comparison. Against the generated lowercase column, so it does not
	// depend on either side remembering to fold case.
	if inv.EmailLower == nil || *inv.EmailLower != email {
		return nil, refuse("address does not match the invitation",
			slog.String("invitation", inv.ID.String()))
	}

	var (
		userID  uuid.UUID
		created bool
	)
	user, err := q.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// D7: under `closed` the environment ceiling is absolute and an invite
		// may only add users who already exist. No session, and no invitation,
		// opens a closed instance.
		if !s.cfg.NewAccounts {
			return nil, refuse("signup is closed and the address has no account")
		}
		id, err := s.createUser(ctx, q, email, in.Name, in.Password)
		if err != nil {
			if isUniqueViolation(err) {
				// Two redemptions of the same address racing. The loser refuses
				// rather than retrying, and the invitation is still unspent.
				return nil, refuse("account was created concurrently")
			}
			return nil, err
		}
		userID, created = id, true
	case err != nil:
		return nil, fmt.Errorf("look up user: %w", err)
	default:
		if user.Status != "active" || user.PasswordHash == nil {
			return nil, refuse("account is not usable", slog.String("status", user.Status))
		}
		// A locked account is locked here too. This is the second door onto the
		// same password, and it used to be the unbolted one: five wrong guesses
		// at /auth/login lock the account, and the sixth through a redemption
		// was answered on its merits (F51). Honouring it costs the invitation
		// nothing — the token is not spent, and the person can redeem once the
		// window passes.
		if user.LockedUntil != nil && user.LockedUntil.After(time.Now()) {
			return nil, refuse("account is locked out",
				slog.String("invitation", inv.ID.String()))
		}
		if err := s.hasher.Verify(in.Password, *user.PasswordHash); err != nil {
			if !errors.Is(err, auth.ErrMismatch) {
				// A malformed stored hash is an operational fault, not a failed
				// redemption. Surface it rather than reporting a generic refusal
				// while the corrupt row goes unnoticed.
				return nil, fmt.Errorf("verify password for user %s: %w", user.ID, err)
			}
			// Recorded on s.q rather than on q, and that is the whole point
			// rather than a detail: this path returns an error, the deferred
			// rollback discards everything the transaction did, and a failure
			// counted inside it would be counted into nothing. The write is on
			// its own connection and touches only the users row.
			if _, rerr := s.q.RecordFailedLogin(ctx, dbgen.RecordFailedLoginParams{
				ID:             user.ID,
				Threshold:      s.cfg.Lockout.ThresholdParam(),
				LockoutSeconds: s.cfg.Lockout.WindowSecondsParam(),
			}); rerr != nil {
				s.log.Warn("could not record a failed redemption attempt",
					slog.String("user", user.ID.String()), slog.Any("error", rerr))
			}
			s.log.Debug("invitation refused: wrong password",
				slog.String("invitation", inv.ID.String()))
			return nil, ErrNotRedeemable
		}
		n, err := q.CountMembershipsForUser(ctx, dbgen.CountMembershipsForUserParams{
			OrganizationID: inv.OrganizationID, UserID: user.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("check membership: %w", err)
		}
		if n > 0 {
			return nil, refuse("already a member", slog.String("invitation", inv.ID.String()))
		}
		userID = user.ID
	}

	// Membership only (D6). No personal organization and no workspace: the
	// invited person is a colleague in an organization that already exists, and
	// provisioning them one of their own is what would make them a tenant.
	//
	// workspace_id NULL, so the membership covers every workspace in the
	// organization — the same shape registration creates.
	if _, err := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
		ID:             uuid.Must(uuid.NewV7()),
		UserID:         userID,
		OrganizationID: inv.OrganizationID,
		RoleID:         inv.RoleID,
		WorkspaceID:    nil,
	}); err != nil {
		if isUniqueViolation(err) {
			return nil, refuse("membership already exists")
		}
		return nil, fmt.Errorf("create membership: %w", err)
	}

	// Single-use, spent here. Conditional on the invitation still being
	// redeemable, so this could not succeed twice even without the lock taken
	// above; zero rows rolls the whole transaction back.
	spent, err := q.MarkInvitationRedeemed(ctx, dbgen.MarkInvitationRedeemedParams{
		ID: inv.ID, RedeemedBy: &userID,
	})
	if err != nil {
		return nil, fmt.Errorf("spend invitation: %w", err)
	}
	if spent == 0 {
		return nil, refuse("invitation was spent concurrently",
			slog.String("invitation", inv.ID.String()))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	out := &Redeemed{
		UserID:           userID,
		Email:            email,
		OrganizationID:   inv.OrganizationID,
		OrganizationName: inv.OrganizationName,
		Role:             inv.RoleSlug,
		Created:          created,
	}

	// Recorded against the person who joined, not the administrator who invited
	// them: they are who took this action. The invitation's own record already
	// names the inviter, and the two read as one story.
	s.record(ctx, &auth.Identity{
		UserID: userID,
		Email:  email,
		OrgID:  inv.OrganizationID,
	}, audit.Event{
		Action:     audit.ActionInvitationRedeemed,
		TargetType: "invitation",
		TargetID:   &inv.ID,
		Metadata: map[string]any{
			"email":           email,
			"role":            inv.RoleSlug,
			"account_created": created,
		},
	})
	s.notifyInviter(ctx, inv, out)

	return out, nil
}

// createUser makes the account an invitation admits, inside the redemption
// transaction.
//
// A user row and nothing else. The organization, workspace and owner membership
// that auth.Register provisions alongside one are exactly what D6 says must not
// happen here.
func (s *Service) createUser(ctx context.Context, q *dbgen.Queries, email, name, password string) (uuid.UUID, error) {
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return uuid.Nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
		ID:           uuid.Must(uuid.NewV7()),
		Email:        email,
		Name:         name,
		PasswordHash: &hash,
		Status:       "active",
		// Not marked verified. Holding the link is evidence somebody received
		// it, not evidence of who: under D27 the address is bound, but nothing
		// here proves the redeemer reads mail at it, and claiming otherwise
		// would put a false fact in the column a later milestone will trust.
		EmailVerifiedAt: nil,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return user.ID, nil
}

// linkFor builds the copyable invitation URL.
func (s *Service) linkFor(token string) string {
	return strings.TrimSuffix(s.cfg.AppURL, "/") + "/invite/" + token
}

// record writes one audit event, logging rather than failing when it cannot.
func (s *Service) record(ctx context.Context, actor *auth.Identity, e audit.Event) {
	if s.cfg.Audit == nil {
		return
	}
	if err := s.cfg.Audit.Record(ctx, actor, e); err != nil {
		s.log.Warn("invitation changed but the audit record was not written",
			slog.String("action", e.Action), slog.Any("error", err))
	}
}

// mail queues the invitation message, reporting whether one was queued.
//
// Nil mailer is an instance with no relay, which is the default (D1): nothing
// is queued, nothing fails, and the copyable link in the response is the whole
// delivery path. A failure to queue is logged and not returned — the invitation
// exists and its link works, and refusing the whole operation because the
// outbox write failed would throw away a grant that was already made.
func (s *Service) mail(ctx context.Context, c *Created, actor *auth.Identity, org string) bool {
	if s.cfg.Mail == nil {
		return false
	}
	inviter := actor.Name
	if inviter == "" {
		inviter = actor.Email
	}
	if err := s.cfg.Mail.Enqueue(ctx, c.Email, MailKind, map[string]string{
		"Inviter":      inviter,
		"Organization": org,
		"Role":         c.Role,
		"URL":          c.URL,
		"Email":        c.Email,
		"Expires":      c.ExpiresAt.UTC().Format("2 January 2006, 15:04 UTC"),
		// Whether redemption may create an account, which the mail asserted
		// unconditionally until 0.2.0 (F54). The service has always known —
		// `Redeem` reads the same field — but `RenderMail` was handed six keys
		// and none of them was this, so the template could not have branched
		// even if somebody had wanted it to. A closed instance with a relay is
		// an intended, documented state, and the redemption page tells the
		// truth about it while the mail that got somebody there did not.
		"NewAccounts": boolText(s.cfg.NewAccounts),
	}); err != nil {
		s.log.Warn("invitation issued but the mail was not queued",
			slog.String("invitation", c.ID.String()), slog.Any("error", err))
		return false
	}
	return true
}

// notifyInviter tells whoever sent the invitation that it was accepted.
//
// In-app, which is the baseline M22 built; there is no mail counterpart because
// the person being told is by definition someone who uses the dashboard. A
// failure is logged: the membership exists either way, and losing the
// notification is smaller than losing the join.
func (s *Service) notifyInviter(ctx context.Context, inv dbgen.GetInvitationByTokenHashRow, out *Redeemed) {
	if s.cfg.Notify == nil || inv.InvitedBy == nil {
		return
	}
	if err := s.cfg.Notify.Notify(ctx, *inv.InvitedBy, notify.Event{
		Kind:  notify.KindInviteAccepted,
		Title: out.Email + " accepted your invitation",
		Body: fmt.Sprintf("They joined %s as %s.",
			inv.OrganizationName, inv.RoleSlug),
		Data: map[string]any{
			"email":      out.Email,
			"role":       inv.RoleSlug,
			"invitation": inv.ID.String(),
		},
	}); err != nil {
		s.log.Warn("invitation redeemed but the inviter was not notified",
			slog.String("invitation", inv.ID.String()), slog.Any("error", err))
	}
}

// statusOf derives an invitation's state from its three columns and the clock.
//
// Order matters: revoked beats redeemed beats expired. A revoked invitation that
// has also lapsed is revoked — that is what somebody did to it — and expiry is
// only the answer when nothing else happened.
func statusOf(revoked, redeemed *time.Time, expires, now time.Time) string {
	switch {
	case revoked != nil:
		return StatusRevoked
	case redeemed != nil:
		return StatusRedeemed
	case !expires.After(now):
		return StatusExpired
	default:
		return StatusPending
	}
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
