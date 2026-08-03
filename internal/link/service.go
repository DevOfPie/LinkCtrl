package link

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/feed"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Permissions this service enforces. Named constants so a typo is a compile
// error rather than a silently-always-false check.
const (
	PermRead      = "links.read"
	PermCreate    = "links.create"
	PermUpdate    = "links.update"
	PermDelete    = "links.delete"
	PermTagsRead  = "tags.read"
	PermTagsWrite = "tags.write"
	// PermDomainsWrite guards settings that apply to the hostname rather than to
	// a workspace. One host serves every workspace on the instance, so this is a
	// wider grant than links.update despite touching fewer rows.
	PermDomainsWrite = "domains.write"
)

// TrashRetentionDays is how long a soft-deleted link stays restorable.
const TrashRetentionDays = 30

// Invalidator clears cached snapshots when a link changes. The redirect cache
// implements it in M7; a nil Invalidator is valid and means "no cache".
//
// InvalidateDomain is the M32.5 addition and it is on the same interface rather
// than a second one, because a caller that holds a cache it can only half
// invalidate is a caller that will eventually forget which half. It clears
// every alias on the domain, which is what a domain-level setting change
// requires: the cached snapshot carries the domain's bot policy so the redirect
// path needs no second lookup, and the bill for that arrives here.
type Invalidator interface {
	InvalidateAlias(ctx context.Context, domainID uuid.UUID, alias string)
	InvalidateDomain(ctx context.Context, domainID uuid.UUID)
}

type Service struct {
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	policy  DestinationPolicy
	aliases alias.Policy
	baseURL string
	cache   Invalidator
	// splitHosts records whether short links have a hostname of their own. The
	// root-redirect setting is meaningless without one.
	splitHosts bool
	rootCache  RootInvalidator
	// audit records administrative changes. Nil is valid and means nothing is
	// recorded — the CLI and most tests run that way.
	audit audit.Recorder
	// feed is the opt-in third-party reputation check (M32). Nil on every
	// instance that has not named a feed, which is the default, and nil is what
	// "no destination leaves the box" is made of: there is no client to call
	// and no flag whose false branch could be missed.
	feed FeedChecker
	// feedMetrics counts feed checks. Nil counts nothing.
	feedMetrics FeedObserver
	// hasher turns a link password into an argon2id hash (M35). Nil refuses to
	// set one rather than storing it in the clear, which is the direction a
	// missing dependency has to fail in.
	hasher *auth.Hasher
	// gates reads the durable click budget and mints workspace signing secrets
	// (M35). Nil leaves the budget unreported and signing unavailable; the CLI
	// and several tests run that way.
	gates GateReader
	// dns answers the custom-domain challenge (M40). Nil refuses to verify
	// anything rather than pretending a hostname passed, which is the direction
	// a missing dependency has to fail in when the thing it decides is whether
	// an alias namespace is served on somebody's public hostname.
	dns TXTLookup
	// hosts broadcasts a change to the verified set, so no replica goes on
	// serving a domain this one has just unverified. Nil means this process is
	// alone, which is the CLI and most tests.
	hosts HostInvalidator
	// domainNotify warns a workspace before its hostname stops being served.
	// Nil warns nobody and the grace window still runs.
	domainNotify DomainNotifier
	// verifyGraceWindow is D70's bounded patience. Zero means the default; it is
	// never "no window", because an unset knob must not turn one DNS hiccup into
	// an outage.
	verifyGraceWindow time.Duration
	// linkScheme is http or https, taken from BaseURL, for the short URLs built
	// on a custom hostname. A custom domain has a hostname and no scheme of its
	// own, and guessing https would print a URL that does not work on a
	// development instance.
	linkScheme string
	// hostnames caches domain id to hostname for building short URLs off the
	// default domain. Bounded by the number of domains and dropped on rename.
	hostnames sync.Map
	log       *slog.Logger
}

// GateReader is what the management surfaces need from internal/gate: the exact
// budget a gated link has spent, and the workspace key a signed URL is made
// with.
//
// An interface rather than the concrete service, so internal/link does not
// import a package that imports it back through the redirect path.
type GateReader interface {
	Budget(ctx context.Context, linkID uuid.UUID) (int64, *time.Time, error)
	EnsureSecret(ctx context.Context, workspaceID uuid.UUID) ([]byte, error)
}

// RootInvalidator drops the cached root redirect when it changes. Nil is valid
// and means the redirect tree is not running in this process.
type RootInvalidator interface {
	InvalidateRoot()
}

