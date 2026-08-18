// Package recovery repairs a forgotten password.
//
// **It is a defect being closed, not a feature being added** (finding F141).
// Until M51 a forgotten password locked the account out permanently — true of
// every account on every instance, including the one that administers the box —
// and the only route back was an operator rewriting an argon2 hash in the
// database on somebody's behalf. Nothing here is scoped by taste; it is scoped
// by what makes that finding's claim false.
//
// Its own package rather than a method on internal/auth, matching internal/invite
// and internal/signup: the two other places a bearer-shaped token admits somebody
// holding no credential each own their table, their mail kind and their refusals,
// and a third one folded into the session service would be the first exception.
//
// Four ideas run through it.
//
// **The mailbox is the authority, and it is the only one.** There are no
// security questions, no backup address and no administrator-initiated reset.
// The first two are worse than the mailbox; the third is a permission-model
// question, and inventing one inside a recovery milestone is what D38 declined
// to do inside a signup milestone. `lctl instance principal move` (D98) already
// repairs *who administers the box*, which is a different question from *who
// knows this password*.
//
// **A request answers the same way whatever the address is.** Same body, same
// status, and the same argon2 cost either way — so neither the response nor a
// stopwatch says whether an address is registered. The answer goes to the
// address by mail, which is where it belongs: the channel that proves the
// address exists is the mailbox, and the person holding the mailbox is entitled
// to know somebody tried. That is signup's stance (F13) rather than a second one
// invented here, and its cost is the same one signup accepted — mail is sent to
// addresses that never registered, bounded by the login limiter.
//
// **With no mailer, it refuses out loud.** This is the one place in the product
// that deliberately breaks the *degrades mail-free* pattern D1 established, and
// it breaks it by refusing rather than by degrading into nothing: a reset request
// that silently succeeds into a void is worse than the lockout it was meant to
// cure. `SMTP_HOST` unset is the shipped default, so on a default instance this
// is a route that says the instance has no mailer and names the operator's route
// back.
//
// **A successful reset ends every session and every other token.** A recovery
// that leaves the thief's session alive has recovered nothing. API keys are
// deliberately not revoked — a key is a separate credential with its own
// rotation story (D9, D87), and taking them out on a password reset would make
// recovery an outage.
package recovery

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
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// TTL is how long an emailed reset link stays usable.
//
// A constant rather than a variable, for the reason signup.VerificationTTL is
// one: an invitation's window is an administrator's policy about somebody else's
// onboarding and D29 made that tunable, where this is a person finishing
// something they started minutes ago. One hour rather than signup's day, because
// the two links are not worth the same — a verification token creates an account
// whose password the holder does not know, and this one sets a password on an
// account that already exists. Requesting again supersedes the old link, so
// nobody is ever stuck waiting for this to lapse.
const TTL = time.Hour

// ConsumedRetentionDays is how long a spent row is kept before the purge takes
// it. Matches signup.ConsumedRetentionDays, and short for the same reason: the
// password is the durable evidence and the audit log holds the rest.
const ConsumedRetentionDays = 7

// TokenBytes is the entropy in a reset token, matching an invitation's and a
// verification's.
const TokenBytes = 32

// MailKind names the template that carries the link, which is also the outbox's
// `kind` column and the filename in internal/ui/templates/mail.
const MailKind = "password-reset"

// MailKindUnavailable is what an address that cannot be recovered gets instead:
// no account here, or an account this mechanism refuses.
//
// **One template for both, deliberately.** It exists so the *response* does not
// have to distinguish them — the mail reaches the address and only its owner
// reads it, where a status code reaches whoever typed the address into the form
// (F13, mirroring `account-exists` at signup). Splitting it in two would put the
// distinction back into the one channel that is allowed to carry it, which is
// fine, and would also mean a suspended account's owner learns their status from
// a form somebody else filled in. The message names both possibilities and
// points at the operator, who is the only person who can act on either.
const MailKindUnavailable = "password-reset-unavailable"

