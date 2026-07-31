package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Permissions API key management itself requires.
const (
	PermAPIKeysRead  = "apikeys.read"
	PermAPIKeysWrite = "apikeys.write"
)

// ErrAPIKeyInvalid covers every reason a presented key does not authenticate:
// malformed, unknown, wrong secret, revoked, expired, or belonging to an
// account that is no longer active.
//
// One error rather than several. The distinction is of no use to a legitimate
// caller — the key list shows revocation and expiry, so the owner can already
// see which of theirs is which — and separate responses would tell whoever
// found a leaked key whether it is still worth trying elsewhere.
var ErrAPIKeyInvalid = errors.New("auth: api key is not valid")

// Token layout: "lk_live_" + 8-character public id + "_" + 43-character secret.
//
// The public id is stored and indexed, so verification is a single-row lookup
// rather than a scan comparing every hash. The tag is fixed-length and the id
// is fixed-length, which means the parts are taken by offset — splitting on
// "_" would break the moment a base64url secret contained one.
//
// "live" is there so a future test-mode key is distinguishable by eye rather
// than by asking the database. The whole token is one word with no spaces or
// punctuation beyond underscores, so it survives being pasted into a shell, a
// YAML file and a CI secret box unquoted.
const (
	apiKeyTag = "lk_live_"

	// 5 bytes because base32 encodes exactly 5 bytes as 8 characters with no
	// padding. 40 bits is not a secret — it is a lookup handle — and the
	// unique index catches the rare collision.
	apiKeyIDBytes = 5
	apiKeyIDChars = 8

	// 32 bytes of secret, which is the same entropy as a session token.
	apiKeySecretBytes = 32
	apiKeySecretChars = 43 // base64url, unpadded

	// APIKeyPrefixLength is the length of the public, storable part.
	APIKeyPrefixLength = len(apiKeyTag) + apiKeyIDChars
	apiKeyTokenLength  = APIKeyPrefixLength + 1 + apiKeySecretChars
)

// Lowercase base32 for the public id: no padding, no mixed case to transcribe,
// and no characters that need escaping anywhere a key gets pasted.
var apiKeyIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newAPIKeyToken returns a token together with its two parts.
func newAPIKeyToken() (token, prefix, secret string, err error) {
	idBuf := make([]byte, apiKeyIDBytes)
	if _, err := rand.Read(idBuf); err != nil {
		return "", "", "", fmt.Errorf("auth: read api key id: %w", err)
	}
	secretBuf := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(secretBuf); err != nil {
		return "", "", "", fmt.Errorf("auth: read api key secret: %w", err)
	}

	prefix = apiKeyTag + strings.ToLower(apiKeyIDEncoding.EncodeToString(idBuf))
	secret = base64.RawURLEncoding.EncodeToString(secretBuf)
	return prefix + "_" + secret, prefix, secret, nil
}

// ParseAPIKey splits a token into its public prefix and its secret.
//
// Everything about the shape is checked here so that a malformed token costs no
// database round trip, which is what stops a flood of junk Authorization
// headers turning into a flood of queries.
func ParseAPIKey(token string) (prefix, secret string, err error) {
	if len(token) != apiKeyTokenLength ||
		!strings.HasPrefix(token, apiKeyTag) ||
		token[APIKeyPrefixLength] != '_' {
		return "", "", ErrAPIKeyInvalid
	}

	prefix = token[:APIKeyPrefixLength]
	secret = token[APIKeyPrefixLength+1:]

	if _, err := apiKeyIDEncoding.DecodeString(strings.ToUpper(prefix[len(apiKeyTag):])); err != nil {
		return "", "", ErrAPIKeyInvalid
	}
	if _, err := base64.RawURLEncoding.DecodeString(secret); err != nil {
		return "", "", ErrAPIKeyInvalid
	}
	return prefix, secret, nil
}

// APIKeyHash is the value stored in api_keys.key_hash.
//
// HMAC-SHA256 with a pepper from configuration, so a database dump on its own
// does not permit offline verification. Deliberately not argon2: the secret is
// full-entropy random, so stretching buys nothing, and 64 MiB of work per
// request would not fit a 150ms API budget.
//
// The prefix is part of the message, which binds a hash to the row that holds
// it: a hash copied to another key's row no longer verifies.
func APIKeyHash(pepper []byte, prefix, secret string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(prefix))
	mac.Write([]byte{0})
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}