type Config struct {
	Policy DestinationPolicy
	// Aliases carries the operator's reserved-word additions and the profanity
	// switch. The zero value is the safe default: built-in list, filter on.
	Aliases alias.Policy
	BaseURL string
	Cache   Invalidator
	// SplitHosts mirrors config.SplitHosts. The root-redirect setting is refused
	// when false, because there the root is the dashboard.
	SplitHosts bool
	RootCache  RootInvalidator
	// Audit records administrative changes. Nil records nothing.
	Audit audit.Recorder
	// Feed is the opt-in third-party reputation check. Nil is the default and
	// the only state in which this program sends nothing anywhere; see
	// Service.askFeed and docs/build-notes/decisions.md, D40.
	Feed FeedChecker
	// FeedMetrics counts feed checks, including the failures that fail open.
	// Nil counts nothing.
	FeedMetrics FeedObserver
	// Hasher hashes link passwords (M35). Nil refuses to set one.
	Hasher *auth.Hasher
	// Gates reads the durable click budget and the workspace signing secret
	// (M35). Nil leaves both unavailable.
	Gates GateReader
	// DNS answers the custom-domain challenge (M40). Nil refuses verification.
	DNS TXTLookup
	// Hosts broadcasts a change to the verified hostname set across replicas.
	// Nil keeps the change local, which is the pre-pub/sub behaviour.
	Hosts HostInvalidator
	// DomainNotify warns a workspace whose hostname is failing verification.
	// Nil warns nobody.
	DomainNotify DomainNotifier
	// VerifyGrace is how long a failing hostname keeps serving (D70). Zero uses
	// DefaultVerifyGrace.
	VerifyGrace time.Duration
	// Log receives the warning when an audit write fails. Nil uses the default
	// logger, so a dropped record is never silent.
	Log *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		pool:    pool,
		q:       dbgen.New(pool),
		policy:  cfg.Policy,
		aliases: cfg.Aliases,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		cache:   cfg.Cache,

		splitHosts:  cfg.SplitHosts,
		rootCache:   cfg.RootCache,
		audit:       cfg.Audit,
		feed:        cfg.Feed,
		feedMetrics: cfg.FeedMetrics,
		hasher:      cfg.Hasher,
		gates:       cfg.Gates,

		dns:               cfg.DNS,
		hosts:             cfg.Hosts,
		domainNotify:      cfg.DomainNotify,
		verifyGraceWindow: cfg.VerifyGrace,
		linkScheme:        schemeOf(cfg.BaseURL),
		log:               log,
	}
}

// FeedDisclosure is what this instance does with destinations, as the
// disclosure page and the API print it.
//
// It reads the service's own checker rather than the configuration the checker
// was built from, so the page cannot describe a feed the service is not using.
// The zero Disclosure — Enabled false — is the default instance and is the
// whole of the answer there: nothing is sent anywhere.
func (s *Service) FeedDisclosure() feed.Disclosure {
	if s.feed == nil {
		return feed.Disclosure{}
	}
	return s.feed.Describe()
}

// CreateInput describes a new link.
type CreateInput struct {
	URL         string
	Alias       string // optional; generated when empty
	Title       string
	Description string
	Tags        []string
	ExpiresAt   *time.Time
	// ForwardQuery merges the visitor's query string into the destination.
	// Off by default; the destination's own parameters always win on conflict.
	ForwardQuery bool
	// ForwardPath appends the visitor's extra path segments to the destination.
	// Off by default: with it on the alias answers every path beneath itself,
	// and that is a decision about the link's whole namespace rather than about
	// one URL.
	ForwardPath bool

	// FolderID files the new link (M38). Nil leaves it unfiled, which is where
	// every link created before folders existed still is.
	FolderID *uuid.UUID

	// CampaignID labels the new link (M41). Nil leaves it unlabelled. A folder
	// and a campaign are different questions — where the link lives and what it
	// is for — so a link may carry both, one or neither.
	CampaignID *uuid.UUID

	// DomainID names the hostname the link is served on (M40). Nil takes the
	// workspace's own default, which is the instance default until the workspace
	// has verified a hostname of its own.
	//
	// It must be a domain this workspace may use *and* verified. Both halves are
	// checked, and the second is the one that matters: a link on an unverified
	// hostname would be a short URL the product handed somebody that resolves
	// nowhere, on a name this instance has no evidence they control.
	DomainID *uuid.UUID

	// The gates (M35). Each is off unless asked for, so a link created without
	// them is byte-for-byte the link this service created before they existed.
	//
	// Password is write-only in every direction: it is hashed here and nothing
	// reads it back. MaxClicks and OneTime are the same gate with different
	// numbers — see gate.ClickLimit — and RequireSignature refuses any request
	// without a valid HMAC for the alias.
	Password         string
	MaxClicks        *int64
	OneTime          bool
	RequireSignature bool
}

