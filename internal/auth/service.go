package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

var (
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrInvalidEmail       = errors.New("auth: invalid email address")
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	ErrAccountLocked      = errors.New("auth: account temporarily locked")
	ErrAccountInactive    = errors.New("auth: account is not active")
	ErrSignupClosed       = errors.New("auth: registration is closed")
)

// Identity is an authenticated user together with the workspace they are
// acting in. Both the REST handlers and the dashboard handlers resolve to this
// same type, so authorization cannot diverge between the two surfaces.
type Identity struct {
	UserID      uuid.UUID
	Email       string
	Name        string
	WorkspaceID uuid.UUID
	OrgID       uuid.UUID
	SessionID   uuid.UUID
	Role        string
	// RoleRank orders roles against each other: lower binds tighter, so owner
	// (10) outranks admin (20) outranks editor (30) outranks viewer (40).
	//
	// Carried on the identity rather than looked up where it is needed because
	// it is a property of who the actor is, exactly like Role, and the first
	// consumer — the invitation role ceiling (D28) — must not be able to reach
	// the wrong membership by asking a second time. It fails closed: an identity
	// whose role could not be resolved gets NoRoleRank, which outranks nothing.
	RoleRank int32
	// APIKeyID is set when the request authenticated with an API key instead
	// of a session cookie. Services consult it for the few operations that
	// must require an interactive sign-in; everything else is deliberately
	// blind to which credential was used.
	APIKeyID    *uuid.UUID
	permissions map[string]struct{}
}

// Can reports whether the identity holds a permission.
//
// This is the RBAC evaluator, and it is deliberately called from the service
// layer rather than from middleware. Middleware only knows the route; the
// service knows which workspace the object being touched belongs to, which is
// the question that actually matters.
func (i *Identity) Can(permission string) bool {
	if i == nil {
		return false
	}
	_, ok := i.permissions[permission]
	return ok
}

// Permissions returns the identity's permissions, for API-key scope
// intersection and for rendering the UI.
func (i *Identity) Permissions() []string {
	out := make([]string, 0, len(i.permissions))
	for p := range i.permissions {
		out = append(out, p)
	}
	return out
}

// IsAPIKey reports whether the request authenticated with an API key.
func (i *Identity) IsAPIKey() bool { return i != nil && i.APIKeyID != nil }

// restrictTo intersects the identity's permissions with a set of scopes.
//
// Intersection, never replacement: a key cannot hold a permission its owner
// lacks, so revoking a role revokes every key that leant on it. Called with
// the key's scopes on every request rather than stored, so a role change takes
// effect immediately.
func (i *Identity) restrictTo(scopes []string) {
	allowed := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		if _, held := i.permissions[s]; held {
			allowed[s] = struct{}{}
		}
	}
	i.permissions = allowed
}

// Service owns registration, login and session lifecycle.
type Service struct {
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	hasher  *Hasher
	ttl     SessionTTL
	lockout LockoutPolicy
}

type ServiceConfig struct {
	Params  Params
	TTL     SessionTTL
	Lockout LockoutPolicy
}

func NewService(pool *pgxpool.Pool, cfg ServiceConfig) *Service {
	// Threshold 0 means "no lockout", because that is what the configuration
	// layer promises: LOGIN_LOCKOUT_THRESHOLD validates as "must be 0 (no
	// limit) or positive" and .env.example says 0 disables. Substituting the
	// default here made that promise false — an operator who set 0 got the
	// standard five-strike lockout and no indication otherwise. The variable
	// carries envDefault:"5", so an unset one never reaches this as 0.
	//
	// Only the window is defaulted, and only when a caller left it unset.
	if cfg.Lockout.Window <= 0 {
		cfg.Lockout.Window = DefaultLockout.Window
	}
	return &Service{
		pool:    pool,
		q:       dbgen.New(pool),
		hasher:  NewHasher(cfg.Params),
		ttl:     cfg.TTL,
		lockout: cfg.Lockout,
	}
}