// NonDelegableScopes are permissions an API key may never hold, whatever its
// creator's role.
//
// Key management is the important one: a key that can mint keys makes
// revocation meaningless, because whoever holds a leaked key simply issues
// another before the original is cut off. So minting stays behind an
// interactive session, and org.delete follows the same rule — an irreversible
// action should require a human sign-in rather than a token in a CI variable.
//
// audit.read is here for a different reason, and the difference matters to
// whoever adds the next entry. It escalates nothing and reverses nothing; it is
// listed because of what it discloses. The audit log is the one place a network
// prefix is tied to a named person, so the rule this map encodes is now
// "escalating, irreversible, or disclosing" rather than only the first two.
//
// This map is the only thing that makes audit.read session-only. There is no
// second check in the handler or the service — the endpoint authorizes on the
// permission like every other endpoint — so if machine export ever outweighs
// the disclosure, deleting this one line is the whole change. See decisions.md.
var NonDelegableScopes = map[string]struct{}{
	PermAPIKeysRead:  {},
	PermAPIKeysWrite: {},
	"org.delete":     {},
	"audit.read":     {},
}

// APIKeyInfo is a key as its owner sees it. The secret is absent by
// construction: it is never stored, so it cannot be listed.
type APIKeyInfo struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// CreatedAPIKey is the response to creating a key: the record, plus the only
// copy of the token that will ever exist.
type CreatedAPIKey struct {
	APIKeyInfo
	Key string `json:"key"`
}

// CreateAPIKeyInput describes a new key.
type CreateAPIKeyInput struct {
	Name      string
	Scopes    []string
	ExpiresAt *time.Time
}

// APIKeyConfig configures the key service.
type APIKeyConfig struct {
	// Pepper keys the HMAC. Required; a short one is refused rather than
	// silently accepted, because a weak pepper is invisible in behaviour.
	Pepper []byte
	// UsageFlushInterval is how often buffered last_used_at values are
	// written. Coarse on purpose: the value answers "is this key still in
	// use", which does not need second resolution.
	UsageFlushInterval time.Duration
	Logger             *slog.Logger
}

// APIKeyService issues, lists, revokes and authenticates API keys.
//
// It sits alongside Service rather than inside it because the two answer
// different questions with different inputs — a password and a cookie versus a
// bearer token and a pepper — and only this one needs a secret from
// configuration. Both resolve to the same Identity, so nothing downstream can
// tell which credential a request arrived with unless it asks.
type APIKeyService struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	auth   *Service
	pepper []byte
	usage  *keyUsageTracker
	log    *slog.Logger
}

// MinPepperLength mirrors the config validation floor, so a service built
// directly in a test cannot be weaker than a deployed one.
const MinPepperLength = 32