// Errors this package returns that a caller distinguishes.
var (
	// ErrNoMailer is a reset asked for on an instance with no relay configured.
	//
	// Loud, and that is the decision rather than an accident. Every other
	// consumer of the mailer degrades when it is absent; this one cannot,
	// because the mail *is* the mechanism. Answering "check your inbox" to
	// somebody whose instance can send nothing is the failure mode this refusal
	// exists to prevent.
	ErrNoMailer = errors.New("recovery: this instance has no mailer configured")

	// ErrNotResettable is every failure to complete a reset: no such token,
	// expired, already spent, an account whose status is not active, an account
	// with no password to replace.
	//
	// **One error for all of them, answered 404 and never 410.** A caller
	// enumerating cannot tell the five apart, which is the point: 410 would
	// confirm that a token existed, and a distinct refusal for a suspended
	// account would tell whoever holds the link what state the account is in.
	// Saying that is the operator's job and not this form's.
	ErrNotResettable = errors.New("recovery: this link is no longer valid")
)

// Enqueuer is internal/mail's writing half, as this package needs it.
//
// Declared here rather than imported so "no mailer configured" is a nil
// interface rather than a flag every call site has to remember to check, and so
// a test satisfies it in four lines.
type Enqueuer interface {
	Enqueue(ctx context.Context, to, kind string, data map[string]string) error
}