// Create makes a link, generating an alias when none is supplied.
func (s *Service) Create(ctx context.Context, actor *auth.Identity, in CreateInput) (*domain.Link, error) {
	if !actor.Can(PermCreate) {
		return nil, fmt.Errorf("%w: creating links requires %s", domain.ErrForbidden, PermCreate)
	}

	var errs domain.ValidationErrors

	// The gates (M35). Hashing happens before the transaction, because argon2id
	// is deliberately expensive and holding a transaction open across ~100ms of
	// key derivation would put the create path's cost onto every other writer.
	var passwordHash *string
	if in.Password != "" {
		hashed, perr := s.hashLinkPassword(in.Password)
		if perr != nil {
			var ve domain.ValidationErrors
			if errors.As(perr, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, perr
			}
		} else {
			passwordHash = &hashed
		}
	}
	errs = append(errs, validateClickLimit(in.MaxClicks)...)

	normalizedURL, err := s.checkDestination(ctx, actor, in.URL, surfaceLinkCreate)
	if err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			errs = append(errs, ve...)
		} else {
			return nil, err
		}
	}

	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		errs = append(errs, domain.FieldError{
			Field: "expires_at", Code: "in_past",
			Message: "expiry must be in the future",
		})
	}
	errs = append(errs, s.resolveFolder(ctx, actor.WorkspaceID, in.FolderID)...)
	errs = append(errs, s.resolveCampaign(ctx, actor.WorkspaceID, in.CampaignID)...)

	if len(errs) > 0 {
		return nil, errs
	}

	dom, err := s.resolveTargetDomain(ctx, actor, in.DomainID)
	if err != nil {
		return nil, err
	}

	// Resolve the alias before opening a transaction, so a long generation
	// loop does not hold one open.
	var code string
	if in.Alias != "" {
		code, err = s.aliases.Validate(in.Alias)
		if err != nil {
			var ae *alias.Error
			if errors.As(err, &ae) {
				return nil, domain.ValidationErrors{{
					Field: "alias", Code: string(ae.Reason), Message: ae.Message,
				}}
			}
			return nil, err
		}
		// Availability is checked here, not left to the unique index, because
		// the index cannot see two of the states that make an alias unavailable:
		// it is partial on deleted_at IS NULL, so a trashed row holding its
		// alias through the recovery window does not block an insert, and
		// reserved_aliases — trafficked aliases of purged links — is a separate
		// table entirely. The index remains the guarantee against live-row
		// races; this is the enforcement of "the alias stays reserved while the
		// row exists". One message for all three causes, deliberately: which of
		// them applies is not the caller's business.
		if taken, err := s.q.IsAliasTaken(ctx, dbgen.IsAliasTakenParams{
			DomainID: dom.ID, Alias: code,
		}); err != nil {
			return nil, fmt.Errorf("check alias: %w", err)
		} else if taken {
			return nil, fmt.Errorf("%w: alias %q is already in use", domain.ErrConflict, code)
		}
	} else {
		code, err = s.aliases.Generate(ctx, func(ctx context.Context, candidate string) (bool, error) {
			return s.q.IsAliasTaken(ctx, dbgen.IsAliasTakenParams{
				DomainID: dom.ID, Alias: candidate,
			})
		})
		if err != nil {
			return nil, fmt.Errorf("generate alias: %w", err)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	linkID := uuid.Must(uuid.NewV7())
	row, err := q.CreateLink(ctx, dbgen.CreateLinkParams{
		ID:               linkID,
		WorkspaceID:      actor.WorkspaceID,
		DomainID:         dom.ID,
		Alias:            code,
		PrimaryUrl:       normalizedURL,
		Title:            in.Title,
		Description:      in.Description,
		Status:           string(domain.StatusActive),
		ExpiresAt:        in.ExpiresAt,
		CreatedBy:        &actor.UserID,
		ForwardQuery:     in.ForwardQuery,
		ForwardPath:      in.ForwardPath,
		PasswordHash:     passwordHash,
		MaxClicks:        in.MaxClicks,
		OneTime:          in.OneTime,
		RequireSignature: in.RequireSignature,
		FolderID:         in.FolderID,
		CampaignID:       in.CampaignID,
	})
	if err != nil {
		// The unique index is the real guarantee; the pre-check only makes
		// this rare. A user-supplied alias that collides is a 409, while a
		// generated one that collides is a bug worth surfacing as such.
		if isUniqueViolation(err) {
			if in.Alias != "" {
				return nil, fmt.Errorf("%w: alias %q is already in use", domain.ErrConflict, code)
			}
			return nil, fmt.Errorf("%w: generated alias collided", domain.ErrConflict)
		}
		return nil, fmt.Errorf("create link: %w", err)
	}

	destID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateDestination(ctx, dbgen.CreateDestinationParams{
		ID:          destID,
		LinkID:      linkID,
		WorkspaceID: actor.WorkspaceID,
		Url:         normalizedURL,
		UrlHost:     HostOf(normalizedURL),
	}); err != nil {
		return nil, fmt.Errorf("create destination: %w", err)
	}
	if err := q.SetPrimaryDestination(ctx, dbgen.SetPrimaryDestinationParams{
		ID: linkID, PrimaryDestinationID: &destID,
	}); err != nil {
		return nil, fmt.Errorf("set primary destination: %w", err)
	}

	tags, err := s.applyTags(ctx, q, actor.WorkspaceID, linkID, in.Tags)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Clear any negative cache entry for this alias. Without this, an alias
	// somebody probed before it existed stays 404 for the whole negative TTL,
	// and the link looks broken immediately after creation.
	if s.cache != nil {
		s.cache.InvalidateAlias(ctx, dom.ID, code)
	}

	return s.toDomain(ctx, row, tags), nil
}

// UpdateInput is a partial update; nil fields are left unchanged.
type UpdateInput struct {
	URL          *string
	Alias        *string
	Title        *string
	Description  *string
	ExpiresAt    *time.Time
	ClearExpiry  bool
	Tags         *[]string
	ForwardQuery *bool
	ForwardPath  *bool
	// BotBlocking is the link's own answer to "refuse automated clients":
	// inherit, on, or off. Nil leaves it alone, which is what the dashboard form
	// sends when the domain enforces and the control is disabled.
	BotBlocking *domain.BotPolicy

	// Which folder the link is filed in (M38). Three states, exactly as the
	// expiry and the password have: nil leaves it where it is, an id files it
	// there, and ClearFolder takes it out of every folder. A form's "no folder"
	// option is the third, and without it a link could be filed and never
	// unfiled.
	FolderID    *uuid.UUID
	ClearFolder bool

	// Which campaign the link belongs to (M41). Three states for the reason the
	// folder above has them, and the third is the one that matters: without
	// ClearCampaign the only way out of a campaign joined by mistake would be to
	// delete the campaign, which would take every other link with it.
	CampaignID    *uuid.UUID
	ClearCampaign bool

	// The gates (M35). Two of them need three states rather than two, because
	// "leave the password alone" and "remove the password" are different
	// requests and a form that posts an empty box means the first: nobody can
	// re-type a password they cannot read, so an empty field has to be "no
	// change" or every save would clear the gate.
	//
	// Clearing is therefore explicit, exactly as ClearExpiry already is.
	Password         *string
	ClearPassword    bool
	MaxClicks        *int64
	ClearMaxClicks   bool
	OneTime          *bool
	RequireSignature *bool
}

func (s *Service) Update(ctx context.Context, actor *auth.Identity, id uuid.UUID, in UpdateInput) (*domain.Link, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	existing, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}

	var errs domain.ValidationErrors
	var normalizedURL string
	if in.URL != nil {
		normalizedURL, err = s.checkDestination(ctx, actor, *in.URL, surfaceLinkUpdate)
		if err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, err
			}
		}
	}

	var newAlias *string
	if in.Alias != nil && *in.Alias != existing.Alias {
		code, aerr := s.aliases.Validate(*in.Alias)
		if aerr != nil {
			var ae *alias.Error
			if errors.As(aerr, &ae) {
				errs = append(errs, domain.FieldError{
					Field: "alias", Code: string(ae.Reason), Message: ae.Message,
				})
			} else {
				return nil, aerr
			}
		} else {
			// Same availability rule as Create, for the same reason: the
			// partial unique index cannot see trashed rows or reserved
			// aliases, and an alias change is a creation as far as the
			// namespace is concerned.
			if taken, err := s.q.IsAliasTaken(ctx, dbgen.IsAliasTakenParams{
				DomainID: existing.DomainID, Alias: code,
			}); err != nil {
				return nil, fmt.Errorf("check alias: %w", err)
			} else if taken {
				return nil, fmt.Errorf("%w: alias %q is already in use", domain.ErrConflict, code)
			}
			newAlias = &code
		}
	}

	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		errs = append(errs, domain.FieldError{
			Field: "expires_at", Code: "in_past", Message: "expiry must be in the future",
		})
	}

	// The gates (M35). Same order as Create, and hashed outside the transaction
	// for the same reason.
	var passwordHash *string
	if in.Password != nil && *in.Password != "" {
		hashed, perr := s.hashLinkPassword(*in.Password)
		if perr != nil {
			var ve domain.ValidationErrors
			if errors.As(perr, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, perr
			}
		} else {
			passwordHash = &hashed
		}
	}
	errs = append(errs, validateClickLimit(in.MaxClicks)...)
	if !in.ClearFolder {
		errs = append(errs, s.resolveFolder(ctx, actor.WorkspaceID, in.FolderID)...)
		errs = append(errs, s.resolveCampaign(ctx, actor.WorkspaceID, in.CampaignID)...)
	}

	// The bot-blocking setting, and the one refusal it carries.
	//
	// An enforcing domain overrides a link that says off, and the override is
	// already unconditional in domain.BlocksBots — so accepting the value here
	// would store a setting that does nothing and tell the caller it worked.
	// That is the failure mode this refusal exists for: somebody turns bot
	// blocking off for their link, the API says 200, and bots keep being
	// refused with nothing anywhere explaining why.
	var newPolicy *string
	if in.BotBlocking != nil {
		dom, derr := s.q.GetDomainBotSettings(ctx, existing.DomainID)
		if derr != nil {
			return nil, fmt.Errorf("read domain bot settings: %w", derr)
		}
		policy := domain.DomainBots(dom.BlockBots, dom.BlockBotsEnforced)
		switch {
		case *in.BotBlocking == domain.BotAllow && domain.BotPolicyLocked(policy):
			errs = append(errs, domain.FieldError{
				Field: "bot_blocking", Code: "domain_enforced",
				Message: "bot blocking is enforced for every link on " + dom.Hostname +
					" and cannot be turned off per link",
			})
		default:
			v := string(*in.BotBlocking)
			newPolicy = &v
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.UpdateLink(ctx, dbgen.UpdateLinkParams{
		ID:           id,
		WorkspaceID:  actor.WorkspaceID,
		Title:        in.Title,
		Description:  in.Description,
		ExpiresAt:    in.ExpiresAt,
		ClearExpiry:  in.ClearExpiry,
		Alias:        newAlias,
		ForwardQuery: in.ForwardQuery,
		ForwardPath:  in.ForwardPath,
		BotBlocking:  newPolicy,

		PasswordHash:     passwordHash,
		ClearPassword:    in.ClearPassword,
		MaxClicks:        in.MaxClicks,
		ClearMaxClicks:   in.ClearMaxClicks,
		OneTime:          in.OneTime,
		RequireSignature: in.RequireSignature,
		FolderID:         in.FolderID,
		ClearFolder:      in.ClearFolder,
		CampaignID:       in.CampaignID,
		ClearCampaign:    in.ClearCampaign,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: alias is already in use", domain.ErrConflict)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("update link: %w", err)
	}

	// A rename abandons the old alias, and abandoning it is not the same as
	// freeing it. The partial unique index stops covering it the moment the row
	// changes, so without this the alias is immediately claimable by anyone —
	// including another workspace — while every bookmark and printed QR code
	// still points at it. That is the redirect hijack reserved_aliases exists
	// to prevent, and the purge job already prevents it on its own path; the
	// threshold here is deliberately the same one PurgeExpiredLinks uses, so
	// the two paths cannot disagree about what "in the wild" means.
	//
	// In the transaction with UpdateLink, because a rename that committed
	// without its reservation would leave the alias free with no second chance
	// to notice.
	if newAlias != nil && existing.ClickCount > 0 {
		if err := q.ReserveAlias(ctx, dbgen.ReserveAliasParams{
			DomainID: existing.DomainID,
			Alias:    existing.Alias,
			Reason:   "renamed with traffic",
		}); err != nil {
			return nil, fmt.Errorf("reserve previous alias: %w", err)
		}
	}

	if in.URL != nil {
		// Updates the destination; the trigger mirrors it into
		// links.primary_url. Editing the destination without changing the
		// alias is the core promise of the product, so this path matters.
		if err := q.UpdateDestinationURL(ctx, dbgen.UpdateDestinationURLParams{
			ID: id, WorkspaceID: actor.WorkspaceID,
			Url: normalizedURL, UrlHost: HostOf(normalizedURL),
		}); err != nil {
			return nil, fmt.Errorf("update destination: %w", err)
		}
		row.PrimaryUrl = normalizedURL
	}

	var tags []domain.Tag
	if in.Tags != nil {
		if err := q.DetachAllTags(ctx, id); err != nil {
			return nil, fmt.Errorf("detach tags: %w", err)
		}
		if tags, err = s.applyTags(ctx, q, actor.WorkspaceID, id, *in.Tags); err != nil {
			return nil, err
		}
	} else {
		if tags, err = s.loadTags(ctx, q, id); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Both the old and new alias must be invalidated: the old one so it stops
	// resolving, the new one so a stale negative entry does not shadow it.
	if s.cache != nil {
		s.cache.InvalidateAlias(ctx, existing.DomainID, existing.Alias)
		if newAlias != nil {
			s.cache.InvalidateAlias(ctx, existing.DomainID, *newAlias)
		}
	}

	// Who decided this link would refuse automated clients, and when.
	//
	// Recorded only when the value actually moved: a form that posts every field
	// re-sends the current setting on every save, and a log where most entries
	// say nothing changed is a log nobody reads. This is the administrative half
	// of the feature and it is the half the audit trail is for — the refusals
	// themselves are traffic, and are counted rather than recorded.
	if s.audit != nil && newPolicy != nil && *newPolicy != existing.BotBlocking {
		if err := s.audit.Record(ctx, actor, audit.Event{
			Action:     audit.ActionLinkBotBlockingChanged,
			TargetType: "link",
			TargetID:   &row.ID,
			Metadata: map[string]any{
				"alias": row.Alias,
				"from":  existing.BotBlocking,
				"to":    *newPolicy,
			},
		}); err != nil {
			s.log.Warn("bot blocking changed but the audit record was not written",
				slog.String("alias", row.Alias), slog.Any("error", err))
		}
	}

	return s.toDomain(ctx, row, tags), nil
}

func (s *Service) Get(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*domain.Link, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	row, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}
	tags, err := s.loadTags(ctx, s.q, id)
	if err != nil {
		return nil, err
	}
	// One link, so the exact click budget is affordable here in a way it is not
	// on the list (M35).
	return s.withBudget(ctx, s.toDomain(ctx, row, tags)), nil
}

// List returns a keyset-paginated page of links.
func (s *Service) List(ctx context.Context, actor *auth.Identity, f domain.LinkFilter) (*domain.Page[domain.Link], error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}

	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}

	// Normalized once, because the cursor records the sort it was minted under
	// and compares it on the way back in. Without this, an omitted sort and an
	// explicit "newest" are different strings for the same ordering, and adding
	// the parameter mid-pagination would be refused for no reason.
	sort := f.Sort
	if sort != domain.SortOldest && sort != domain.SortMostClicks {
		sort = domain.SortNewest
	}

	params := dbgen.ListLinksParams{
		WorkspaceID: actor.WorkspaceID,
		Sort:        string(sort),
		// One extra row tells us whether another page exists without a second
		// query and without a count.
		PageLimit: limit + 1,
	}
	if f.Status != "" {
		st := string(f.Status)
		params.Status = &st
	}
	// An empty or punctuation-only search must mean "no filter", not "match
	// nothing": websearch_to_tsquery yields an empty query for input like "!!"
	// and would otherwise return zero rows for a harmless keystroke.
	if q := strings.TrimSpace(f.Search); q != "" {
		params.Search = &q
	}
	if len(f.TagIDs) > 0 {
		params.TagIds = f.TagIDs
	}
	// Unfiled wins over a named folder, because a request asking for both has
	// contradicted itself and "the links in this folder that are in no folder"
	// has exactly one honest answer.
	if f.Unfiled {
		params.Unfiled = true
	} else {
		params.FolderID = f.FolderID
	}
	// The campaign filter (M41), resolved exactly as the folder pair above is
	// and for the same reason.
	if f.Uncampaigned {
		params.Uncampaigned = true
	} else {
		params.CampaignID = f.CampaignID
	}
	// Which hostname the links are served on (M40). Not validated against what
	// this workspace owns: the query is already scoped to the workspace, so an
	// id naming somebody else's domain returns an empty page rather than a
	// refusal — and returning a refusal would confirm the id names a real
	// hostname, which is the one thing another workspace's domain must not tell
	// this one.
	params.DomainID = f.DomainID
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		// A cursor is only meaningful under the sort that produced it: it names
		// a position in one ordering, and the query filters on whichever tuple
		// that sort uses. Carrying it into a different sort is how a page silently
		// skips and repeats rows, so it is refused rather than reinterpreted.
		if cur.Sort != string(sort) {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "sort_changed",
				Message: "pagination cursor belongs to a different sort order; start from the first page",
			}}
		}
		params.CursorCreated = &cur.CreatedAt
		params.CursorID = &cur.ID
		params.CursorClicks = &cur.Clicks
	}

	rows, err := s.q.ListLinks(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}

	page := &domain.Page[domain.Link]{Items: make([]domain.Link, 0, limit)}
	if len(rows) > int(limit) {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		tags := make([]domain.Tag, 0, len(r.TagIds))
		for i, idStr := range r.TagIds {
			id, err := uuid.Parse(idStr)
			if err != nil {
				continue
			}
			name := ""
			if i < len(r.TagNames) {
				name = r.TagNames[i]
			}
			tags = append(tags, domain.Tag{ID: id, Name: name})
		}
		// ListLinksRow is flat rather than embedding dbgen.Link, because the
		// aggregated tag columns are part of the same select.
		page.Items = append(page.Items, *s.toDomain(ctx, dbgen.Link{
			ID:           r.ID,
			WorkspaceID:  r.WorkspaceID,
			DomainID:     r.DomainID,
			FolderID:     r.FolderID,
			CampaignID:   r.CampaignID,
			Alias:        r.Alias,
			PrimaryUrl:   r.PrimaryUrl,
			Title:        r.Title,
			Description:  r.Description,
			Status:       r.Status,
			ExpiresAt:    r.ExpiresAt,
			ForwardQuery: r.ForwardQuery,
			ForwardPath:  r.ForwardPath,
			BotBlocking:  r.BotBlocking,
			// The gates, so a list can mark a gated link instead of showing it
			// as though it were open (M35).
			PasswordHash:     r.PasswordHash,
			MaxClicks:        r.MaxClicks,
			OneTime:          r.OneTime,
			RequireSignature: r.RequireSignature,
			ClickCount:       r.ClickCount,
			LastClickAt:      r.LastClickAt,
			CreatedAt:        r.CreatedAt,
			UpdatedAt:        r.UpdatedAt,
			ArchivedAt:       r.ArchivedAt,
		}, tags))
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(string(sort), last.CreatedAt, last.ClickCount, last.ID)
	}

	if f.IncludeTotal {
		total, err := s.q.CountLinks(ctx, dbgen.CountLinksParams{
			WorkspaceID: actor.WorkspaceID,
			Status:      params.Status,
			Search:      params.Search,
			// The same filter the page itself used, or the total describes a
			// different set of links than the items beside it.
			TagIds:       params.TagIds,
			Unfiled:      params.Unfiled,
			FolderID:     params.FolderID,
			Uncampaigned: params.Uncampaigned,
			CampaignID:   params.CampaignID,
			DomainID:     params.DomainID,
		})
		if err != nil {
			return nil, fmt.Errorf("count links: %w", err)
		}
		page.Total = &total
	}

	return page, nil
}