func NewAPIKeyService(pool *pgxpool.Pool, authSvc *Service, cfg APIKeyConfig) (*APIKeyService, error) {
	if len(cfg.Pepper) < MinPepperLength {
		return nil, fmt.Errorf("auth: api key pepper must be at least %d bytes, got %d",
			MinPepperLength, len(cfg.Pepper))
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.UsageFlushInterval <= 0 {
		cfg.UsageFlushInterval = 30 * time.Second
	}

	return &APIKeyService{
		pool:   pool,
		q:      dbgen.New(pool),
		auth:   authSvc,
		pepper: cfg.Pepper,
		log:    cfg.Logger,
		usage:  newKeyUsageTracker(pool, cfg.UsageFlushInterval, cfg.Logger),
	}, nil
}

// Start launches the background writer for last_used_at.
func (s *APIKeyService) Start() { s.usage.start() }

// Close flushes buffered usage timestamps and stops the writer.
func (s *APIKeyService) Close(ctx context.Context) error { return s.usage.close(ctx) }

// FlushUsage writes buffered last_used_at values immediately. Called by Close,
// and by tests that would otherwise have to sleep.
func (s *APIKeyService) FlushUsage(ctx context.Context) error { return s.usage.flush(ctx) }

// Create issues a key and returns the only copy of its token.
//
// The token is not recoverable afterwards by design: only the HMAC is stored,
// which is the same reasoning as never storing a password. A caller who loses
// it revokes the key and issues another.
func (s *APIKeyService) Create(ctx context.Context, actor *Identity, in CreateAPIKeyInput) (*CreatedAPIKey, error) {
	if err := requireSessionActor(actor, "creating an API key"); err != nil {
		return nil, err
	}
	if !actor.Can(PermAPIKeysWrite) {
		return nil, fmt.Errorf("%w: creating API keys requires %s", domain.ErrForbidden, PermAPIKeysWrite)
	}

	name := strings.TrimSpace(in.Name)
	var errs domain.ValidationErrors
	switch {
	case name == "":
		errs = append(errs, domain.FieldError{
			Field: "name", Code: "required",
			Message: "a name is required, so the key can be recognised later",
		})
	case len(name) > 80:
		errs = append(errs, domain.FieldError{
			Field: "name", Code: "too_long", Message: "name must be at most 80 characters",
		})
	}

	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		errs = append(errs, domain.FieldError{
			Field: "expires_at", Code: "in_past", Message: "expiry must be in the future",
		})
	}

	scopes, scopeErrs, err := s.resolveScopes(ctx, actor, in.Scopes)
	if err != nil {
		return nil, err
	}
	errs = append(errs, scopeErrs...)

	if len(errs) > 0 {
		return nil, errs
	}

	// The key is scoped to the workspace its creator was acting in. Phase 2
	// widens this to a choice; leaving workspace_id NULL would mean "every
	// workspace in the organization", which is not something to grant by
	// default.
	workspaceID := actor.WorkspaceID

	for attempt := range 3 {
		token, prefix, secret, err := newAPIKeyToken()
		if err != nil {
			return nil, err
		}

		row, err := s.q.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{
			ID:             uuid.Must(uuid.NewV7()),
			UserID:         actor.UserID,
			OrganizationID: actor.OrgID,
			WorkspaceID:    &workspaceID,
			Name:           name,
			Prefix:         prefix,
			KeyHash:        APIKeyHash(s.pepper, prefix, secret),
			Scopes:         scopes,
			ExpiresAt:      in.ExpiresAt,
		})
		if err != nil {
			// 40 bits of public id makes this vanishingly rare, but the unique
			// index is what guarantees one prefix resolves to one key, so a
			// collision has to be retried rather than surfaced.
			if isUniqueViolation(err) && attempt < 2 {
				continue
			}
			return nil, fmt.Errorf("create api key: %w", err)
		}

		return &CreatedAPIKey{APIKeyInfo: keyInfo(row), Key: token}, nil
	}
	return nil, errors.New("auth: could not allocate an unused api key prefix")
}

