package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
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
	if cfg.Lockout.Threshold == 0 {
		cfg.Lockout = DefaultLockout
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

func ValidateEmail(email string) error {
	e := NormalizeEmail(email)
	if e == "" || len(e) > 320 || !emailPattern.MatchString(e) {
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
		if isUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	orgID := uuid.Must(uuid.NewV7())
	org, err := q.CreateOrganization(ctx, dbgen.CreateOrganizationParams{
		ID:         orgID,
		Name:       name,
		Slug:       slugify(name) + "-" + orgID.String()[:8],
		IsPersonal: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create organization: %w", err)
	}

	wsID := uuid.Must(uuid.NewV7())
	ws, err := q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{
		ID:             wsID,
		OrganizationID: org.ID,
		Name:           "Default",
		Slug:           "default",
	})
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	ownerRole, err := q.GetRoleBySlug(ctx, "owner")
	if err != nil {
		return nil, fmt.Errorf("look up owner role: %w", err)
	}

	// workspace_id is NULL: the membership covers every workspace in the
	// organization, which is what a personal organization always wants.
	if _, err := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
		ID:             uuid.Must(uuid.NewV7()),
		UserID:         user.ID,
		OrganizationID: org.ID,
		RoleID:         ownerRole.ID,
		WorkspaceID:    nil,
	}); err != nil {
		return nil, fmt.Errorf("create membership: %w", err)
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
// Every failure returns ErrInvalidCredentials regardless of cause — unknown
// email, wrong password, no local password set. Distinguishing them tells an
// attacker which addresses are registered.
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

	ws, err := s.q.GetDefaultWorkspaceForUser(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}

	token, hash, err := NewSessionToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(s.ttl.Absolute)

	ipPrefix := AnonymizeIP(in.IP)
	session, err := s.q.CreateSession(ctx, dbgen.CreateSessionParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    user.ID,
		TokenHash: hash,
		IpPrefix:  nullable(ipPrefix),
		UserAgent: nullable(truncate(in.UserAgent, 512)),
		ExpiresAt: expires,
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	identity, err := s.identityFor(ctx, user.ID, user.Email, user.Name, ws.ID, ws.OrganizationID)
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

	ws, err := s.q.GetDefaultWorkspaceForUser(ctx, row.UserID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}

	// last_seen_at drives idle expiry, but writing on every request would turn
	// the hottest authenticated query into a write. Once a minute is enough
	// resolution for a seven-day idle window.
	if time.Since(row.LastSeenAt) > time.Minute {
		_ = s.q.TouchSession(ctx, row.ID)
	}

	identity, err := s.identityFor(ctx, row.UserID, row.Email, row.Name, ws.ID, ws.OrganizationID)
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
	ws, err := s.q.GetDefaultWorkspaceForUser(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
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

	hash, err := s.hasher.Hash(next)
	if err != nil {
		return err
	}
	if err := s.q.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
		ID: userID, PasswordHash: &hash,
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
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

	role := ""
	if r, err := s.q.GetUserRoleInWorkspace(ctx, dbgen.GetUserRoleInWorkspaceParams{
		UserID: userID, ID: wsID,
	}); err == nil {
		role = r.Slug
	}

	return &Identity{
		UserID:      userID,
		Email:       email,
		Name:        name,
		WorkspaceID: wsID,
		OrgID:       orgID,
		Role:        role,
		permissions: set,
	}, nil
}

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

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

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

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