// Service requests and completes password resets.
type Service struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	hasher *auth.Hasher
	cfg    Config
	log    *slog.Logger
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree: the package doing the work does not
// read the environment.
type Config struct {
	// AppURL is the origin a reset link points at.
	AppURL string
	// Hasher writes the new password, at the cost parameters the operator
	// configured, and equalizes the timing of a request. Required: a hasher this
	// package invented for itself would use costs nobody chose.
	Hasher *auth.Hasher
	// Mail delivers the link. Nil is an instance with no relay, and it is the one
	// dependency here whose absence is a refusal rather than a degradation.
	Mail Enqueuer
	// Audit records the completed reset. Nil records nothing.
	Audit audit.Recorder
	Log   *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Hasher == nil {
		return nil, errors.New("recovery: no hasher")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{
		pool:   pool,
		q:      dbgen.New(pool),
		hasher: cfg.Hasher,
		cfg:    cfg,
		log:    cfg.Log,
	}, nil
}

// MailerConfigured reports whether this instance can deliver a reset link.
//
// Read by the two surfaces before they draw a form, so somebody does not type an
// address into a page that was never going to send anything. Request refuses
// again on its own, because a surface remembering to ask is not the invariant.
func (s *Service) MailerConfigured() bool { return s.cfg.Mail != nil }

// Requested is what a caller tells the person afterwards.
//
// The normalized address and nothing else. No expiry, no identifier, no hint of
// which branch ran — the whole value of this struct is that there is only one
// shape of it. signup.Registered carries an ExpiresAt and had to truncate it to
// microseconds so Go's nanosecond clock did not answer, in the fractional
// digits, the question the status code no longer did (F13). The cheapest way not
// to repeat that is to return no timestamp: the mail says when the link lapses,
// and the mail only reaches somebody who has an account.
type Requested struct {
	Email string
}

// Request mints a reset link and mails it, if the address can be recovered.
//
// The refusals are silent by construction. Every path below returns the same
// Requested value and pays the same argon2 cost, so the caller cannot tell an
// address with an account from one without, and neither can a stopwatch. What
// differs is only which message lands in the mailbox.
func (s *Service) Request(ctx context.Context, address string) (*Requested, error) {
	// Before anything else, because it is the one refusal that is about the
	// instance rather than about the address, and it is not a secret: an
	// operator who has configured no relay knows it, and a stranger learns
	// nothing they could not learn from the sign-up form, which already refuses
	// for the same reason.
	if s.cfg.Mail == nil {
		return nil, ErrNoMailer
	}

	email := auth.NormalizeEmail(address)
	if err := auth.ValidateEmail(email); err != nil {
		return nil, domain.ValidationErrors{{
			Field: "email", Code: "invalid", Message: "that does not look like an email address",
		}}
	}

	// The timing equalizer, spent before the lookup and on every branch.
	//
	// There is no password at this form, so this hash verifies nothing; it exists
	// so that minting a token, inserting a row and queueing a mail is not
	// measurably more work than queueing one mail. Argon2 is this request by two
	// orders of magnitude, which is what makes one dummy hash enough. D27 spends
	// the same work on redemption and signup.Register spends it on registration,
	// both for exactly this reason.
	//
	// The cost is real and it is bounded by the shared login limiter rather than
	// by anything here: this route is registered under the same `guard(...)` as
	// POST /login, so an attacker cannot burn one budget without burning the
	// other. That sharing is deliberate and is why this milestone adds no rate
	// limit of its own.
	s.hasher.DummyVerify(email)

	user, err := s.q.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.unavailable(ctx, email)
	case err != nil:
		return nil, fmt.Errorf("look up user: %w", err)
	}

	// The two accounts this mechanism refuses, and it refuses them here rather
	// than at the link: minting a token for an account that cannot use it would
	// send somebody a link that fails, which is a worse answer than the mail
	// they get instead.
	//
	// A status other than `active` is the suspension seam (auth.Service.Login's
	// own check). A suspended account is not recoverable by the person who lost
	// the password — whether it comes back is the operator's decision and saying
	// so is the operator's job.
	//
	// A null `password_hash` is the SSO-only seam. Nothing builds SSO this phase
	// so the branch is unreachable in practice today; it is refused rather than
	// left to do something surprising when M53 or a later phase makes it
	// reachable. Writing a password onto an account that deliberately has none
	// would be this mechanism inventing a second way in.
	if user.Status != "active" || user.PasswordHash == nil {
		return s.unavailable(ctx, email)
	}

	token, tokenHash, err := auth.NewOpaqueToken(TokenBytes)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Supersede whatever was outstanding. The old link stops working at the same
	// moment, so there is never more than one live link per account, and somebody
	// whose first mail never arrived is not locked out until the window lapses.
	if _, err := q.ConsumePasswordResets(ctx, user.ID); err != nil {
		return nil, fmt.Errorf("supersede outstanding resets: %w", err)
	}
	row, err := q.CreatePasswordReset(ctx, dbgen.CreatePasswordResetParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(TTL),
	})
	if err != nil {
		return nil, fmt.Errorf("create password reset: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// The mail is the whole delivery path — nobody is standing beside this person
	// to hand them a URL, unlike an invitation's copyable link. So a failure to
	// queue is returned rather than logged and swallowed: the row is useless
	// without it, and the next request supersedes this one anyway.
	if err := s.cfg.Mail.Enqueue(ctx, user.Email, MailKind, map[string]string{
		"URL":     s.linkFor(token),
		"Email":   user.Email,
		"Expires": row.ExpiresAt.UTC().Format("2 January 2006, 15:04 UTC"),
	}); err != nil {
		return nil, fmt.Errorf("queue password reset mail: %w", err)
	}

	return &Requested{Email: user.Email}, nil
}

// unavailable is the branch for an address this instance will not mail a link
// to, and it is the reason the response can be identical either way.
func (s *Service) unavailable(ctx context.Context, email string) (*Requested, error) {
	if err := s.cfg.Mail.Enqueue(ctx, email, MailKindUnavailable, map[string]string{
		"Email": email,
	}); err != nil {
		return nil, fmt.Errorf("queue password reset unavailable mail: %w", err)
	}
	return &Requested{Email: email}, nil
}

// Completed is a reset that landed.
type Completed struct {
	UserID uuid.UUID
	Email  string
}

// Reset writes a new password against a token and ends every session for the
// account.
//
// The new password goes through auth.WritePassword, which is the function
// POST /account/password reaches through auth.Service.ChangePassword — one
// password-writing code path in the product and not two, which is what the
// milestone asked for by name rather than two call sites that happen to agree.
func (s *Service) Reset(ctx context.Context, token, password string) (*Completed, error) {
	if s.cfg.Mail == nil {
		// Symmetrical with Request, and not merely tidiness. An instance whose
		// relay was removed after a link went out must not still be completing
		// resets from mail it can no longer send: the same reason signup.Verify
		// re-checks the effective mode, which D7 states as a bound on the state
		// the instance is in rather than on the moment a request passed through.
		return nil, ErrNoMailer
	}

	// Length is checked before the token is looked at, so a password that is too
	// short answers the same 422 whether or not the token is real. The other
	// order would let somebody guessing tokens tell a hit from a miss by
	// submitting a password they knew would be rejected, without spending the
	// token to find out.
	if len(password) < auth.MinPasswordLength {
		return nil, domain.ValidationErrors{{
			Field: "password", Code: "too_short",
			Message: fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength),
		}}
	}
	if token == "" {
		return nil, ErrNotResettable
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.GetPasswordResetByTokenHash(ctx, auth.HashOpaqueToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotResettable
		}
		return nil, fmt.Errorf("look up reset: %w", err)
	}
	// Four refusals collapsed into one answer. Spent, lapsed, suspended, and an
	// account with no password to replace are indistinguishable to a caller, and
	// each has its own test.
	//
	// The last two are re-checked here and not only at Request because an
	// account's state can change while a link sits in an inbox — the same
	// reasoning signup.Verify applies to the signup mode.
	if row.ConsumedAt != nil ||
		!row.ExpiresAt.After(time.Now()) ||
		row.Status != "active" ||
		row.PasswordHash == nil {
		return nil, ErrNotResettable
	}

	if err := auth.WritePassword(ctx, q, s.hasher, row.UserID, password); err != nil {
		return nil, err
	}

	// This token and every sibling. A recovery that leaves a second live token
	// behind has recovered nothing.
	spent, err := q.ConsumePasswordResets(ctx, row.UserID)
	if err != nil {
		return nil, fmt.Errorf("spend reset: %w", err)
	}
	if spent == 0 {
		// Unreachable while the row above was unconsumed and locked FOR UPDATE,
		// and checked anyway: zero rows means something else spent it inside this
		// transaction's window, and rolling back is the only correct answer.
		return nil, ErrNotResettable
	}

	// Every session, with none kept — `KeepSession` left nil. That is the
	// difference from ChangePassword, which keeps the browser the change was made
	// in: there is no session here to keep, and the whole point of a recovery is
	// that somebody else may be holding one.
	if err := q.RevokeAllUserSessions(ctx, dbgen.RevokeAllUserSessionsParams{
		UserID: row.UserID,
	}); err != nil {
		return nil, fmt.Errorf("revoke sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, row.UserID, row.Email)

	return &Completed{UserID: row.UserID, Email: row.Email}, nil
}

// record writes the audit entry for a completed reset.
//
// **The actor is the account itself**, because that is who took the action —
// the same choice invitation redemption makes for the person who joined, and
// for the same reason: nobody else was present, and an entry attributed to
// `system` would lose the one fact worth recording.
//
// The identity carries no organization, so the row lands with a NULL
// `organization_id`. That is correct rather than convenient: there is no session
// and therefore no workspace the person was standing in, and an account may
// belong to several organizations — stamping the record with whichever one was
// found first is the misattribution F36 named. The consequence is that
// `audit.read.instance` is what reads it, held by the principal, which is the
// right audience for "somebody recovered an account on this box".
//
// The network fact is the request's IP prefix and nothing more, and there is no
// metadata at all: audit.Record takes the address from the context and reduces
// it to a prefix there, so no caller ever holds a full address destined for this
// table. The privacy stance is inherited and this is not where it bends. What
// the entry says is *this account's password was reset, from this network, at
// this time*, which is the whole of what an operator reads it for.
//
// Logged and not returned on failure, deliberately. The reset has committed by
// the time this runs; losing the record of it is bad, and losing the recovery
// itself because the record failed is worse.
func (s *Service) record(ctx context.Context, userID uuid.UUID, email string) {
	if s.cfg.Audit == nil {
		return
	}
	err := s.cfg.Audit.Record(ctx, &auth.Identity{UserID: userID, Email: email}, audit.Event{
		Action:     audit.ActionPasswordReset,
		TargetType: "user",
		TargetID:   &userID,
	})
	if err != nil {
		s.log.Error("could not record a password reset",
			slog.String("user", userID.String()), slog.Any("error", err))
	}
}

// PurgeFinished removes lapsed and spent rows, reporting how many went. Called
// by the hourly maintenance pass, for the reason the signup sweep exists: a
// waiting room with no sweep is a table that grows forever with nothing watching
// it.
func (s *Service) PurgeFinished(ctx context.Context, batch int32) (int64, error) {
	n, err := s.q.PurgeFinishedPasswordResets(ctx, dbgen.PurgeFinishedPasswordResetsParams{
		KeepDays: ConsumedRetentionDays,
		Batch:    batch,
	})
	if err != nil {
		return 0, fmt.Errorf("recovery: purge finished password resets: %w", err)
	}
	return n, nil
}

// linkFor builds the reset URL.
func (s *Service) linkFor(token string) string {
	return strings.TrimSuffix(s.cfg.AppURL, "/") + "/reset/" + token
}