// List returns the actor's own keys.
//
// Own, not the workspace's: a key is a personal credential acting as its
// owner, and showing one user another's credentials serves no purpose that
// listing memberships does not serve better.
func (s *APIKeyService) List(ctx context.Context, actor *Identity) ([]APIKeyInfo, error) {
	if err := requireSessionActor(actor, "listing API keys"); err != nil {
		return nil, err
	}
	if !actor.Can(PermAPIKeysRead) {
		return nil, fmt.Errorf("%w: listing API keys requires %s", domain.ErrForbidden, PermAPIKeysRead)
	}

	rows, err := s.q.ListAPIKeysForUser(ctx, dbgen.ListAPIKeysForUserParams{
		UserID:         actor.UserID,
		OrganizationID: actor.OrgID,
	})
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}

	out := make([]APIKeyInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, APIKeyInfo{
			ID: r.ID, Name: r.Name, Prefix: r.Prefix, Scopes: r.Scopes,
			LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt,
			RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// Revoke disables a key immediately.
//
// Immediately in the literal sense: nothing about a key is cached, so the next
// request presenting it fails. That is the reason revocation is checked in the
// verification query rather than kept in a cache alongside the hash.
func (s *APIKeyService) Revoke(ctx context.Context, actor *Identity, id uuid.UUID) error {
	if err := requireSessionActor(actor, "revoking an API key"); err != nil {
		return err
	}
	if !actor.Can(PermAPIKeysWrite) {
		return fmt.Errorf("%w: revoking API keys requires %s", domain.ErrForbidden, PermAPIKeysWrite)
	}

	n, err := s.q.RevokeAPIKey(ctx, dbgen.RevokeAPIKeyParams{ID: id, UserID: actor.UserID})
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if n == 0 {
		// Also the answer for someone else's key: "not found" rather than
		// "forbidden", so ids cannot be probed for existence.
		return domain.ErrNotFound
	}
	return nil
}

// Authenticate resolves a bearer token to an identity.
//
// The identity's permissions are the intersection of the owner's current role
// and the key's scopes, recomputed on every request. So demoting a user
// weakens their keys at once, and a scope the role no longer grants stops
// working without the key having to be reissued.
func (s *APIKeyService) Authenticate(ctx context.Context, token string) (*Identity, error) {
	prefix, secret, err := ParseAPIKey(token)
	if err != nil {
		return nil, err
	}

	row, err := s.q.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAPIKeyInvalid
		}
		return nil, fmt.Errorf("look up api key: %w", err)
	}

	// Constant-time, so the comparison cannot be used to recover a stored hash
	// byte by byte.
	if !hmac.Equal(row.KeyHash, APIKeyHash(s.pepper, prefix, secret)) {
		return nil, ErrAPIKeyInvalid
	}
	now := time.Now()
	if row.RevokedAt != nil ||
		(row.ExpiresAt != nil && !row.ExpiresAt.After(now)) ||
		row.Status != "active" {
		return nil, ErrAPIKeyInvalid
	}

	var wsID, orgID uuid.UUID
	if row.WorkspaceID != nil {
		wsID, orgID = *row.WorkspaceID, row.OrganizationID
	} else {
		// A NULL workspace means "any workspace in the organization", which
		// Phase 1 never issues but the column permits. Resolve it the same way
		// a login does rather than failing.
		ws, err := s.q.GetDefaultWorkspaceForUser(ctx, row.UserID)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace for api key: %w", err)
		}
		wsID, orgID = ws.ID, ws.OrganizationID
	}

	identity, err := s.auth.identityFor(ctx, row.UserID, row.Email, row.UserName, wsID, orgID)
	if err != nil {
		return nil, err
	}
	identity.restrictTo(row.Scopes)
	keyID := row.ID
	identity.APIKeyID = &keyID

	// Recorded, not written: the request must not wait on a database write to
	// a column nothing reads on the hot path.
	s.usage.touch(keyID, now)

	return identity, nil
}

// resolveScopes validates requested scopes against the permission vocabulary
// and against what the actor may delegate.
//
// Two rules, both load-bearing. A scope must be a real permission, or a typo
// would produce a key that silently authorizes nothing. And a scope must be
// one the actor holds, or minting a key would be a way to grant yourself
// permissions your role does not include.
func (s *APIKeyService) resolveScopes(ctx context.Context, actor *Identity, requested []string) ([]string, domain.ValidationErrors, error) {
	if len(requested) == 0 {
		return nil, domain.ValidationErrors{{
			Field: "scopes", Code: "required",
			Message: "at least one scope is required; a key with no scopes can do nothing",
		}}, nil
	}

	known, err := s.q.ListPermissionSlugs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load permission vocabulary: %w", err)
	}
	valid := make(map[string]struct{}, len(known))
	for _, slug := range known {
		valid[slug] = struct{}{}
	}

	var errs domain.ValidationErrors
	seen := make(map[string]struct{}, len(requested))
	scopes := make([]string, 0, len(requested))

	for _, raw := range requested {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}

		_, defined := valid[scope]
		switch {
		case !defined:
			errs = append(errs, domain.FieldError{
				Field: "scopes", Code: "unknown",
				Message: fmt.Sprintf("%q is not a permission this instance defines", scope),
			})
		case isNonDelegable(scope):
			errs = append(errs, domain.FieldError{
				Field: "scopes", Code: "not_delegable",
				Message: fmt.Sprintf("%q cannot be granted to an API key; it requires a signed-in session", scope),
			})
		case !actor.Can(scope):
			// A field error rather than a bare 403, so a form can mark the
			// offending scope. The message names it, which is safe: the caller
			// is authenticated and is being told about their own role.
			errs = append(errs, domain.FieldError{
				Field: "scopes", Code: "not_held",
				Message: fmt.Sprintf("you do not hold %q, so a key cannot be granted it", scope),
			})
		default:
			scopes = append(scopes, scope)
		}
	}

	if len(errs) > 0 {
		return nil, errs, nil
	}
	if len(scopes) == 0 {
		return nil, domain.ValidationErrors{{
			Field: "scopes", Code: "required",
			Message: "at least one scope is required; a key with no scopes can do nothing",
		}}, nil
	}
	return scopes, nil, nil
}