func (s *Service) Archive(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*domain.Link, error) {
	return s.setArchived(ctx, actor, id, true)
}

func (s *Service) Restore(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*domain.Link, error) {
	return s.setArchived(ctx, actor, id, false)
}

func (s *Service) setArchived(ctx context.Context, actor *auth.Identity, id uuid.UUID, archive bool) (*domain.Link, error) {
	if !actor.Can(PermDelete) {
		return nil, fmt.Errorf("%w: archiving links requires %s", domain.ErrForbidden, PermDelete)
	}

	var row dbgen.Link
	var err error
	if archive {
		row, err = s.q.ArchiveLink(ctx, dbgen.ArchiveLinkParams{ID: id, WorkspaceID: actor.WorkspaceID})
	} else {
		row, err = s.q.RestoreLink(ctx, dbgen.RestoreLinkParams{ID: id, WorkspaceID: actor.WorkspaceID})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("archive link: %w", err)
	}

	if s.cache != nil {
		s.cache.InvalidateAlias(ctx, row.DomainID, row.Alias)
	}
	tags, err := s.loadTags(ctx, s.q, id)
	if err != nil {
		return nil, err
	}
	return s.toDomain(ctx, row, tags), nil
}

// Delete soft-deletes a link, keeping it restorable for TrashRetentionDays.
func (s *Service) Delete(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermDelete) {
		return fmt.Errorf("%w: deleting links requires %s", domain.ErrForbidden, PermDelete)
	}

	row, err := s.q.SoftDeleteLink(ctx, dbgen.SoftDeleteLinkParams{
		ID: id, WorkspaceID: actor.WorkspaceID, RetentionDays: TrashRetentionDays,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("delete link: %w", err)
	}

	if s.cache != nil {
		s.cache.InvalidateAlias(ctx, row.DomainID, row.Alias)
	}
	return nil
}