// Hasher exposes the configured hasher for the CLI, which creates users
// outside a request.
func (s *Service) Hasher() *Hasher { return s.hasher }

// NeedsSetup reports whether the instance has no users yet.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := s.q.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n == 0, nil
}

// emailPattern is deliberately permissive. Fully validating an address in a
// regex is not possible, and over-strict patterns reject valid addresses
// (plus-addressing, new TLDs, unicode domains). Deliverability is proven by
// sending mail, not by a pattern.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// NormalizeEmail trims and lowercases. The database also stores a generated
// lowercase column, so comparison never depends on the caller remembering.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail is the gate on every path that writes an address: creating the
// first account, issuing an invitation, and starting a registration. It is not
// on the login path, where the address is compared and never sent to.
//
// The regex above is permissive on purpose, and the second check is what stops
// permissive becoming unsendable. `net/mail.ParseAddress` is the parser the
// mailer itself uses, so an address that passes the pattern and fails the parser
// is one this product will accept, store, and then fail to send to — which is
// what F53 was: nine forms including `a<b@c.de`, `a,b@c.de` and
// `user@exa(mple.com` matched the pattern, committed a `pending_registrations`
// row, and then answered 500 from the enqueue, a status the API does not
// declare. Checking here rather than in signup closes it for invitations too,
// which reach the same enqueue through a different door.
//
// Strictly a narrowing: every address the parser accepts and the pattern does
// not — `Barry Gibbs <bg@example.com>` is the shape — is still refused, because
// the pattern runs first and because a display-name form is not the address
// somebody typed.
func ValidateEmail(email string) error {
	e := NormalizeEmail(email)
	if e == "" || len(e) > 320 || !emailPattern.MatchString(e) {
		return ErrInvalidEmail
	}
	parsed, err := mail.ParseAddress(e)
	if err != nil || parsed.Address != e {
		return ErrInvalidEmail
	}
	return nil
}

// RegisterInput describes a new account.
type RegisterInput struct {
	Email    string
	Name     string
	Password string
	// IsFirstUser marks the setup flow, which is permitted even when signup is
	// closed — otherwise a fresh closed instance could never create its first
	// account.
	IsFirstUser bool
}