func isNonDelegable(scope string) bool {
	_, ok := NonDelegableScopes[scope]
	return ok
}

// requireSessionActor refuses an operation attempted with an API key.
//
// Defence in depth: apikeys.* is already non-delegable, so no key holds the
// permission these operations check. This makes the rule explicit at the point
// it matters, and gives the caller a message that explains itself rather than
// "missing permission apikeys.write" on a credential that can never have it.
func requireSessionActor(actor *Identity, action string) error {
	if actor == nil {
		return domain.ErrUnauthorized
	}
	if actor.IsAPIKey() {
		return fmt.Errorf("%w: %s requires a signed-in session, not an API key",
			domain.ErrForbidden, action)
	}
	return nil
}

func keyInfo(row dbgen.ApiKey) APIKeyInfo {
	return APIKeyInfo{
		ID: row.ID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
		LastUsedAt: row.LastUsedAt, ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt, CreatedAt: row.CreatedAt,
	}
}

// maxPendingKeyTouches bounds the tracker's memory. Reaching it would need more
// distinct keys in one flush window than any real deployment has; the cap is
// there so a pathological case degrades by losing a timestamp rather than by
// growing without limit.
const maxPendingKeyTouches = 8192

// keyUsageTracker coalesces last_used_at writes.
//
// A map rather than a queue, because the useful value is "when was this key
// last used" and repeated uses of the same key inside a flush window collapse
// to one row update. A key used a thousand times a second costs one write per
// interval.
type keyUsageTracker struct {
	pool     *pgxpool.Pool
	q        *dbgen.Queries
	interval time.Duration
	log      *slog.Logger

	mu       sync.Mutex
	pending  map[uuid.UUID]time.Time
	dropped  int64
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

func newKeyUsageTracker(pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) *keyUsageTracker {
	return &keyUsageTracker{
		pool:     pool,
		q:        dbgen.New(pool),
		interval: interval,
		log:      log,
		pending:  make(map[uuid.UUID]time.Time),
		done:     make(chan struct{}),
	}
}

func (t *keyUsageTracker) touch(id uuid.UUID, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if prev, ok := t.pending[id]; ok {
		if at.After(prev) {
			t.pending[id] = at
		}
		return
	}
	if len(t.pending) >= maxPendingKeyTouches {
		t.dropped++
		return
	}
	t.pending[id] = at
}

func (t *keyUsageTracker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	go func() {
		defer close(t.done)
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := t.flush(ctx); err != nil {
					t.log.Warn("could not record api key usage", slog.Any("error", err))
				}
			}
		}
	}()
}

func (t *keyUsageTracker) close(ctx context.Context) error {
	t.stopOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
			<-t.done
		}
	})
	// Flushed after the loop has stopped, so the final write is not racing a
	// tick. Uses the caller's context, which is the shutdown deadline.
	return t.flush(ctx)
}

func (t *keyUsageTracker) flush(ctx context.Context) error {
	t.mu.Lock()
	if len(t.pending) == 0 {
		dropped := t.dropped
		t.dropped = 0
		t.mu.Unlock()
		if dropped > 0 {
			t.log.Warn("api key usage timestamps dropped", slog.Int64("count", dropped))
		}
		return nil
	}
	ids := make([]uuid.UUID, 0, len(t.pending))
	times := make([]time.Time, 0, len(t.pending))
	for id, at := range t.pending {
		ids = append(ids, id)
		times = append(times, at)
	}
	// Cleared before the write, not after. A failed flush loses a timestamp
	// nothing depends on; holding entries for a retry would let a database
	// outage grow the map instead.
	t.pending = make(map[uuid.UUID]time.Time)
	dropped := t.dropped
	t.dropped = 0
	t.mu.Unlock()

	if dropped > 0 {
		t.log.Warn("api key usage timestamps dropped", slog.Int64("count", dropped))
	}

	if err := t.q.TouchAPIKeys(ctx, dbgen.TouchAPIKeysParams{Ids: ids, UsedAt: times}); err != nil {
		return fmt.Errorf("touch api keys: %w", err)
	}
	return nil
}