// --- tags -------------------------------------------------------------------

func (s *Service) ListTags(ctx context.Context, actor *auth.Identity) ([]domain.Tag, error) {
	if !actor.Can(PermTagsRead) {
		return nil, domain.ErrForbidden
	}
	rows, err := s.q.ListTags(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	out := make([]domain.Tag, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Tag{ID: r.ID, Name: r.Name, Color: r.Color, LinkCount: r.LinkCount})
	}
	return out, nil
}

func (s *Service) DeleteTag(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermTagsWrite) {
		return domain.ErrForbidden
	}
	n, err := s.q.DeleteTag(ctx, dbgen.DeleteTagParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// applyTags resolves tag names to rows, creating any that do not exist.
//
// Tags are created implicitly rather than requiring a separate call: typing a
// new tag on a link is the common case, and making it a two-step operation
// serves nobody.
func (s *Service) applyTags(ctx context.Context, q *dbgen.Queries, wsID, linkID uuid.UUID, names []string) ([]domain.Tag, error) {
	out := make([]domain.Tag, 0, len(names))
	seen := map[string]bool{}

	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || len(name) > 64 {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true

		tag, err := q.GetTagByName(ctx, dbgen.GetTagByNameParams{WorkspaceID: wsID, Lower: name})
		if errors.Is(err, pgx.ErrNoRows) {
			tag, err = q.CreateTag(ctx, dbgen.CreateTagParams{
				ID: uuid.Must(uuid.NewV7()), WorkspaceID: wsID, Name: name,
			})
			// A concurrent create is fine: re-read the winner rather than
			// failing the whole link creation over a tag race.
			if err != nil && isUniqueViolation(err) {
				tag, err = q.GetTagByName(ctx, dbgen.GetTagByNameParams{WorkspaceID: wsID, Lower: name})
			}
		}
		if err != nil {
			return nil, fmt.Errorf("resolve tag %q: %w", name, err)
		}

		if err := q.AttachTag(ctx, dbgen.AttachTagParams{
			LinkID: linkID, TagID: tag.ID, WorkspaceID: wsID,
		}); err != nil {
			return nil, fmt.Errorf("attach tag: %w", err)
		}
		out = append(out, domain.Tag{ID: tag.ID, Name: tag.Name, Color: tag.Color})
	}
	return out, nil
}

func (s *Service) loadTags(ctx context.Context, q *dbgen.Queries, linkID uuid.UUID) ([]domain.Tag, error) {
	rows, err := q.GetLinkTags(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("load tags: %w", err)
	}
	out := make([]domain.Tag, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Tag{ID: r.ID, Name: r.Name, Color: r.Color})
	}
	return out, nil
}

