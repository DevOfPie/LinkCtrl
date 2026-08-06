package link

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

// Campaigns (M41).
//
// **No permission of their own**, for the reason folders have none (D67) and
// routing rules have none (D49): a campaign is a label a link carries, so
// reading the list is `links.read`, creating a campaign is `links.create`,
// editing one is `links.update` and deleting one is `links.delete`. D18's
// delegability question does not arise, because there is no new slug to
// classify — an API key holding `links.update` can label links, which is what
// "organise links" means for a machine as much as for a person. Recorded as D75.
//
// Two rules are enforced here and nowhere else:
//
//   - **A slug is unique in the workspace, case-insensitively.** Backed by
//     `campaigns_workspace_slug_key` (00600), which is what makes it true when
//     two requests interleave; the check here is what makes the refusal a
//     sentence rather than a constraint violation.
//   - **Deleting a campaign unlabels its links rather than deleting them.** The
//     schema does not do this on its own: `links.campaign_id` is ON DELETE SET
//     NULL and the delete here is soft, so the cascade never fires. Both
//     statements run in one transaction — a link pointing at a campaign no query
//     returns is a link the campaign filter can reach and the campaign list
//     cannot explain.
//
// Nothing here invalidates a cached redirect snapshot, and that is a fact about
// the redirect path rather than an oversight: `campaign_id` is not in the
// snapshot, because what a link is *for* changes nothing about where it sends
// anybody. The schedule is descriptive for the same reason — see
// domain/campaign.go.

// CreateCampaignInput is a new campaign.
type CreateCampaignInput struct {
	Name string
	// Slug is optional; an empty one is derived from the name.
	Slug        string
	Description string
	StartsAt    *time.Time
	EndsAt      *time.Time
}

// UpdateCampaignInput is a partial update; nil fields are left unchanged.
type UpdateCampaignInput struct {
	Name        *string
	Slug        *string
	Description *string
	// The schedule bounds are three-valued, exactly as a link's expiry is: nil
	// leaves the bound alone and the Clear flag removes it.
	StartsAt      *time.Time
	ClearStartsAt bool
	EndsAt        *time.Time
	ClearEndsAt   bool
}

// Campaigns returns the workspace's campaigns, each with its link count.
func (s *Service) Campaigns(ctx context.Context, actor *auth.Identity) ([]domain.Campaign, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	return s.listCampaigns(ctx, actor.WorkspaceID)
}

