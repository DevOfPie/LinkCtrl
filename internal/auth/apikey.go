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
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
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
// **D18 now says that too.** Until 2026-08-05 the decision named only the
// escalating and disclosing limbs and closed with "everything else is
// delegable" — which, read literally by whoever adds the next irreversible
// permission, makes org.delete delegable. This comment was right and the
// decision was not, for eight months of milestones. F12 corrected the text
// rather than the map, and the near miss is worth leaving on the record here:
// the next milestone to add an irreversible permission is the one that would
// have applied the two limbs, found neither matched, and shipped it delegable.
//
// This map is the only thing that makes audit.read session-only. There is no
// second check in the handler or the service — the endpoint authorizes on the
// permission like every other endpoint — so if machine export ever outweighs
// the disclosure, deleting this one line is the whole change. See decisions.md.
//
// destinations.decide is the escalating limb again, and more directly than key
// management is. Allowing a disputed destination deletes a row from the
// instance-wide low-confidence blocklist, after which every destination under
// that host becomes creatable — by the key that removed it, among others. A key
// that can decide what it is allowed to point at has widened its own reach by an
// action it took itself (M31, applying D18).
//
// **destinations.review is deliberately no longer here** (M45, D98). It used to
// be, because one permission guarded both reading the queue and deciding what is
// in it, and the deciding half is what the paragraph above convicts. D98 split
// them, and the split is how "API access is read-only for disputes; a change
// requires a person" is built: a key may list and inspect disputes, and is
// refused by this map when it tries to act on one. That refusal comes from the
// map rather than from a check on what kind of credential is calling — the
// inherited Permissions rule says anything branching on credential type outside
// this map and D43 is a defect, and F104 already convicts seven places for it,
// so adding an eighth deliberately would have been the wrong direction. Reading
// the queue matches neither limb of D18: it discloses who filed a dispute and a
// defanged host, never an address or a network prefix, and it escalates nothing.
//
// instance.admin is the second limb in its hardest form (M45, D98). Holding it
// confers destinations.decide on a person, so a key holding it would be a key
// that widens its own reach by manufacturing somebody else's — the shape D9
// keeps apikeys.* out of the map for, one step further removed. It is also the
// only permission in this product whose whole content is granting another one,
// which is exactly the thing a credential must not be able to do unattended.
//
// audit.read.instance is the *disclosing* limb, for the reason audit.read is:
// the instance audit surface is the same table, carrying the same ip_prefix tied
// to the same named actors, differing only in that its rows belong to no tenant.
// A permission that leaks what its sibling is listed here to protect would make
// the sibling's entry decorative.
//
// webhooks.write is the *durability* of a reach, which is the shape none of the
// entries above quite has (M42, applying D18's second limb). A webhook is a
// standing instruction to send every link change in a workspace to an address
// its creator chose, and it keeps sending after the credential that created it
// is revoked: revoking the key does not revoke the channel. That is a reach the
// key retains once it is gone, which is what makes it escalation rather than
// ordinary use of a permission the holder already has.
//
// webhooks.read is deliberately **not** here. Reading the list discloses where a
// workspace's events go and what the recent deliveries did, which is exactly
// what an integrator's tooling needs and escalates nothing. The pair therefore
// splits the way apikeys.* does not, and the split is the point: a key can watch
// its own integration, and a human has to create one.
//
// automation.write is the durability limb again, and one turn further round than
// webhooks.write (M43, applying D18). A webhook is a standing instruction to
// *report*; an automation rule is a standing instruction to *act* — it archives
// links on the scheduler, unattended, and it can make the server emit an event
// on top of that. Both outlive the credential that created them, so revoking the
// key does not revoke the instruction, and that is what makes it escalation
// rather than ordinary use of a permission the holder already has. An editor can
// archive a link today; nobody should be able to leave behind a token that keeps
// archiving links after it has been revoked.
//
// automation.read is deliberately **not** here, for the reason webhooks.read is
// not: reading the list says what a workspace has told the scheduler to do and
// when each rule last fired, which is exactly what an integrator's tooling needs
// and escalates nothing.
var NonDelegableScopes = map[string]struct{}{
	PermAPIKeysRead:        {},
	PermAPIKeysWrite:       {},
	"org.delete":           {},
	"audit.read":           {},
	"webhooks.write":       {},
	"automation.write":     {},
	PermInstanceAdmin:      {},
	PermDestinationsDecide: {},
	PermAuditReadInstance:  {},
}