func (s *Service) toDomain(ctx context.Context, l dbgen.Link, tags []domain.Tag) *domain.Link {
	if tags == nil {
		tags = []domain.Tag{}
	}
	return &domain.Link{
		ID:          l.ID,
		WorkspaceID: l.WorkspaceID,
		Alias:       l.Alias,
		ShortURL:    s.shortURL(ctx, l.DomainID, l.Alias),
		URL:         l.PrimaryUrl,
		Title:       l.Title,
		Description: l.Description,
		// Derived, not read: nothing writes 'expired' to the column. The list
		// and count queries filter on the same rule, and
		// TestExpiredLinkReportsAndFiltersAsExpired pins the two together.
		Status: domain.EffectiveStatus(
			domain.LinkStatus(l.Status), l.ExpiresAt, time.Now()),
		Tags:         tags,
		FolderID:     l.FolderID,
		CampaignID:   l.CampaignID,
		ForwardQuery: l.ForwardQuery,
		ForwardPath:  l.ForwardPath,
		BotBlocking:  domain.BotPolicy(l.BotBlocking),
		// The gates, and the password reduced to a boolean on the way out. The
		// hash is in `l` and stops here: nothing in this project hands one to a
		// caller, however privileged.
		HasPassword:      l.PasswordHash != nil && *l.PasswordHash != "",
		MaxClicks:        l.MaxClicks,
		OneTime:          l.OneTime,
		RequireSignature: l.RequireSignature,
		ExpiresAt:        l.ExpiresAt,
		ClickCount:       l.ClickCount,
		LastClickAt:      l.LastClickAt,
		CreatedAt:        l.CreatedAt,
		UpdatedAt:        l.UpdatedAt,
		ArchivedAt:       l.ArchivedAt,
	}
}