// listCampaigns reads the workspace's campaigns, for callers that have already
// authorized — the link pages load them to draw a campaign picker.
func (s *Service) listCampaigns(ctx context.Context, workspaceID uuid.UUID) ([]domain.Campaign, error) {
	rows, err := s.q.ListCampaigns(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list campaigns: %w", err)
	}
	out := make([]domain.Campaign, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Campaign{
			ID: r.ID, WorkspaceID: r.WorkspaceID, Name: r.Name, Slug: r.Slug,
			Description: r.Description, StartsAt: r.StartsAt, EndsAt: r.EndsAt,
			LinkCount: r.LinkCount, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// Campaign returns one campaign.
func (s *Service) Campaign(
	ctx context.Context, actor *auth.Identity, id uuid.UUID,
) (*domain.Campaign, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	row, err := s.q.GetCampaign(ctx, dbgen.GetCampaignParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	// The link count is not on the single-row query: adding it would put a
	// grouped scan on a read whose caller — the edit form — never shows one, and
	// the list is where the number is read.
	return campaignFromRow(row, 0), nil
}

// CreateCampaign adds a campaign.
func (s *Service) CreateCampaign(
	ctx context.Context, actor *auth.Identity, in CreateCampaignInput,
) (*domain.Campaign, error) {
	if !actor.Can(PermCreate) {
		return nil, fmt.Errorf("%w: creating campaigns requires %s", domain.ErrForbidden, PermCreate)
	}

	name, errs := domain.ValidateCampaignName(in.Name)
	slug, slugErrs := domain.ValidateCampaignSlug(in.Slug, in.Name)
	errs = append(errs, slugErrs...)
	desc, descErrs := domain.ValidateCampaignDescription(in.Description)
	errs = append(errs, descErrs...)
	errs = append(errs, domain.ValidateCampaignSchedule(in.StartsAt, in.EndsAt)...)

	count, err := s.q.CountCampaigns(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("count campaigns: %w", err)
	}
	if count >= domain.MaxCampaignsPerWorkspace {
		errs = append(errs, domain.FieldError{
			Field: "name", Code: "too_many",
			Message: fmt.Sprintf("a workspace may have at most %d campaigns; past that "+
				"a campaign is not how anybody is finding a link, and tags are",
				domain.MaxCampaignsPerWorkspace),
		})
	}
	if len(errs) > 0 {
		return nil, errs
	}

	row, err := s.q.CreateCampaign(ctx, dbgen.CreateCampaignParams{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: actor.WorkspaceID,
		Name: name, Slug: slug, Description: desc,
		StartsAt: in.StartsAt, EndsAt: in.EndsAt,
	})
	if err != nil {
		// The index is the real guarantee; nothing above checks the slug against
		// the existing rows, because this is the check and it is one round trip
		// rather than two.
		if pgerr.IsUniqueViolation(err) {
			return nil, domain.ValidationErrors{campaignSlugTaken(slug)}
		}
		return nil, fmt.Errorf("create campaign: %w", err)
	}
	return campaignFromRow(row, 0), nil
}

// UpdateCampaign edits a campaign.
func (s *Service) UpdateCampaign(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, in UpdateCampaignInput,
) (*domain.Campaign, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: editing campaigns requires %s", domain.ErrForbidden, PermUpdate)
	}

	current, err := s.q.GetCampaign(ctx, dbgen.GetCampaignParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get campaign: %w", err)
	}

	var errs domain.ValidationErrors
	params := dbgen.UpdateCampaignParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
		ClearStartsAt: in.ClearStartsAt, ClearEndsAt: in.ClearEndsAt,
		StartsAt: in.StartsAt, EndsAt: in.EndsAt,
	}
	if in.Name != nil {
		name, e := domain.ValidateCampaignName(*in.Name)
		errs = append(errs, e...)
		params.Name = &name
	}
	if in.Slug != nil {
		// Derived from the *new* name when the slug field is blank, or from the
		// stored one when the name is not being changed — so clearing the slug
		// box on the edit form regenerates it rather than refusing.
		basis := current.Name
		if in.Name != nil {
			basis = *in.Name
		}
		slug, e := domain.ValidateCampaignSlug(*in.Slug, basis)
		errs = append(errs, e...)
		params.Slug = &slug
	}
	if in.Description != nil {
		desc, e := domain.ValidateCampaignDescription(*in.Description)
		errs = append(errs, e...)
		params.Description = &desc
	}

	// The schedule is checked against what the row will hold afterwards, not
	// against what the request carries: moving only the end date past the stored
	// start is valid, and moving it before is not.
	starts, ends := current.StartsAt, current.EndsAt
	if in.ClearStartsAt {
		starts = nil
	} else if in.StartsAt != nil {
		starts = in.StartsAt
	}
	if in.ClearEndsAt {
		ends = nil
	} else if in.EndsAt != nil {
		ends = in.EndsAt
	}
	errs = append(errs, domain.ValidateCampaignSchedule(starts, ends)...)
	if len(errs) > 0 {
		return nil, errs
	}

	row, err := s.q.UpdateCampaign(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if pgerr.IsUniqueViolation(err) {
			taken := current.Slug
			if params.Slug != nil {
				taken = *params.Slug
			}
			return nil, domain.ValidationErrors{campaignSlugTaken(taken)}
		}
		return nil, fmt.Errorf("update campaign: %w", err)
	}
	return campaignFromRow(row, 0), nil
}

// DeleteCampaign removes a campaign and unlabels its links.
//
// **The links survive**, which is the whole of what this method has to get
// right. A campaign is a label, and deleting a label deletes no link — but
// unlike the folder case (02400), no foreign key does this: the delete is soft,
// so `links.campaign_id ON DELETE SET NULL` never fires. Both statements are
// therefore in one transaction, and test/integration/campaigns_test.go asserts
// it rather than assuming it.
func (s *Service) DeleteCampaign(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermDelete) {
		return fmt.Errorf("%w: deleting campaigns requires %s", domain.ErrForbidden, PermDelete)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	n, err := q.DeleteCampaign(ctx, dbgen.DeleteCampaignParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return fmt.Errorf("delete campaign: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	if _, err := q.UnassignCampaignLinks(ctx, dbgen.UnassignCampaignLinksParams{
		CampaignID: &id, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return fmt.Errorf("unassign campaign links: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// resolveCampaign checks that a campaign id names a campaign of this workspace,
// for the link create and update paths.
//
// `links.campaign_id` has a foreign key to `campaigns(id)` that says nothing
// about tenancy, exactly as `folder_id` does — without this a caller could label
// their link with another workspace's campaign, and that campaign's link count
// would then report a link its readers cannot see.
func (s *Service) resolveCampaign(
	ctx context.Context, workspaceID uuid.UUID, campaignID *uuid.UUID,
) domain.ValidationErrors {
	if campaignID == nil {
		return nil
	}
	if _, err := s.q.GetCampaign(ctx, dbgen.GetCampaignParams{
		ID: *campaignID, WorkspaceID: workspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ValidationErrors{{
				Field: "campaign_id", Code: "not_found",
				Message: "no campaign with that id in this workspace",
			}}
		}
		return domain.ValidationErrors{{
			Field: "campaign_id", Code: "unavailable",
			Message: "the campaign could not be read: " + err.Error(),
		}}
	}
	return nil
}

func campaignFromRow(row dbgen.Campaign, linkCount int64) *domain.Campaign {
	return &domain.Campaign{
		ID: row.ID, WorkspaceID: row.WorkspaceID, Name: row.Name, Slug: row.Slug,
		Description: row.Description, StartsAt: row.StartsAt, EndsAt: row.EndsAt,
		LinkCount: linkCount, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func campaignSlugTaken(slug string) domain.FieldError {
	return domain.FieldError{
		Field: "slug", Code: "conflict",
		Message: fmt.Sprintf("there is already a campaign with the slug %q in this "+
			"workspace; a slug is what a filter URL names, so two cannot share one", slug),
	}
}