// Register creates a user with their personal organization, workspace and
// owner membership, in one transaction.
//
// Provisioning all four together is what lets Phase 1 behave as a single-user
// product while every row already carries the tenancy columns Phase 2 needs.
// A user without a workspace would be a state no other code path expects, so
// it must not be possible to observe one.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Identity, error) {
	if err := ValidateEmail(in.Email); err != nil {
		return nil, err
	}
	// Enforced here, not only in the HTML form: the API is a first-class
	// surface, and a policy that one client can skip is not a policy.
	if err := validatePasswordLength(in.Password, "password"); err != nil {
		return nil, err
	}
	email := NormalizeEmail(in.Email)

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// The setup flow's "is this instance empty?" has to be answered inside the
	// transaction that acts on the answer, under a lock that makes the pair
	// atomic. The caller's earlier NeedsSetup check is a UI affordance; this is
	// the enforcement. Without it two concurrent setup posts could both create
	// a first user, and on a closed instance the second one is an intruder.
	if in.IsFirstUser {
		if err := q.LockFirstUserSetup(ctx); err != nil {
			return nil, fmt.Errorf("lock setup: %w", err)
		}
		n, err := q.CountUsers(ctx)
		if err != nil {
			return nil, fmt.Errorf("count users: %w", err)
		}
		if n > 0 {
			// Not ErrEmailTaken: the address may be fine, the instance is
			// simply already set up. Signup rules apply from here on.
			return nil, ErrSignupClosed
		}
	}

	userID := uuid.Must(uuid.NewV7())
	user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
		ID:           userID,
		Email:        email,
		Name:         name,
		PasswordHash: &hash,
		Status:       "active",
		// The first user is trusted by construction: they had filesystem or
		// deploy access to reach the setup page.
		EmailVerifiedAt: verifiedAt(in.IsFirstUser),
	})
	if err != nil {
		if pgerr.IsUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	org, ws, err := ProvisionOrganization(ctx, q, user.ID, name, true)
	if err != nil {
		return nil, err
	}

	// The setup account becomes the instance-level principal (D98), here and
	// nowhere else on this path — `Register` without IsFirstUser is ordinary
	// self-serve registration, and conferring instance reach there would rebuild
	// F15 exactly: under LINKCTRL_SIGNUP_MODE=open, one registration would make
	// a stranger the person who moderates every organization's destinations.
	//
	// This is the right home for it because it is the only place in the product
	// where "this account is the instance's" is already established rather than
	// assumed. The branch above holds an advisory lock and re-counts users inside
	// this transaction, so exactly one account can ever take it — and the comment
	// on EmailVerifiedAt says what that account is: somebody who had filesystem or
	// deploy access to reach the setup page. That is the claim to the box; every
	// other account has a claim to a tenant.
	//
	// In the same transaction, so an instance is never observable in the state
	// where it has been claimed and has nobody who can administer it.
	if in.IsFirstUser {
		if err := grantInstancePrincipal(ctx, q, user.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return s.identityFor(ctx, user.ID, user.Email, user.Name, ws.ID, org.ID)
}

// LoginInput is a sign-in attempt.
type LoginInput struct {
	Email     string
	Password  string
	IP        netip.Addr
	UserAgent string
}

type LoginResult struct {
	Identity *Identity
	Token    string
	Expires  time.Time
}

// Login authenticates and starts a session.
//
// **Every failure is answered identically, and every failure costs the same.**
// Unknown address, wrong password, no local password set, suspended account, and
// an account already locked out by repeated failures are one answer to whoever
// asked, and each spends one argon2 verification on the way. Distinguishing any
// of them — by problem type, by status, by prose, or by how long the refusal
// takes — tells a stranger which addresses are registered.
//
// The errors below stay distinct because the process wants them: a lockout is a
// different operational event from a typo, and a test can assert it. What must
// not differ is what a caller sees, so the two boundaries that answer a person
// collapse them — internal/httpx/problem.go for the API, internal/httpx/web.go
// for the sign-in form. That split is the one ErrAccountInactive has always had.
//
// Finding F92 is why both halves are spelled out here. ErrAccountLocked used to
// reach the API as its own problem type and a 429, so the fifth wrong password
// against a registered address answered differently from the fifth against an
// unregistered one — unauthenticated, on the shipped `closed` default, where the
// registration oracle is refused before any lookup, and inside LOGIN_RATE_PER_MIN
// so the per-address limiter never masked it. It also returned before any
// verification, which made it *fast* where every other refusal pays a hash; a fix
// that equalised the status and not the work would have left the question
// answerable with a stopwatch.
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	email := NormalizeEmail(in.Email)

	user, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the same work as a real verification, so the response time
			// does not reveal that the account does not exist.
			s.hasher.DummyVerify(in.Password)
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("look up user: %w", err)
	}

	if user.LockedUntil != nil && user.LockedUntil.After(time.Now()) {
		// Spend the verification this branch is about to skip. Without it a
		// locked account refuses in one database round trip where every other
		// refusal costs an argon2 hash, and that gap answers "is this address
		// registered?" on its own — five wrong passwords, then time the sixth.
		// The lockout still holds whatever the password was: this buys the
		// timing back, not an early exit.
		s.hasher.DummyVerify(in.Password)
		return nil, ErrAccountLocked
	}
	if user.Status != "active" {
		s.hasher.DummyVerify(in.Password)
		return nil, ErrAccountInactive
	}
	if user.PasswordHash == nil {
		// SSO-only or erased account.
		s.hasher.DummyVerify(in.Password)
		return nil, ErrInvalidCredentials
	}

	if err := s.hasher.Verify(in.Password, *user.PasswordHash); err != nil {
		if !errors.Is(err, ErrMismatch) {
			// A malformed stored hash is an operational fault, not a user
			// error. Surface it rather than reporting "wrong password" while
			// the corrupt row goes unnoticed.
			return nil, fmt.Errorf("verify password for user %s: %w", user.ID, err)
		}
		res, rerr := s.q.RecordFailedLogin(ctx, dbgen.RecordFailedLoginParams{
			ID:             user.ID,
			Threshold:      s.lockout.ThresholdParam(),
			LockoutSeconds: s.lockout.WindowSecondsParam(),
		})
		if rerr == nil && res.LockedUntil != nil && res.LockedUntil.After(time.Now()) {
			return nil, ErrAccountLocked
		}
		return nil, ErrInvalidCredentials
	}

	// Correct password. Upgrade the hash if the cost policy has risen since it
	// was written; this is the only moment the plaintext is available.
	if s.hasher.NeedsRehash(*user.PasswordHash) {
		if newHash, herr := s.hasher.Hash(in.Password); herr == nil {
			_ = s.q.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
				ID: user.ID, PasswordHash: &newHash,
			})
		}
	}

	if err := s.q.RecordSuccessfulLogin(ctx, user.ID); err != nil {
		return nil, fmt.Errorf("record login: %w", err)
	}

	// No session id: this is the request that creates one. So a sign-in starts
	// at the pinned default, or at the last workspace used, and the session
	// carries that from its first row rather than being corrected afterwards.
	//
	// An account that belongs to nothing signs in anyway (D36). The session is
	// created with no workspace, which is a value the column already permits —
	// sessions.workspace_id is nullable and is SET NULL when a workspace is
	// deleted, so a signed-in browser was always going to reach this state; what
	// changes here is that signing in can start in it.
	ws, err := s.resolveWorkspace(ctx, user.ID, nil, nil)
	if err != nil && !errors.Is(err, ErrNoWorkspace) {
		return nil, err
	}
	orphaned := errors.Is(err, ErrNoWorkspace)

	token, hash, err := NewSessionToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(s.ttl.Absolute)

	ipPrefix := AnonymizeIP(in.IP)
	params := dbgen.CreateSessionParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    user.ID,
		TokenHash: hash,
		IpPrefix:  nullable(ipPrefix),
		UserAgent: nullable(truncate(in.UserAgent, 512)),
		ExpiresAt: expires,
	}
	if !orphaned {
		params.WorkspaceID = &ws.ID
	}
	session, err := s.q.CreateSession(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	var identity *Identity
	if orphaned {
		identity, err = s.identityWithoutOrganization(ctx, user.ID, user.Email, user.Name)
	} else {
		identity, err = s.identityFor(
			ctx, user.ID, user.Email, user.Name, ws.ID, ws.OrganizationID)
	}
	if err != nil {
		return nil, err
	}
	identity.SessionID = session.ID

	return &LoginResult{Identity: identity, Token: token, Expires: expires}, nil
}