// Cursors encode the (created_at, id) pair the keyset query compares against.
// Opaque base64 rather than exposing the columns, so the pagination scheme can
// change without breaking clients that stored a cursor.
// cursor is a position in one specific ordering. It carries the sort it was
// minted under so the query cannot be asked to resume a 'clicks' page under
// 'newest', and it carries every column any sort keys on, because which one the
// predicate compares depends on that sort.
type cursor struct {
	Sort      string
	CreatedAt time.Time
	Clicks    int64
	ID        uuid.UUID
}

func encodeCursor(sort string, at time.Time, clicks int64, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join([]string{
		"1", sort, at.UTC().Format(time.RFC3339Nano),
		strconv.FormatInt(clicks, 10), id.String(),
	}, "|")))
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, err
	}
	// Version-prefixed and fixed-arity: a cursor from an older build decodes to
	// the wrong number of fields and is refused, rather than being read as a
	// position it does not describe.
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != "1" {
		return cursor{}, errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[2])
	if err != nil {
		return cursor{}, err
	}
	clicks, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return cursor{}, err
	}
	id, err := uuid.Parse(parts[4])
	if err != nil {
		return cursor{}, err
	}
	return cursor{Sort: parts[1], CreatedAt: at, Clicks: clicks, ID: id}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// schemeOf takes the scheme from the configured link base URL.