// KeyIssuableRoles are the roles an API key may put somebody into (D43).
// Absolute, not relative to whoever created the key.
//
// The second of the two mechanisms that may branch on credential type, and it
// sits beside the first so that a reader meets both at once. NonDelegableScopes
// above governs what a key may **hold**. This governs what a key may **make**
// with one it legitimately holds, and members.write is the permission that needs
// both: a key holding it does not itself gain anything, but the interactive
// principal it produces is not a credential — nothing revokes that principal
// when the key is revoked, and requireSessionActor cannot tell it from an
// account somebody registered.
//
// Named rather than ranked, deliberately. A relative ceiling — one rank below
// the issuer — is the fix this looks like and it closes nothing: admin holds
// every permission except org.delete (00700_seed.sql), so a key an owner created
// could still produce an admin holding apikeys.write, audit.read and
// members.write. The boundary is between admin and editor because of what those
// two roles *hold*, which is not a property of where a rank sorts, so a role
// added later is refused here until somebody decides otherwise rather than
// admitted by arithmetic.
//
// **Every way a key can put somebody at a role passes through this**, which is
// what D43 originally missed: it bounded the invitation and left role assignment
// on an existing membership — team.ChangeRole and team.Grant — reaching admin
// with the same key and the same permission. Reaching admin by promotion rather
// than by admission is one axis over, not a different defect.
var KeyIssuableRoles = map[string]struct{}{
	"editor": {},
	"viewer": {},
}

// APIKeyInfo is a key as its owner sees it. The secret is absent by
// construction: it is never stored, so it cannot be listed.
type APIKeyInfo struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Prefix string    `json:"prefix"`
	Scopes []string  `json:"scopes"`
	// OrgWide is the workspace choice made when the key was created: false for
	// a key bound to one workspace, true for one not pinned to any (a NULL
	// workspace_id). Reported rather than left implicit, because the two are
	// otherwise indistinguishable in a list and they are not the same credential.
	//
	// Not pinned is not *all at once*: there is no per-request workspace
	// selector, so a request made with such a key resolves exactly one workspace
	// the way a sign-in does, bounded to the organization the key was issued in
	// (D90). The qualifier is here because leaving it out cost two readers a
	// high-severity misfiling — F122, and this field is one of the sites F139
	// found still saying it short.
	OrgWide bool `json:"org_wide"`
	// OrganizationID is the key's **reach** (M54): the organization it is pinned
	// to, or null for a key that reaches every organization its owner holds an
	// organization-wide membership in.
	//
	// Reported rather than derived, because a null and a set value are two
	// different credentials and nothing else in this struct distinguishes them.
	// OrgWide above answers a different question one tier down — whether the key
	// is pinned to a *workspace* — and the two are not redundant: a
	// workspace-bound key is pinned to an organization by construction, so
	// OrgWide false implies this is set, while OrgWide true says nothing about
	// it either way.
	OrganizationID *uuid.UUID `json:"organization_id"`
	// RevokedOrganizations are the organizations an administrator has cut this
	// key out of, and it closes F178 — the seeing half of M54's reach
	// revocation, whose acting half shipped without it.
	//
	// A credential that has silently stopped resolving into one tenant read on
	// this page exactly as it did the day before, and the audit record that says
	// why is written in the administrator's organization, which the owner may
	// hold no audit.read in. The support question that produced the row is *why
	// does my key work in Acme and not in Beta*, and until this field there was
	// no answer anywhere the owner could reach.
	//
	// Always an array, never null, and empty for every pinned key: an
	// organization is a pinned key's whole reach, so cutting it is an outright
	// revoke and RevokedAt is where that shows.
	RevokedOrganizations []APIKeyOrgRevocation `json:"revoked_organizations"`
	LastUsedAt           *time.Time            `json:"last_used_at"`
	ExpiresAt            *time.Time            `json:"expires_at"`
	RevokedAt            *time.Time            `json:"revoked_at"`
	// RotatedAt, GraceExpiresAt and SuccessorID describe a key that has been
	// replaced. All three are set together, on the predecessor, and all three
	// are nil on a key that has not been rotated. GraceExpiresAt is the moment
	// it stops authenticating anything.
	RotatedAt      *time.Time `json:"rotated_at"`
	GraceExpiresAt *time.Time `json:"grace_expires_at"`
	SuccessorID    *uuid.UUID `json:"successor_id"`
	CreatedAt      time.Time  `json:"created_at"`
}