// Authenticate resolves a session token to an identity.
func (s *Service) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrSessionNotFound
	}
	row, err := s.q.GetSessionByTokenHash(ctx, HashSessionToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("look up session: %w", err)
	}

	sess := Session{
		ID:         row.ID,
		UserID:     row.UserID,
		CreatedAt:  row.CreatedAt,
		LastSeenAt: row.LastSeenAt,
		ExpiresAt:  row.ExpiresAt,
	}
	if err := s.ttl.Valid(sess, time.Now()); err != nil {
		return nil, err
	}
	if row.Status != "active" {
		return nil, ErrAccountInactive
	}

	// The session's id is passed, so wherever this browser last switched to wins
	// over the account-level preference. That is the difference between "where
	// do I start" and "where am I now", and it is why the two are stored apart.
	//
	// This is the line D36 is really about. Every authenticated request in the
	// product passes through here, so treating "belongs to nothing" as a failure
	// would turn one owner's tenancy teardown into an authentication outage for
	// the person it orphaned — they would be unable to sign in and therefore
	// unable to create the organization the product wants to offer them.
	ws, err := s.resolveWorkspace(ctx, row.UserID, &row.ID, nil)
	if err != nil && !errors.Is(err, ErrNoWorkspace) {
		return nil, err
	}

	// last_seen_at drives idle expiry, but writing on every request would turn
	// the hottest authenticated query into a write. Once a minute is enough
	// resolution for a seven-day idle window.
	if time.Since(row.LastSeenAt) > time.Minute {
		_ = s.q.TouchSession(ctx, row.ID)
	}

	var identity *Identity
	if errors.Is(err, ErrNoWorkspace) {
		identity, err = s.identityWithoutOrganization(ctx, row.UserID, row.Email, row.Name)
	} else {
		identity, err = s.identityFor(
			ctx, row.UserID, row.Email, row.Name, ws.ID, ws.OrganizationID)
	}
	if err != nil {
		return nil, err
	}
	identity.SessionID = row.ID
	return identity, nil
}