//
// A custom hostname is a name and nothing else: the row has no scheme, and
// hard-coding https would print a short URL that does not work on a development
// instance served over http. Whatever the operator configured for their own link
// host is the honest answer for a hostname served by the same listener.
func schemeOf(baseURL string) string {
	if strings.HasPrefix(strings.ToLower(baseURL), "http://") {
		return "http"
	}
	return "https"
}

// shortURL is the public URL of a link, on whichever hostname it lives.
//
// Off the default domain it is the configured link base URL, exactly as it was
// before custom domains existed. On a custom domain it is that domain's
// hostname, because a link created on go.acme.com whose short_url named the
// instance's own host would be a link the product told you to publish at the
// wrong address.
func (s *Service) shortURL(ctx context.Context, domainID uuid.UUID, alias string) string {
	if host := s.hostnameFor(ctx, domainID); host != "" {
		return s.linkScheme + "://" + host + "/" + alias
	}
	return s.baseURL + "/" + alias
}

// hostnameFor resolves a domain id to its hostname, or "" for the instance
// default and for anything that cannot be read.
//
// Cached in the process, because it is consulted once per link on every list
// page and the answer changes only when somebody renames a domain — which drops
// the entry here and broadcasts, so the other replicas drop theirs too. Empty
// for the instance default deliberately: that domain's hostname is a placeholder
// the resolver never reads (00700), and its public name is LINK_BASE_URL.
//
// A read failure returns "" and caches nothing, so the link is described with
// the instance's own host rather than with no host at all. Printing the wrong
// hostname would be worse than printing the default one only if the default were
// also wrong, and it is the address every link had before this milestone.
func (s *Service) hostnameFor(ctx context.Context, domainID uuid.UUID) string {
	if v, ok := s.hostnames.Load(domainID); ok {
		host, _ := v.(string)
		return host
	}
	row, err := s.q.GetDomainByID(ctx, domainID)
	if err != nil {
		return ""
	}
	host := ""
	if !row.IsDefault {
		host = strings.ToLower(row.Hostname)
	}
	s.hostnames.Store(domainID, host)
	return host
}

// ForgetHostnames drops the id-to-hostname cache.
//
// Wired to the same signal that reloads the verified-hostname set, so a rename
// on one replica reaches the short URLs printed by every other one. Without it a
// renamed domain would keep being advertised under its old name until the
// process restarted — a stale string in the one field whose whole job is to be
// copied and pasted.
func (s *Service) ForgetHostnames() {
	s.hostnames.Range(func(k, _ any) bool {
		s.hostnames.Delete(k)
		return true
	})
}

// targetDomain is the hostname a new link will be created on.
type targetDomain struct {
	ID       uuid.UUID
	Hostname string
}

// resolveTargetDomain decides which hostname a new link lands on.
//
// Two paths and one rule. With no domain named, the workspace's own default —
// which is its verified hostname if it has one, and the instance default
// otherwise; GetWorkspaceDefaultDomain is where that ordering lives, and this
// milestone is where it gained the workspace argument its name always claimed.
//
// With one named, it must be a domain the actor may use and it must be verified.
// The ownership check is the same one M39 wrote, reused rather than restated:
// the instance default is usable by everybody, an organization's domain by its
// members, a workspace's by that workspace. The verification check is M40's, and
// it is the reason this function exists rather than the id being passed
// straight through — a link created on an unverified hostname is a short URL
// that cannot resolve, minted on a name nobody has proved they hold.
func (s *Service) resolveTargetDomain(
	ctx context.Context, actor *auth.Identity, id *uuid.UUID,
) (targetDomain, error) {
	if id == nil {
		wsID, orgID := actor.WorkspaceID, actor.OrgID
		row, err := s.q.GetWorkspaceDefaultDomain(ctx, dbgen.GetWorkspaceDefaultDomainParams{
			WorkspaceID: &wsID, OrganizationID: &orgID,
		})
		if err != nil {
			return targetDomain{}, fmt.Errorf("resolve default domain: %w", err)
		}
		return targetDomain{ID: row.ID, Hostname: row.Hostname}, nil
	}

	row, err := s.q.GetDomainByID(ctx, *id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return targetDomain{}, domain.ValidationErrors{{
				Field: "domain_id", Code: "not_found",
				Message: "no such domain",
			}}
		}
		return targetDomain{}, fmt.Errorf("read domain: %w", err)
	}
	// Usable, not administrable. Creating a link on a hostname is what every
	// member of the workspace does; renaming or removing it needs
	// domains.write, and requiring that here would mean only admins could
	// create links once a workspace had its own hostname.
	usable := row.IsDefault ||
		(row.WorkspaceID != nil && *row.WorkspaceID == actor.WorkspaceID) ||
		(row.WorkspaceID == nil && row.OrganizationID != nil && *row.OrganizationID == actor.OrgID)
	if !usable {
		return targetDomain{}, fmt.Errorf("%w: that hostname belongs to another workspace",
			domain.ErrForbidden)
	}
	if !row.IsDefault && row.VerifiedAt == nil {
		return targetDomain{}, domain.ValidationErrors{{
			Field: "domain_id", Code: "unverified",
			Message: row.Hostname + " is not verified yet, so nothing is served on it; " +
				"publish its DNS record and verify it before putting links there",
		}}
	}
	return targetDomain{ID: row.ID, Hostname: row.Hostname}, nil
}