// APIKeyOrgRevocation is one organization cut out of an account-wide key's
// reach, as the key's owner sees it.
//
// The name is carried and not only the id, because the question this answers is
// asked in words — *why does my key work in Acme and not in Beta* — and a uuid
// answers it in none of them. It discloses nothing the owner did not already
// have: a bar can only exist on a key whose owner held an organization-wide
// membership there when it was written.
//
// Who did it is deliberately absent. api_key_org_revocations carries revoked_by,
// and naming an administrator of another organization to a credential's holder
// is a disclosure this field was not asked for; the audit record in that
// organization is where the actor is recorded, for the people who may read it.
type APIKeyOrgRevocation struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	RevokedAt        time.Time `json:"revoked_at"`
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
	// OrgWide asks for a key that is not pinned to the workspace its creator was
	// acting in. Each request still resolves exactly one, the way a sign-in
	// does, within the organization the key is issued in — see APIKeyInfo.OrgWide
	// and D90.
	//
	// Opt-in, and false is the behaviour every key had before M44. Being able to
	// act in any of an organization's workspaces is not something to grant
	// because somebody left a field blank, and the check behind it is not the
	// ordinary permission check — see MayCreateOrgWide.
	OrgWide bool
	// OrganizationID pins the key to one organization instead of leaving it
	// account-wide (M54). Nil is account-wide, and it is the default for an
	// unpinned key: a key is minted by an *account* and reaches the organizations
	// that account belongs to, the way a personal access token does.
	//
	// The only value accepted is the organization the caller is acting in.
	// Minting into another one would need authority there that nothing has
	// checked, and the caller can switch organization and mint again — which is
	// the same act with the authorization visible.
	//
	// Ignored, not refused, when OrgWide is false: a workspace-bound key is
	// pinned to the workspace's organization by construction, and the check
	// constraint added in 04200 refuses any other combination outright. Naming
	// the caller's own organization there is therefore a no-op rather than a
	// contradiction, and naming a different one is still a validation error.
	OrganizationID *uuid.UUID
}

// APIKeyConfig configures the key service.
type APIKeyConfig struct {
	// Pepper keys the HMAC. Required; a short one is refused rather than
	// silently accepted, because a weak pepper is invisible in behaviour.
	Pepper []byte
	// UsageFlushInterval is how often buffered last_used_at values are
	// written. Coarse on purpose: the value answers "is this key still in
	// use", which does not need second resolution.
	//
	// It is also the tolerance on that answer, and rotation depends on the
	// number: a predecessor that reads as idle may have been used up to this
	// long ago, which is why MinRotationGrace sits an order of magnitude above
	// it.
	UsageFlushInterval time.Duration
	// Auditor records rotations, and one administrator revoking somebody else's
	// key. Optional — a nil one means the operation still happens and is logged
	// as unrecorded, which is the same trade every other service makes with its
	// audit writer.
	Auditor APIKeyAuditor
	Logger  *slog.Logger
}