// IdentityForEmail resolves a user to an identity without a session.
//
// For the CLI, which acts as a named user rather than as root: `lctl apikey
// create` goes through the same service call and the same permission checks a
// request would, so the CLI cannot mint a key the user could not.
func (s *Service) IdentityForEmail(ctx context.Context, email string) (*Identity, error) {
	user, err := s.q.GetUserByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("look up user: %w", err)
	}
	if user.Status != "active" {
		return nil, ErrAccountInactive
	}
	// No session, so the account's own preference decides: the CLI acts where
	// the person would land if they signed in — including, since D36, nowhere.
	ws, err := s.resolveWorkspace(ctx, user.ID, nil, nil)
	if errors.Is(err, ErrNoWorkspace) {
		return s.identityWithoutOrganization(ctx, user.ID, user.Email, user.Name)
	}
	if err != nil {
		return nil, err
	}
	return s.identityFor(ctx, user.ID, user.Email, user.Name, ws.ID, ws.OrganizationID)
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.q.RevokeSession(ctx, sessionID)
}

// validatePasswordLength applies the only password rule the product has.
// Length and nothing else, per NIST SP 800-63B: composition rules push people
// toward predictable substitutions without adding entropy.
func validatePasswordLength(password, field string) error {
	if len(password) < MinPasswordLength {
		return domain.ValidationErrors{{
			Field: field, Code: "too_short",
			Message: fmt.Sprintf("the password must be at least %d characters", MinPasswordLength),
		}}
	}
	return nil
}

// WritePassword hashes a password and stores it against an account.
//
// **The product's one password-writing path**, and it is exported for that
// reason rather than for reuse. Two of them existed the moment M51 needed to
// write a password without a session to verify against: this function is what
// POST /account/password reaches through ChangePassword below, and what a
// completed recovery reaches through internal/recovery. One statement, one
// hasher, one place where `failed_login_count` and `locked_until` are cleared —
// which matters more than it looks, because an account recovered while locked
// out by the guessing that made its owner reset it would otherwise still refuse
// the new password.
//
// It takes the Queries rather than reading the service's own, so a caller inside
// a transaction passes the transactional handle and the write joins whatever
// else that transaction is doing. Recovery needs exactly that: spending the
// token and setting the password must not be separable.
func WritePassword(
	ctx context.Context, q *dbgen.Queries, h *Hasher, userID uuid.UUID, password string,
) error {
	hash, err := h.Hash(password)
	if err != nil {
		return err
	}
	if err := q.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
		ID: userID, PasswordHash: &hash,
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return nil
}

