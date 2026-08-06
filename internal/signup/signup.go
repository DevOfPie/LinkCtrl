// Package signup decides whether this instance admits new accounts, and admits
// them.
//
// One idea runs through all of it. **`LINKCTRL_SIGNUP_MODE` is the mode, and
// the operator is the only one who sets it** (D38). There is no stored toggle,
// no permission, and no endpoint: changing how an instance admits accounts is
// an `.env` edit and a restart. What this package computes on top of that
// variable is one derivation and no policy — `open` with no mailer is `invite`,
// because there would otherwise be no way to verify an address.
//
// Three consequences are worth stating before the code.
//
// **`open` requires a configured mailer** (D1). Open registration proves the
// address before an account exists at all: the form writes a pending
// registration and mails a link, and the user, organization and workspace are
// created when the link is followed. With no relay configured there is nothing
// to prove an address with, so the effective mode drops to `invite` — and the
// signup page refuses on GET rather than letting somebody fill a form in and
// discover it at submit time.
//
// **`closed` admits no new account by any path** (D7), which is why this
// package rather than the environment answers internal/invite's question about
// whether redemption may create an account. A mode that closed the signup form
// but left invitations creating accounts would make the word mean two things.
//
// **A self-registered account gets its own organization and workspace** (D6),
// which is the opposite of an invited one. That difference is the whole reason
// this milestone ships after invitations, and the form says it in words.
package signup