// APIKeyService issues, lists, revokes and authenticates API keys.
//
// It sits alongside Service rather than inside it because the two answer
// different questions with different inputs — a password and a cookie versus a
// bearer token and a pepper — and only this one needs a secret from
// configuration. Both resolve to the same Identity, so nothing downstream can
// tell which credential a request arrived with unless it asks.
type APIKeyService struct {
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	auth    *Service
	pepper  []byte
	usage   *keyUsageTracker
	auditor APIKeyAuditor
	log     *slog.Logger
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
		pool:    pool,
		q:       dbgen.New(pool),
		auth:    authSvc,
		pepper:  cfg.Pepper,
		auditor: cfg.Auditor,
		log:     cfg.Logger,
		usage:   newKeyUsageTracker(pool, cfg.UsageFlushInterval, cfg.Logger),
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

	// A key an organization other than the caller's own would be minted into is
	// refused before either choice below is made, so the message is about the
	// field somebody filled in rather than about a consequence of it.
	if in.OrganizationID != nil && *in.OrganizationID != actor.OrgID {
		errs = append(errs, domain.FieldError{
			Field: "organization_id", Code: "not_yours",
			Message: "a key can only be pinned to the organization you are acting in; " +
				"switch organization and create it there",
		})
	}

	// The workspace choice (M44). By default a key is scoped to the workspace
	// its creator was acting in, which is what every key issued before M44 got
	// and what the 00500 column comment always allowed for. Leaving workspace_id
	// NULL means "every workspace in the organization", so it is asked for
	// explicitly and granted only to somebody whose own reach is that wide.
	//
	// The reach choice (M54) sits on top of it and only exists for an unpinned
	// key. A workspace belongs to exactly one organization, so a workspace-bound
	// key is pinned to that organization by construction — asking for anything
	// else is refused by the check constraint rather than by convention.
	var workspaceID, organizationID *uuid.UUID
	if in.OrgWide {
		may, err := s.MayCreateOrgWide(ctx, actor)
		if err != nil {
			return nil, err
		}
		if !may {
			errs = append(errs, domain.FieldError{
				Field: "org_wide", Code: "not_permitted",
				Message: "an organization-wide key needs an organization-wide membership that grants " +
					PermAPIKeysWrite + "; a role held in one workspace can issue a key for that workspace",
			})
		}
		// Nil stays nil, and that is the model change: an unpinned key is
		// account-wide unless its creator asks for the snapshot. Pinning is the
		// opt-in because it is the choice that cannot be undone — a key pinned to
		// one tenant can never widen, and there is no way back from a key that
		// widened when its owner joined a second.
		organizationID = in.OrganizationID
	} else {
		ws := actor.WorkspaceID
		org := actor.OrgID
		workspaceID, organizationID = &ws, &org
	}

	if len(errs) > 0 {
		return nil, errs
	}

	for attempt := range 3 {
		token, prefix, secret, err := newAPIKeyToken()
		if err != nil {
			return nil, err
		}

		row, err := s.q.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{
			ID:             uuid.Must(uuid.NewV7()),
			UserID:         actor.UserID,
			OrganizationID: organizationID,
			WorkspaceID:    workspaceID,
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
			if pgerr.IsUniqueViolation(err) && attempt < 2 {
				continue
			}
			return nil, fmt.Errorf("create api key: %w", err)
		}

		// No bars, and here it is a fact rather than a shortcut: this id was
		// allocated a statement ago, and a bar names an id. Rotation is the case
		// that cannot say this — see keyInfo.
		return &CreatedAPIKey{APIKeyInfo: keyInfo(row, nil), Key: token}, nil
	}
	return nil, errors.New("auth: could not allocate an unused api key prefix")
}

// MayCreateOrgWide reports whether this actor may issue a key that reaches
// every workspace in the organization.
//
// The check is **not** `actor.Can(PermAPIKeysWrite)`, and the difference is the
// whole point. `Can` answers from the union of every membership matching the
// workspace being acted in (D31), so an actor holding `apikeys.write` through a
// membership scoped to one workspace answers yes to it — and issuing an
// organization-wide key on the strength of a workspace-scoped role is precisely
// the shape F27 had. D44's rule is that a write is authorized against the
// membership whose scope covers its target, and an organization-wide key's
// target is the organization: `In(nil)` is that question, and only an
// organization-wide membership reaches it.
//
// No new permission was minted for this, deliberately. A permission is held per
// *role*, and roles are granted per membership, so an `apikeys.org_scope` would
// have been held by a workspace-scoped admin exactly as `apikeys.write` already
// is — the new slug would have looked like a gate and enforced nothing the wrong
// check was already failing to enforce.
//
// Also gated on being a session, because Create is: a key cannot mint a key at
// all, so it certainly cannot mint a wider one.
func (s *APIKeyService) MayCreateOrgWide(ctx context.Context, actor *Identity) (bool, error) {
	if actor == nil || actor.IsAPIKey() || !actor.Can(PermAPIKeysWrite) {
		return false, nil
	}
	authority, err := LoadMembershipAuthority(ctx, s.q, actor.UserID, actor.OrgID, PermAPIKeysWrite)
	if err != nil {
		return false, err
	}
	return authority.In(nil).Granted, nil
}