// ChangePassword updates a password and logs out every other session.
func (s *Service) ChangePassword(ctx context.Context, userID, keepSession uuid.UUID, current, next string) error {
	if err := validatePasswordLength(next, "new_password"); err != nil {
		return err
	}
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("look up user: %w", err)
	}
	if user.PasswordHash == nil {
		return ErrInvalidCredentials
	}
	if err := s.hasher.Verify(current, *user.PasswordHash); err != nil {
		return ErrInvalidCredentials
	}

	if err := WritePassword(ctx, s.q, s.hasher, userID, next); err != nil {
		return err
	}

	// Anyone holding the old password must lose their sessions; that is the
	// point of changing it. The current session is kept so the user is not
	// logged out of the browser they just used.
	return s.q.RevokeAllUserSessions(ctx, dbgen.RevokeAllUserSessionsParams{
		UserID:      userID,
		KeepSession: &keepSession,
	})
}

// identityFor loads the permission set for a user in a workspace.
func (s *Service) identityFor(ctx context.Context, userID uuid.UUID, email, name string, wsID, orgID uuid.UUID) (*Identity, error) {
	perms, err := s.q.GetUserPermissions(ctx, dbgen.GetUserPermissionsParams{
		UserID: userID,
		ID:     wsID,
	})
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", err)
	}
	set := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		set[p] = struct{}{}
	}
	if err := s.addInstanceGrants(ctx, userID, set); err != nil {
		return nil, err
	}

	role, rank := "", int32(NoRoleRank)
	if r, err := s.q.GetUserRoleInWorkspace(ctx, dbgen.GetUserRoleInWorkspaceParams{
		UserID: userID, ID: wsID,
	}); err == nil {
		role, rank = r.Slug, r.Rank
	}

	return &Identity{
		UserID:      userID,
		Email:       email,
		Name:        name,
		WorkspaceID: wsID,
		OrgID:       orgID,
		Role:        role,
		RoleRank:    rank,
		permissions: set,
	}, nil
}

// identityWithoutOrganization is who an account is when it belongs to nothing.
//
// Everything tenancy-shaped is zero: no workspace, no organization, no role, and
// no permission that came from a membership, so Can answers false for every one
// of those and every service call refuses on the check it already makes. That is
// the whole enforcement — there is no second authorization path for this state,
// and the one operation that must remain reachable from it (creating a first
// organization) opens its own door, at its own call site, where a reader can see
// it. See team.CreateOrganization and D36.
//
// **Instance grants survive it**, and that is the point of them (D98). An
// instance-level permission is held over the box rather than through a tenancy,
// so an operator whose only organization was deleted must not stop being able to
// review the instance's disputes — the alternative is that a tenancy teardown
// silently strands the queue, which is a sharper version of the finding this
// principal exists to close.
//
// RoleRank is NoRoleRank rather than zero for the reason the constant explains:
// rank counts downward in authority, so a zero here would read as outranking the
// owner role.
//
// A method rather than a function since D98, because there is now exactly one
// thing to load and it is not reached through any tenancy.
func (s *Service) identityWithoutOrganization(
	ctx context.Context, userID uuid.UUID, email, name string,
) (*Identity, error) {
	set := map[string]struct{}{}
	if err := s.addInstanceGrants(ctx, userID, set); err != nil {
		return nil, err
	}
	return &Identity{
		UserID:      userID,
		Email:       email,
		Name:        name,
		RoleRank:    NoRoleRank,
		permissions: set,
	}, nil
}

// addInstanceGrants folds a user's instance-level permissions into a set that
// already holds whatever their memberships granted.
//
// A union, never a replacement, and the two sources are deliberately
// indistinguishable afterwards: Identity.Can is the one evaluator, and a second
// "but is it an instance permission?" question asked at a call site is how the
// grants would drift out of step with the map that says which of them a key may
// hold.
//
// The instance permissions themselves are enumerated in migration 03400 and
// granted to no role, so this is the only way any of them reaches an identity.
func (s *Service) addInstanceGrants(
	ctx context.Context, userID uuid.UUID, into map[string]struct{},
) error {
	grants, err := s.q.ListInstanceGrants(ctx, userID)
	if err != nil {
		return fmt.Errorf("load instance grants: %w", err)
	}
	for _, g := range grants {
		into[g] = struct{}{}
	}
	return nil
}