import (
	"context"
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
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

// Mode is how open this instance is to new accounts.
//
// Its own type rather than internal/config's, for the reason every service
// package here declares its own configuration: the package that does the work
// does not read the environment. The values are the same three words, so the
// wiring converts with a string conversion and no table of equivalences.
type Mode string

const (
	// Closed admits no new account by any path, invitations included (D7).
	Closed Mode = "closed"
	// Invite admits an account only through a redeemed invitation, where an
	// administrator named the address first.
	Invite Mode = "invite"
	// Open additionally admits anybody through the signup form, once they have
	// proven the address.
	Open Mode = "open"
)

// Valid reports whether m is one of the three modes.
func (m Mode) Valid() bool { return m == Closed || m == Invite || m == Open }

// AdmitsNewAccounts reports whether an account may be created at all — the
// question internal/invite asks before letting a redemption create one.
func (m Mode) AdmitsNewAccounts() bool { return m != Closed }

// VerificationTTL is how long an emailed verification link stays usable.
//
// A constant rather than a variable, unlike INVITE_TTL. An invitation's window
// is an administrator's policy about somebody else's onboarding, and D29 made it
// tunable for that reason; this window is a person finishing something they
// started minutes ago, and one day is generous for that without being a
// credential anybody has to think about. Registering again supersedes the old
// link, so nobody is ever stuck waiting for this to lapse.
const VerificationTTL = 24 * time.Hour

// ConsumedRetentionDays is how long a spent registration row is kept before the
// sweep removes it. Short, because the account it produced is the durable
// evidence and the audit log holds the rest.
const ConsumedRetentionDays = 7

// TokenBytes is the entropy in a verification token, matching an invitation's.
const TokenBytes = 32

// MailKind names the mail template, which is also the outbox's `kind` column.
const MailKind = "verification"

// MailKindExists is what a registration attempt on an address that already has
// an account sends instead. It exists so the *response* does not have to say
// so: the mail reaches the address, and only its owner reads it, where a status
// code reaches whoever typed the address into the form (F13).
const MailKindExists = "account-exists"

// Errors this package returns that a caller distinguishes.
var (
	// ErrClosed is registration refused because the effective mode is not
	// `open`. Returned from both the form and the API, and from verification —
	// an operator who closes sign-ups stops the accounts that were half-way
	// through, because D7's bound is absolute rather than a moment.
	ErrClosed = errors.New("signup: this instance does not accept sign-ups")
	// ErrNotVerifiable is every failure to complete a verification: no such
	// token, expired, already spent. One error for all of them, because the
	// holder of a bad link learns nothing from which.
	ErrNotVerifiable = errors.New("signup: this link is no longer valid")
	// ErrEmailTaken is an address that already has an account.
	ErrEmailTaken = auth.ErrEmailTaken
)

// Enqueuer is internal/mail's writing half, as this package needs it.
//
// Declared here rather than imported so "no mailer configured" is a nil
// interface rather than a flag every call site has to remember to check, and so
// a test satisfies it in four lines.
type Enqueuer interface {
	Enqueue(ctx context.Context, to, kind string, data map[string]string) error
}

// Service answers what the instance's signup mode is, and runs the two halves
// of an open registration.
type Service struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	hasher *auth.Hasher
	cfg    Config
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree.
type Config struct {
	// Mode is LINKCTRL_SIGNUP_MODE, and it is the whole of the policy (D38).
	Mode Mode
	// AppURL is the origin a verification link points at.
	AppURL string
	// Hasher hashes the password chosen at the form, at the cost parameters the
	// operator configured. Required: a hasher this package invented for itself
	// would use costs nobody chose.
	Hasher *auth.Hasher
	// Mail delivers the verification link. Nil is an instance with no relay, and
	// it is what lowers an `open` mode to `invite` — there is no other way to
	// prove an address, and `open` without one would be an open door with a
	// verification step that never happens (D1).
	Mail Enqueuer
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Hasher == nil {
		return nil, errors.New("signup: no hasher")
	}
	if !cfg.Mode.Valid() {
		return nil, fmt.Errorf("signup: %q is not a signup mode", cfg.Mode)
	}
	// No logger: nothing here logs. Every refusal is returned to a caller that
	// renders it, and the one fact an operator needs without asking — `open`
	// configured with no relay — is said once at boot by the process that wired
	// this, because it is a property of the configuration and not of a request.
	return &Service{pool: pool, q: dbgen.New(pool), hasher: cfg.Hasher, cfg: cfg}, nil
}

// MailerConfigured reports whether this instance can prove an address.
func (s *Service) MailerConfigured() bool { return s.cfg.Mail != nil }

// Effective is the mode that actually applies.
//
// `LINKCTRL_SIGNUP_MODE`, lowered to `invite` when no mailer is configured.
// That is the one derivation this package performs, and it exists because open
// registration verifies an address by email (D1): with no relay, `open` would
// be an open door with a verification step that never runs.
//
// No context and no error, because there is nothing to read. The mode is fixed
// for the life of the process, which is what makes "no session or API call can
// change it" a property of the shape rather than of a check.
func (s *Service) Effective() Mode {
	if s.cfg.Mode == Open && s.cfg.Mail == nil {
		return Invite
	}
	return s.cfg.Mode
}

// Configured is `LINKCTRL_SIGNUP_MODE` as the operator set it, before the
// mailer is taken into account. Logged at boot beside Effective, so "you
// configured `open` but there is no relay" is one line in the log rather than a
// mystery at the signup form.
func (s *Service) Configured() Mode { return s.cfg.Mode }

// RegisterInput is somebody filling in the signup form.
type RegisterInput struct {
	Email    string
	Name     string
	Password string
}

// Registered is what a caller tells the person afterwards.
type Registered struct {
	// Email is the normalized address the link was sent to. Echoed back so the
	// page can say which inbox to look in.
	Email string
	// ExpiresAt is when the link stops working.
	ExpiresAt time.Time
}

// Register starts an open-mode registration.
//
// It creates no account. Under D1 the address is proven first, so this writes a
// pending registration and queues the mail that carries the link; the user, the
// organization and the workspace are written by Verify, in one transaction, when
// somebody demonstrates they read mail at the address.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Registered, error) {
	if s.Effective() != Open {
		return nil, ErrClosed
	}
	// Unreachable while Effective lowers `open` to `invite` without one, and
	// checked anyway: this is the invariant D1 is actually about, and a future
	// caller that reaches Register another way must not be able to skip
	// verification.
	if s.cfg.Mail == nil {
		return nil, ErrClosed
	}

	email := auth.NormalizeEmail(in.Email)
	if err := auth.ValidateEmail(email); err != nil {
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "invalid", Message: "that does not look like an email address",
		}}
	}
	if len(in.Password) < auth.MinPasswordLength {
		return nil, domain.ValidationErrors{{
			Field: "password", Code: "too_short",
			Message: fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength),
		}}
	}

	// Hashed before the address is looked up, and that order is the point.
	// Argon2 is the expensive part of this request by two orders of magnitude,
	// so branching before it would make a taken address measurably faster than
	// a free one and hand back the oracle the answer below removes. D27 spends
	// the same work on redemption for the same reason.
	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, err
	}
	token, tokenHash, err := auth.NewOpaqueToken(TokenBytes)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)

	// A taken address gets mail, not a refusal.
	//
	// This endpoint answered 409 for a taken address from Phase 1 until 0.2.0,
	// and until M29 that was defensible: it was API-only, so asking it whether
	// an address is registered took a credential. A browser sign-up form is
	// what makes it a surface a stranger reaches, and rate limiting slows a
	// sweep of a leaked address list without removing the signal. M27 already
	// spends argon2 work so that redemption cannot be asked the same question
	// (D27), so the product had two surfaces disagreeing about whether this is
	// worth not disclosing.
	//
	// Both branches now answer 202 with the same body, cost the same argon2
	// work, and put one message in the outbox. The person who owns the address
	// is told what happened by mail — which is where the answer belongs,
	// because it reaches the address rather than whoever typed it. Nothing is
	// written: there is no pending registration to supersede, and creating one
	// would let a stranger invalidate the real owner's outstanding link.
	taken, err := s.q.CountUsersByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("check address: %w", err)
	}
	if taken > 0 {
		if err := s.cfg.Mail.Enqueue(ctx, email, MailKindExists, map[string]string{
			"Email": email,
		}); err != nil {
			return nil, fmt.Errorf("queue account-exists mail: %w", err)
		}
		// The window quoted to the caller is the one a real registration would
		// have had. Returning a zero time, or omitting the field, would
		// reintroduce the distinction the mail was sent to remove.
		//
		// Truncated to microseconds because the other branch's value has been
		// through Postgres, which stores `timestamptz` to a microsecond. Go's
		// clock is nanosecond, so an untruncated value serializes with three
		// more digits and the fractional part alone answers the question the
		// status code no longer does. The test compares whole bodies for
		// exactly this reason — it found this.
		return &Registered{
			Email:     email,
			ExpiresAt: time.Now().Add(VerificationTTL).Truncate(time.Microsecond),
		}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Supersede whatever was outstanding for this address. The old link stops
	// working at the same moment, so there is never more than one live link per
	// address, and somebody whose first mail never arrived is not locked out
	// until the window lapses.
	if _, err := q.DeleteOutstandingRegistration(ctx, email); err != nil {
		return nil, fmt.Errorf("clear outstanding registration: %w", err)
	}
	row, err := q.CreatePendingRegistration(ctx, dbgen.CreatePendingRegistrationParams{
		ID:           uuid.Must(uuid.NewV7()),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		TokenHash:    tokenHash,
		ExpiresAt:    time.Now().Add(VerificationTTL),
	})
	if err != nil {
		return nil, fmt.Errorf("create pending registration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// The mail is the whole delivery path here, unlike an invitation's copyable
	// link — nobody is standing beside this person to hand them a URL. So a
	// failure to queue is returned rather than logged and swallowed: the row is
	// useless without it, and the next attempt supersedes this one anyway.
	if err := s.cfg.Mail.Enqueue(ctx, row.Email, MailKind, map[string]string{
		"URL":     s.linkFor(token),
		"Email":   row.Email,
		"Expires": row.ExpiresAt.UTC().Format("2 January 2006, 15:04 UTC"),
	}); err != nil {
		return nil, fmt.Errorf("queue verification mail: %w", err)
	}

	return &Registered{Email: row.Email, ExpiresAt: row.ExpiresAt}, nil
}

// Verified is a completed registration.
type Verified struct {
	UserID      uuid.UUID
	Email       string
	Name        string
	WorkspaceID uuid.UUID
	OrgID       uuid.UUID
}

// Verify completes a registration, creating the account it was waiting on.
//
// D6 in one function: a self-registered account gets its own organization and
// its own workspace, and owner membership in it — which is exactly what an
// invited account does not get. auth.ProvisionOrganization is called rather than
// reimplemented, so there is one statement of what provisioning tenancy means.
//
// The effective mode is checked again here, and that is not belt-and-braces. A
// link lives for a day and an operator can close the instance inside that
// window — an `.env` edit and a restart — so a registration started while
// sign-ups were open must not still be able to land afterwards. D7's bound is a
// state the instance is in, not a moment a request passed through.
func (s *Service) Verify(ctx context.Context, token string) (*Verified, error) {
	if s.Effective() != Open {
		return nil, ErrClosed
	}
	if token == "" {
		return nil, ErrNotVerifiable
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.GetPendingRegistrationByTokenHash(ctx, auth.HashOpaqueToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotVerifiable
		}
		return nil, fmt.Errorf("look up registration: %w", err)
	}
	if row.ConsumedAt != nil || !row.ExpiresAt.After(time.Now()) {
		return nil, ErrNotVerifiable
	}

	name := strings.TrimSpace(row.Name)
	if name == "" {
		name = strings.SplitN(row.Email, "@", 2)[0]
	}

	now := time.Now()
	user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
		ID:           uuid.Must(uuid.NewV7()),
		Email:        row.Email,
		Name:         name,
		PasswordHash: &row.PasswordHash,
		Status:       "active",
		// Verified, and this is the one path in the product that can honestly
		// say so: following the link is evidence somebody reads mail at the
		// address. Invitation redemption deliberately leaves it null, because
		// holding a link proves receipt and not readership.
		EmailVerifiedAt: &now,
	})
	if err != nil {
		if pgerr.IsUniqueViolation(err) {
			// The address acquired an account while this link was in somebody's
			// inbox — most likely they were invited and joined. Not a generic
			// refusal: they have proven they read mail there, so telling them
			// the account exists reveals nothing they could not already confirm.
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	// D6: their own organization, their own workspace, owner membership in it.
	org, ws, err := auth.ProvisionOrganization(ctx, q, user.ID, name, true)
	if err != nil {
		return nil, err
	}

	// Single-use, spent here. Conditional on the row still being unspent, so
	// this could not succeed twice even without the lock taken above; zero rows
	// rolls the whole transaction back.
	spent, err := q.ConsumePendingRegistration(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("spend registration: %w", err)
	}
	if spent == 0 {
		return nil, ErrNotVerifiable
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &Verified{
		UserID: user.ID, Email: user.Email, Name: user.Name,
		WorkspaceID: ws.ID, OrgID: org.ID,
	}, nil
}

// PurgeLapsed removes registrations nobody completed and spent rows past the
// short retention window, reporting how many went. Called by the maintenance
// job, for the reason the outbox has a purge: a waiting room with no sweep is
// the one table that grows forever with nothing watching it.
func (s *Service) PurgeLapsed(ctx context.Context) (int64, error) {
	n, err := s.q.PurgeLapsedRegistrations(ctx, ConsumedRetentionDays)
	if err != nil {
		return 0, fmt.Errorf("signup: purge lapsed registrations: %w", err)
	}
	return n, nil
}

// linkFor builds the verification URL.
func (s *Service) linkFor(token string) string {
	return strings.TrimSuffix(s.cfg.AppURL, "/") + "/verify/" + token
}