// List returns the actor's own keys.
//
// Own, not the workspace's: a key is a personal credential acting as its
// owner, and showing one user another's credentials serves no purpose that
// listing memberships does not serve better.
//
// **Own, and not the organization's either** (M54, closing F75). The list used
// to be scoped by owner *and* organization while Revoke was scoped by owner
// alone, so a key issued elsewhere was invisible here and revocable there — a
// 204-versus-404 oracle over guessed ids. The fix is not a filter on the revoke:
// that would make a key unrevokable from the organization somebody is signed
// into, which F75's own severity called worse than the defect. It is that both
// statements now ask the one question a personal credential admits, and an
// account-wide key has no organization to be filtered by in the first place.
//
// The cost, stated because it is a real change in what this page shows: somebody
// who belongs to two organizations sees every key they own from either, rather
// than the current organization's. That is what a personal access token list
// looks like everywhere else, and the reach column is what tells them apart.
func (s *APIKeyService) List(ctx context.Context, actor *Identity) ([]APIKeyInfo, error) {
	if err := requireSessionActor(actor, "listing API keys"); err != nil {
		return nil, err
	}
	if !actor.Can(PermAPIKeysRead) {
		return nil, fmt.Errorf("%w: listing API keys requires %s", domain.ErrForbidden, PermAPIKeysRead)
	}

	rows, err := s.q.ListAPIKeysForUser(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}

	// The reach revocations for the keys just listed, in one round trip keyed on
	// their ids (F178). Second query rather than a join, because the relation is
	// one-to-many and joining would multiply every key row by its bars and make
	// the list's own shape depend on them. Skipped entirely when there are no
	// keys, which is the common case on a fresh account.
	bars := map[uuid.UUID][]APIKeyOrgRevocation{}
	if len(rows) > 0 {
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		// Asked for every listed key rather than only the account-wide ones.
		// Nothing writes a bar for a pinned key — revokeInOrganization revokes
		// those outright — and asking anyway costs nothing, while assuming it
		// here would make this list quietly wrong if that ever stopped holding.
		revocations, err := s.q.ListAPIKeyOrgRevocations(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("list api key reach revocations: %w", err)
		}
		bars = barsByKey(revocations)
	}

	out := make([]APIKeyInfo, 0, len(rows))
	for _, r := range rows {
		revoked := bars[r.ID]
		if revoked == nil {
			revoked = []APIKeyOrgRevocation{}
		}
		out = append(out, APIKeyInfo{
			ID: r.ID, Name: r.Name, Prefix: r.Prefix, Scopes: r.Scopes,
			OrgWide:              r.WorkspaceID == nil,
			OrganizationID:       r.OrganizationID,
			RevokedOrganizations: revoked,
			LastUsedAt:           r.LastUsedAt, ExpiresAt: r.ExpiresAt,
			RevokedAt: r.RevokedAt, RotatedAt: r.RotatedAt,
			GraceExpiresAt: r.GraceExpiresAt, SuccessorID: r.SuccessorID,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// Revoke disables a key immediately.
//
// Immediately in the literal sense: nothing about a key is cached, so the next
// request presenting it fails. That is the reason revocation is checked in the
// verification query rather than kept in a cache alongside the hash.
//
// Two revokes behind one id, tried in that order. Own key first, which is the
// ordinary path and needs no authority beyond apikeys.write. Somebody else's
// second, and only for an actor holding apikeys.write from an
// organization-wide membership — a key belongs to the organization it was
// issued into, so reaching one is an organization-wide act and a
// workspace-scoped admin does not reach it (D44). It exists because there was
// otherwise no answer at all to a key that had to be stopped and whose owner
// would not stop it.
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
	if n > 0 {
		return nil
	}
	return s.revokeInOrganization(ctx, actor, id)
}

// revokeInOrganization is the administrator's arm of Revoke: an id that is not
// the actor's own key.
//
// Every refusal here is ErrNotFound, and that is the same property the
// single-arm version had — an id must not be probeable for existence by
// somebody who may not act on it. An actor without organization-wide authority
// therefore cannot tell "somebody else's key" from "no such key", which is what
// they could tell before.
//
// Audited, unlike revoking your own key, and the asymmetry is the point. The
// vocabulary's own note says minting and revoking need no record because they
// require an interactive session and "the person is the record" — true while the
// only person who could revoke a key was its owner. This arm breaks that
// premise: the credential's owner was not present, may not know, and the
// question afterwards is who stopped it.
//
// **Two writes since M54, and which one happens is a property of the key rather
// than of the request.** A pinned key is revoked outright, exactly as before:
// the actor's organization is that key's entire reach, so cutting the reach and
// destroying the credential are the same act. An account-wide key belongs to an
// account that also acts in tenants this administrator has no authority over, so
// what they may cut is their own organization out of its reach — a row in
// api_key_org_revocations, a distinct audit action, and a key that keeps working
// everywhere else. Neither the endpoint nor the caller chooses between them;
// offering the choice would be offering an administrator the outright revoke
// they may not have.
func (s *APIKeyService) revokeInOrganization(
	ctx context.Context, actor *Identity, id uuid.UUID,
) error {
	authority, err := LoadMembershipAuthority(ctx, s.q, actor.UserID, actor.OrgID, PermAPIKeysWrite)
	if err != nil {
		return err
	}
	if !authority.In(nil).Granted {
		return domain.ErrNotFound
	}

	reach, err := s.q.GetAPIKeyReach(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("load api key reach: %w", err)
	}
	if reach.OrganizationID != nil {
		if *reach.OrganizationID != actor.OrgID {
			return domain.ErrNotFound
		}
		return s.revokePinnedKey(ctx, actor, id)
	}
	return s.revokeReach(ctx, actor, reach)
}

// revokePinnedKey destroys a key issued into the actor's own organization.
func (s *APIKeyService) revokePinnedKey(ctx context.Context, actor *Identity, id uuid.UUID) error {
	org := actor.OrgID
	row, err := s.q.RevokeAPIKeyInOrganization(ctx, dbgen.RevokeAPIKeyInOrganizationParams{
		ID: id, OrganizationID: &org,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("revoke api key in organization: %w", err)
	}

	if s.auditor != nil {
		// The write already happened. A log that could not be written is worth
		// saying loudly and is not worth un-revoking a credential somebody
		// decided to stop.
		if err := s.auditor.RecordAPIKeyRevocation(ctx, actor, APIKeyRevocation{
			KeyID: id, Prefix: row.Prefix, OwnerID: row.UserID,
		}); err != nil {
			s.log.Error("record api key revocation", "err", err, "key_id", id)
		}
	}
	return nil
}

// revokeReach cuts the actor's organization out of an account-wide key (M54).
//
// The owner has to hold an organization-wide membership here, and that is the
// authorization rather than a courtesy check: without one the key does not reach
// this organization at all, so there is nothing to cut and the answer is
// ErrNotFound — the same answer an unknown id gets, so the endpoint still
// discloses nothing about ids the actor may not act on.
//
// This is the one place a key's authority is evaluated in an organization other
// than the one the request is acting in, which is why it goes through
// LoadMemberships rather than through the actor's own identity: Can and OrgID
// both answer for where *this* request landed, and the question here is about
// somebody else's reach into a tenant they are not currently in.
func (s *APIKeyService) revokeReach(
	ctx context.Context, actor *Identity, reach dbgen.GetAPIKeyReachRow,
) error {
	owner, err := LoadMemberships(ctx, s.q, reach.UserID, actor.OrgID)
	if err != nil {
		return err
	}
	if !owner.Reaches(nil) {
		return domain.ErrNotFound
	}

	if _, err := s.q.RevokeAPIKeyReachInOrganization(ctx, dbgen.RevokeAPIKeyReachInOrganizationParams{
		ApiKeyID: reach.ID, OrganizationID: actor.OrgID, RevokedBy: &actor.UserID,
	}); err != nil {
		return fmt.Errorf("revoke api key reach: %w", err)
	}

	if s.auditor != nil {
		if err := s.auditor.RecordAPIKeyReachRevocation(ctx, actor, APIKeyRevocation{
			KeyID: reach.ID, Prefix: reach.Prefix, OwnerID: reach.UserID,
		}); err != nil {
			s.log.Error("record api key reach revocation", "err", err, "key_id", reach.ID)
		}
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
	// The grace check is here, in the request path, and not in the housekeeping
	// job that later writes revoked_at. A rotated predecessor stops verifying at
	// the instant its window closes whether or not any background job is running,
	// on every replica, with no cache to invalidate — which is the same property
	// revocation already had and the reason both are read from the row rather
	// than kept anywhere else.
	if row.RevokedAt != nil ||
		(row.ExpiresAt != nil && !row.ExpiresAt.After(now)) ||
		(row.GraceExpiresAt != nil && !row.GraceExpiresAt.After(now)) ||
		row.Status != "active" {
		return nil, ErrAPIKeyInvalid
	}
	// The membership the key acts through, checked on the way in rather than
	// discovered to be missing afterwards.
	//
	// A key is its owner acting as themselves with a narrower set of scopes, so
	// an owner with no membership covering the key's scope leaves it acting as
	// nobody. Without this the credential still authenticated: it resolved to an
	// identity carrying the organization and a workspace, with an empty
	// permission set, which is a state team_test.go's "an orphaned identity
	// carries neither org nor workspace" already forbids on the session path. An
	// empty permission set is not the same as no credential, and the difference
	// was reachable — CreateOrganization opens its own door for an actor with no
	// memberships at all (D36), and rotation renewed the key indefinitely with
	// the row's stored scopes, so a removed member kept a self-renewing chain
	// nobody could stop.
	//
	// The refusal is deliberately ErrAPIKeyInvalid and not a permission error.
	// The comment on resolveWorkspace's organization-wide branch has said since
	// M44 that "answering 'invalid' is honest for both" about exactly this state;
	// it was simply applied on the one branch Create can never produce.
	if !row.OwnerIsMember {
		return nil, ErrAPIKeyInvalid
	}

	var wsID, orgID uuid.UUID
	// Empty for every branch but the account-wide one, which is the only branch
	// where a bar can exist: a pinned key's organization is its whole reach, so
	// cutting the reach and cutting the key are the same act and nothing writes
	// a revocation row for one.
	var barredOrgs []uuid.UUID
	switch {
	case row.WorkspaceID != nil:
		// Pinned to a workspace, and therefore to that workspace's organization:
		// the 04200 check constraint is what makes this dereference safe rather
		// than a hope about which columns are written together.
		wsID, orgID = *row.WorkspaceID, *row.OrganizationID
	default:
		// A NULL workspace means "every workspace in the organization the key
		// belongs to" — a choice its creator made (M44) and one the column has
		// permitted since 00500. Resolved the same way a login is, so the key
		// follows the person it acts as, including their pinned default.
		//
		// No session id, because there is no session. That kills rung 1 of the
		// precedence, which is a property of one browser, and it does nothing
		// more: it does not insulate the key from the switcher. SwitchWorkspace's
		// other write is users.last_workspace_id — rung 3, and the rung that
		// answers for every owner who has not pinned a default — so a switch made
		// in a browser does move an organization-wide key's requests. That is
		// D90's *resolved the same way a login is* working as written rather than
		// a leak: what the key follows is the person, and where the person is
		// acting is part of that.
		//
		// **Bounded to an organization**, and that bound is load-bearing rather
		// than tidy. The precedence filters on membership alone, and a person's
		// pinned default is a property of the person: an owner who belongs to two
		// organizations and last used the other one would otherwise have their
		// unpinned key resolve into a tenant it may not reach, carrying its scopes
		// with it. Unreachable before M44, because nothing had ever written a NULL
		// workspace here.
		//
		// **Which organization is the M54 change, and it is the only one.** For a
		// pinned key it is the key's own column, exactly as M44 wrote it. For an
		// account-wide key there is no column, so the tenant is resolved first —
		// by the person's own precedence, among the organizations they hold an
		// organization-wide membership in and the key has not been barred from —
		// and *that* answer is what M44's parameter is fed. The parameter survives
		// the model change unaltered; what changed is where its value comes from.
		//
		// The resolution also answers what the key may *read* about, which is
		// F183: it consults api_key_org_revocations to decide where the request
		// lands, so the whole barred set comes back with the one organization it
		// chose and Service.Workspaces bounds the listing without a query of its
		// own.
		keyOrg := row.OrganizationID
		if keyOrg == nil {
			resolved, err := s.q.ResolveOrganizationForAPIKey(ctx, dbgen.ResolveOrganizationForAPIKeyParams{
				UserID: row.UserID, ApiKeyID: row.ID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// Reachable in a way the ErrNoWorkspace branch below is not: the
					// owner may hold memberships and still reach nothing with *this*
					// credential, because every organization-wide one of theirs sits
					// in a tenant an administrator cut this key out of. Same answer,
					// because the credential resolves to no tenancy either way.
					return nil, ErrAPIKeyInvalid
				}
				return nil, fmt.Errorf("resolve organization for api key: %w", err)
			}
			keyOrg = &resolved.ID
			barredOrgs = resolved.BarredOrganizationIds
		}
		ws, err := s.auth.resolveWorkspace(ctx, row.UserID, nil, keyOrg)
		if errors.Is(err, ErrNoWorkspace) {
			// The one caller that does *not* get D36's empty identity. Belonging
			// to nothing is a state a person is walked through — sign in, be
			// offered an organization — and a key has nobody to walk. It is also
			// the rarer half of the state: deleting an organization cascades its
			// keys away, so this is a key whose owner lost their membership
			// while the key survived. Answering "invalid" is honest for both:
			// the credential resolves to no tenancy and can do nothing.
			return nil, ErrAPIKeyInvalid
		}
		if err != nil {
			return nil, err
		}
		wsID, orgID = ws.ID, ws.OrganizationID
	}

	identity, err := s.auth.identityFor(ctx, row.UserID, row.Email, row.UserName, wsID, orgID)
	if err != nil {
		return nil, err
	}
	// The intersection, and the sentence M54 is about: the identity carries the
	// owner's authority **where this request landed** — identityFor asked
	// GetUserPermissions for the resolved workspace, which is the union of the
	// memberships covering it (D31) and therefore the role that applies in the
	// resolved organization — and restrictTo narrows that to the key's stored
	// scopes. Both halves are read on every request, neither is stored on the
	// key, and the consequence is deliberate: the same account-wide key is more
	// powerful in one organization than in another, and a demotion narrows it
	// without the key being touched.
	identity.restrictTo(row.Scopes)
	keyID := row.ID
	identity.APIKeyID = &keyID
	identity.APIKeyOrgID = row.OrganizationID
	identity.APIKeyBarredOrgIDs = barredOrgs

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
// For the three callers in this file it is defence in depth: apikeys.* is
// non-delegable, so no key holds the permission they go on to check, and this
// makes the rule explicit at the point it matters while giving the caller a
// message that explains itself rather than "missing permission apikeys.write"
// on a credential that can never have it.
//
// For the two in workspace.go it is the **only** enforcement, and the
// difference is worth stating rather than leaving to be discovered.
// SwitchWorkspace and SetDefaultWorkspace check no permission at all, because
// neither is an authority over anything an organization owns — they move the
// caller's own session and write an account preference. There is no permission
// to be non-delegable, so nothing stands behind this call: delete it and a
// leaked key repoints where its owner's next sign-in lands (F104).
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

// barsByKey groups reach revocation rows by the key each one names.
//
// Shared by the two readers of those rows — the owner's key list and the
// rotation response — so that one cannot report a bar the other would have
// spelled differently. Ordering inside a key is the query's, which is oldest
// first.
func barsByKey(rows []dbgen.ListAPIKeyOrgRevocationsRow) map[uuid.UUID][]APIKeyOrgRevocation {
	bars := map[uuid.UUID][]APIKeyOrgRevocation{}
	for _, x := range rows {
		bars[x.ApiKeyID] = append(bars[x.ApiKeyID], APIKeyOrgRevocation{
			OrganizationID:   x.OrganizationID,
			OrganizationName: x.OrganizationName,
			RevokedAt:        x.RevokedAt,
		})
	}
	return bars
}

// keyInfo builds the record for a key that has just come into existence.
//
// **The bars are a parameter rather than an empty slice**, and the difference is
// a defect this function used to carry. It read "nothing can have barred an id
// that did not exist when the bar would have been written", which is true of
// creation and false of rotation the moment [CarryAPIKeyReachRevocations]
// exists: a successor is minted with its predecessor's bars copied onto it,
// inside the same transaction, before anything here runs. Each caller therefore
// states what it has rather than inheriting an answer that is right for one of
// them.
//
// nil is normalized to an empty slice, because `revoked_organizations` is a
// documented array on every key shape and `null` is not one.
func keyInfo(row dbgen.ApiKey, revoked []APIKeyOrgRevocation) APIKeyInfo {
	if revoked == nil {
		revoked = []APIKeyOrgRevocation{}
	}
	return APIKeyInfo{
		ID: row.ID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
		OrgWide:              row.WorkspaceID == nil,
		OrganizationID:       row.OrganizationID,
		RevokedOrganizations: revoked,
		LastUsedAt:           row.LastUsedAt, ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt, RotatedAt: row.RotatedAt,
		GraceExpiresAt: row.GraceExpiresAt, SuccessorID: row.SuccessorID,
		CreatedAt: row.CreatedAt,
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