// HasOrganization reports whether this identity belongs to an organization.
//
// False is a real, reachable state since D36 — an account whose only
// organization was deleted keeps its account and loses its tenancy — and it is
// what the dashboard reads to send somebody to the page that offers them one.
// It is an affordance, never the enforcement: what such an identity may do is
// decided by its empty permission set, like everybody else's.
func (i *Identity) HasOrganization() bool { return i != nil && i.OrgID != uuid.Nil }

// NoRoleRank is the rank of an identity whose role could not be resolved.
//
// math.MaxInt32 and not zero, and that choice is the whole safety property:
// rank counts *downward* in authority, so a zero would read as outranking the
// owner role. Anything comparing ranks fails closed against this value.
const NoRoleRank = math.MaxInt32

func verifiedAt(yes bool) *time.Time {
	if !yes {
		return nil
	}
	now := time.Now()
	return &now
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProvisionOrganization creates an organization, its first workspace and an
// owner membership for one user, inside the caller's transaction.
//
// Exported and taking a *dbgen.Queries rather than being a method, because two
// packages provision tenancy and there must not be two implementations of it.
// Registration calls it for the personal organization every account starts with
// (is_personal true); internal/team calls it for an organization somebody
// deliberately creates (is_personal false). The tenancy invariants — an
// organization always has a workspace, and always has an owner, both written in
// the same transaction as the row that needs them — are stated once, here.
//
// The caller owns the transaction and the commit. That is what lets registration
// create the user in the same one, and what keeps this function unable to leave
// a half-provisioned organization behind.
func ProvisionOrganization(
	ctx context.Context, q *dbgen.Queries, userID uuid.UUID, name string, isPersonal bool,
) (dbgen.Organization, dbgen.Workspace, error) {
	var (
		org dbgen.Organization
		ws  dbgen.Workspace
	)

	orgID := uuid.Must(uuid.NewV7())
	// A suffix from the id, because organization slugs are unique instance-wide
	// and names are not: two people called "Acme" must both be able to exist.
	//
	// The **last** twelve hex characters, not the first eight. A UUIDv7 begins
	// with the timestamp, and the leading eight characters are the top 32 bits
	// of a 48-bit millisecond clock — which means they change once every 65
	// seconds and are identical for everything created inside that window. Two
	// organizations of the same name created a few seconds apart therefore
	// produced the same slug and the second one failed on the unique index, as a
	// 500 rather than as anything a caller could act on. The trailing group is
	// the random half of a v7, so it does not have that property.
	org, err := q.CreateOrganization(ctx, dbgen.CreateOrganizationParams{
		ID:         orgID,
		Name:       name,
		Slug:       Slugify(name) + "-" + orgID.String()[24:],
		IsPersonal: isPersonal,
	})
	if err != nil {
		return org, ws, fmt.Errorf("create organization: %w", err)
	}

	ws, err = q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{
		ID:             uuid.Must(uuid.NewV7()),
		OrganizationID: org.ID,
		Name:           "Default",
		Slug:           "default",
	})
	if err != nil {
		return org, ws, fmt.Errorf("create workspace: %w", err)
	}

	ownerRole, err := q.GetRoleBySlug(ctx, "owner")
	if err != nil {
		return org, ws, fmt.Errorf("look up owner role: %w", err)
	}

	// workspace_id is NULL: the membership covers every workspace in the
	// organization, which is what the person who just created it always wants.
	if _, err := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
		ID:             uuid.Must(uuid.NewV7()),
		UserID:         userID,
		OrganizationID: org.ID,
		RoleID:         ownerRole.ID,
		WorkspaceID:    nil,
	}); err != nil {
		return org, ws, fmt.Errorf("create membership: %w", err)
	}

	return org, ws, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify reduces a name to the URL-safe form the tenancy tables store beside
// it. Exported because workspace renaming derives a slug the same way, and a
// second implementation would be a second answer to "what is this called".
func Slugify(s string) string { return slugify(s) }

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "workspace"
	}
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}
