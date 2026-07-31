package link

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
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
type Invalidator interface {
	InvalidateAlias(ctx context.Context, domainID uuid.UUID, alias string)
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
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	return &Service{
		pool:    pool,
		q:       dbgen.New(pool),
		policy:  cfg.Policy,
		aliases: cfg.Aliases,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		cache:   cfg.Cache,

		splitHosts: cfg.SplitHosts,
		rootCache:  cfg.RootCache,
	}
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

	// Phase 2 fields. Accepted by the parser so the API can reject them with a
	// specific message rather than ignoring them silently, which would look
	// like the feature works.
	Password  string
	MaxClicks *int64
	OneTime   bool
}

// Create makes a link, generating an alias when none is supplied.
func (s *Service) Create(ctx context.Context, actor *auth.Identity, in CreateInput) (*domain.Link, error) {
	if !actor.Can(PermCreate) {
		return nil, fmt.Errorf("%w: creating links requires %s", domain.ErrForbidden, PermCreate)
	}

	var errs domain.ValidationErrors

	// Phase 2 guards. Refusing loudly beats accepting and doing nothing.
	if in.Password != "" {
		errs = append(errs, domain.FieldError{
			Field: "password", Code: "not_implemented",
			Message: "password-protected links are not available in this version",
		})
	}
	if in.MaxClicks != nil || in.OneTime {
		errs = append(errs, domain.FieldError{
			Field: "max_clicks", Code: "not_implemented",
			Message: "click-limited and one-time links are not available in this version",
		})
	}

	normalizedURL, err := ValidateDestination(in.URL, s.policy)
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

	if len(errs) > 0 {
		return nil, errs
	}

	dom, err := s.q.GetWorkspaceDefaultDomain(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve default domain: %w", err)
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
		ID:           linkID,
		WorkspaceID:  actor.WorkspaceID,
		DomainID:     dom.ID,
		Alias:        code,
		PrimaryUrl:   normalizedURL,
		Title:        in.Title,
		Description:  in.Description,
		Status:       string(domain.StatusActive),
		ExpiresAt:    in.ExpiresAt,
		CreatedBy:    &actor.UserID,
		ForwardQuery: in.ForwardQuery,
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

	return s.toDomain(row, tags), nil
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
		normalizedURL, err = ValidateDestination(*in.URL, s.policy)
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
			LinkID: id, WorkspaceID: actor.WorkspaceID,
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

	return s.toDomain(row, tags), nil
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
	return s.toDomain(row, tags), nil
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
		page.Items = append(page.Items, *s.toDomain(dbgen.Link{
			ID:           r.ID,
			WorkspaceID:  r.WorkspaceID,
			DomainID:     r.DomainID,
			Alias:        r.Alias,
			PrimaryUrl:   r.PrimaryUrl,
			Title:        r.Title,
			Description:  r.Description,
			Status:       r.Status,
			ExpiresAt:    r.ExpiresAt,
			ForwardQuery: r.ForwardQuery,
			ClickCount:   r.ClickCount,
			LastClickAt:  r.LastClickAt,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
			ArchivedAt:   r.ArchivedAt,
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
			TagIds: params.TagIds,
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
	return s.toDomain(row, tags), nil
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

func (s *Service) toDomain(l dbgen.Link, tags []domain.Tag) *domain.Link {
	if tags == nil {
		tags = []domain.Tag{}
	}
	return &domain.Link{
		ID:          l.ID,
		WorkspaceID: l.WorkspaceID,
		Alias:       l.Alias,
		ShortURL:    s.baseURL + "/" + l.Alias,
		URL:         l.PrimaryUrl,
		Title:       l.Title,
		Description: l.Description,
		// Derived, not read: nothing writes 'expired' to the column. The list
		// and count queries filter on the same rule, and
		// TestExpiredLinkReportsAndFiltersAsExpired pins the two together.
		Status: domain.EffectiveStatus(
			domain.LinkStatus(l.Status), l.ExpiresAt, time.Now()),
		Tags:         tags,
		ForwardQuery: l.ForwardQuery,
		ExpiresAt:    l.ExpiresAt,
		ClickCount:   l.ClickCount,
		LastClickAt:  l.LastClickAt,
		CreatedAt:    l.CreatedAt,
		UpdatedAt:    l.UpdatedAt,
		ArchivedAt:   l.ArchivedAt,
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
