package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// maxOrganizationName bounds a name the same way a workspace's is bounded.
const maxOrganizationName = 80

// Organization is a newly created organization and the workspace it was
// provisioned with.
//
// The workspace is part of the answer rather than a detail: an organization
// with nothing to work in is not usable, so the call that makes one makes both,
// and the response says where to go.
type Organization struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
	// IsPersonal is false for everything created here. The flag marks the
	// organization registration provisions alongside an account — "your own
	// space" — and an organization somebody deliberately created to share is not
	// that, whatever they end up using it for.
	IsPersonal    bool      `json:"is_personal"`
	WorkspaceID   uuid.UUID `json:"workspace_id"`
	WorkspaceName string    `json:"workspace_name"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateOrganization provisions an organization, its first workspace and an
// owner membership for the caller, in one transaction.
//
// The provisioning is auth.ProvisionOrganization — literally the function
// registration calls — rather than a second implementation of the same four
// writes. That is deliberate: the tenancy invariants (an organization always has
// a workspace, and always has an owner, both written in the transaction that
// created it) are the kind that hold until somebody writes them out a second
// time slightly differently.
//
// Gated on orgs.create (D16), which on a default instance is held by the account
// from the setup form and nobody else — see 01300_orgs_create.sql for why that
// is a role grant rather than a check against how the account was made. The
// permission is also the call site a future entitlement check would hang on
// (Phase 3+); nothing here is billing-shaped, and the point of naming it now is
// that the check has somewhere to go without a schema change.
//
// The caller becomes the owner. Not a parameter, and not settable: an
// organization created on somebody else's behalf would be an organization
// nobody asked for, and the account that holds orgs.create is the one that
// wanted it.
func (s *Service) CreateOrganization(
	ctx context.Context, actor *auth.Identity, name string,
) (*Organization, error) {
	if !actor.Can(PermOrgsCreate) {
		return nil, fmt.Errorf("%w: creating an organization requires %s",
			domain.ErrForbidden, PermOrgsCreate)
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, domain.ValidationErrors{{
			Field: "name", Code: "required", Message: "give the organization a name",
		}}
	}
	if len(name) > maxOrganizationName {
		return nil, domain.ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("an organization name is at most %d characters", maxOrganizationName),
		}}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	org, ws, err := auth.ProvisionOrganization(ctx, s.q.WithTx(tx), actor.UserID, name, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Recorded against the new organization rather than the caller's current
	// one. The audit list is scoped by the actor's organization, so this record
	// lands where the new organization's own history begins — which is where
	// somebody reading that organization's log would look for how it started.
	s.record(ctx, &auth.Identity{
		UserID:      actor.UserID,
		Email:       actor.Email,
		Name:        actor.Name,
		OrgID:       org.ID,
		WorkspaceID: ws.ID,
		APIKeyID:    actor.APIKeyID,
	}, audit.Event{
		Action:     audit.ActionOrganizationCreated,
		TargetType: "organization",
		TargetID:   &org.ID,
		Metadata: map[string]any{
			"name":      org.Name,
			"slug":      org.Slug,
			"workspace": ws.Name,
		},
	})

	return &Organization{
		ID:            org.ID,
		Name:          org.Name,
		Slug:          org.Slug,
		IsPersonal:    org.IsPersonal,
		WorkspaceID:   ws.ID,
		WorkspaceName: ws.Name,
		CreatedAt:     org.CreatedAt,
	}, nil
}
