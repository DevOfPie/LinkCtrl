package team

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// maxWorkspaceName bounds a name at something a heading can hold. Far above any
// real workspace name, far below anything that would break a layout.
const maxWorkspaceName = 80

// Workspace is one workspace of the caller's organization, as the management
// page sees it.
//
// Deliberately not auth.Workspace, which is the switcher's shape: that one
// carries the organization it belongs to because the switcher spans several,
// and this list never leaves one.
type Workspace struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
	// Current is where this request is acting. Computed against the identity
	// rather than stored, because "current" is a property of the request.
	Current bool `json:"current"`
	// Manageable is whether the caller may rename or delete this one. Under D31
	// a member can hold workspace.write in one workspace and nothing in the next,
	// so this is answered per row rather than once for the page.
	Manageable bool `json:"manageable"`
}

// Workspaces lists the workspaces of the caller's organization that they may
// act in.
//
// The same membership rule the evaluator applies, because it is the same query
// the switcher uses — an organization-wide membership matches every workspace,
// a workspace-scoped one matches exactly its own. So this is not "every
// workspace in the organization": it is every workspace this person has any
// business seeing, which is the set a management page should offer.
func (s *Service) Workspaces(ctx context.Context, actor *auth.Identity) ([]Workspace, error) {
	if !actor.Can(PermWorkspaceRead) {
		return nil, fmt.Errorf("%w: listing workspaces requires %s",
			domain.ErrForbidden, PermWorkspaceRead)
	}
	rows, err := s.q.ListWorkspacesForUser(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	out := make([]Workspace, 0, len(rows))
	for _, r := range rows {
		if r.OrganizationID != actor.OrgID {
			continue
		}
		manageable, err := canInWorkspace(ctx, s.q, actor.UserID, r.ID, PermWorkspaceWrite)
		if err != nil {
			return nil, err
		}
		out = append(out, Workspace{
			ID:         r.ID,
			Name:       r.Name,
			Slug:       r.Slug,
			Current:    r.ID == actor.WorkspaceID,
			Manageable: manageable,
		})
	}
	return out, nil
}

// CreateWorkspace adds a workspace to the caller's organization.
//
// Gated on workspace.write in the workspace the request is acting in, which is
// what the identity carries. That is the right question for a create: there is
// no target workspace yet, and the authority being exercised is over the
// organization the caller is currently in.
func (s *Service) CreateWorkspace(
	ctx context.Context, actor *auth.Identity, name string,
) (*Workspace, error) {
	if !actor.Can(PermWorkspaceWrite) {
		return nil, fmt.Errorf("%w: creating a workspace requires %s",
			domain.ErrForbidden, PermWorkspaceWrite)
	}
	name, slug, err := workspaceName(name)
	if err != nil {
		return nil, err
	}

	row, err := s.q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{
		ID:             uuid.Must(uuid.NewV7()),
		OrganizationID: actor.OrgID,
		Name:           name,
		Slug:           slug,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nameTaken()
		}
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionWorkspaceCreated,
		TargetType: "workspace",
		TargetID:   &row.ID,
		Metadata:   map[string]any{"name": row.Name, "slug": row.Slug},
	})

	return &Workspace{ID: row.ID, Name: row.Name, Slug: row.Slug, Manageable: true}, nil
}

// RenameWorkspace changes a workspace's name, and its slug with it.
//
// The permission is read against the *target* workspace rather than taken from
// the identity. An identity carries the permissions of the workspace its request
// is acting in, and under D31 somebody can hold workspace.write in one workspace
// and nothing at all in the next — so trusting the identity here would let a
// workspace-scoped admin rename a workspace they cannot even see.
func (s *Service) RenameWorkspace(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, name string,
) (*Workspace, error) {
	name, slug, err := workspaceName(name)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	ws, err := s.writableWorkspace(ctx, q, actor, id)
	if err != nil {
		return nil, err
	}

	row, err := q.RenameWorkspace(ctx, dbgen.RenameWorkspaceParams{
		ID: ws.ID, OrganizationID: actor.OrgID, Name: name, Slug: slug,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nameTaken()
		}
		return nil, fmt.Errorf("rename workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionWorkspaceRenamed,
		TargetType: "workspace",
		TargetID:   &row.ID,
		Metadata:   map[string]any{"from": ws.Name, "to": row.Name, "slug": row.Slug},
	})

	return &Workspace{
		ID: row.ID, Name: row.Name, Slug: row.Slug,
		Current: row.ID == actor.WorkspaceID, Manageable: true,
	}, nil
}

// DeleteWorkspace removes a workspace, and refuses while anything depends on
// it.
//
// Two refusals, and they are different kinds of protection.
//
// **Any link at all** (D32). links, tags and folders cascade from workspaces, so
// this delete is a redirect outage for every alias in it, and Phase 1 has no
// trash to restore one from. Archived links count: an archived link keeps its
// alias and its click history, so cascading it away would be silent data loss
// dressed as tidying up. The cost the owner accepted knowingly is that emptying
// a workspace is a link-at-a-time job, because Phase 2 has neither bulk delete
// nor a cross-workspace move.
//
// **The organization's last workspace.** Every member of an organization
// resolves into one of its workspaces to act at all, and
// ResolveWorkspaceForUser reports finding none as a broken instance rather than
// as an empty state — so deleting the last one would leave every member of the
// organization unable to authenticate. Guarded for the same reason the last
// owner is: the state is unreachable by any other route and unrecoverable
// without SQL.
//
// **What survives it.** The aliases of trashed links that received traffic. The
// link refusal is about live links and says nothing about trashed ones, so this
// delete is a third path by which an alias leaves its row — beside the purge job
// and the rename — and it is the one that used to release the alias for free
// (F28). All three now reserve at the same threshold.
func (s *Service) DeleteWorkspace(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	ws, err := s.writableWorkspace(ctx, q, actor, id)
	if err != nil {
		return err
	}

	// Counted inside the transaction that holds the workspace row locked, so a
	// link created while this was being decided cannot slip through between the
	// count and the cascade.
	links, err := q.CountWorkspaceLinks(ctx, ws.ID)
	if err != nil {
		return fmt.Errorf("count links: %w", err)
	}
	if links > 0 {
		return fmt.Errorf(
			"%w: %s still holds %d link(s), archived ones included; delete them first",
			domain.ErrConflict, ws.Name, links)
	}

	remaining, err := q.CountOrganizationWorkspaces(ctx, actor.OrgID)
	if err != nil {
		return fmt.Errorf("count workspaces: %w", err)
	}
	if remaining <= 1 {
		return fmt.Errorf(
			"%w: this is the organization's only workspace, and everybody in it has to "+
				"have one to work in; create another first",
			domain.ErrConflict)
	}

	// The guard above counted live links and found none. Trashed ones it
	// deliberately does not count, so the cascade below may still be about to
	// hard-delete links — and every alias among them that received traffic is on
	// printed material and in somebody's bookmarks. Reserved here, in the
	// transaction that deletes them, because a delete that committed without its
	// reservations would release those aliases to the whole instance with no
	// second chance to notice (F28).
	if err := q.ReserveWorkspaceTraffickedAliases(ctx, ws.ID); err != nil {
		return fmt.Errorf("reserve trafficked aliases: %w", err)
	}

	n, err := q.DeleteWorkspace(ctx, dbgen.DeleteWorkspaceParams{
		ID: ws.ID, OrganizationID: actor.OrgID,
	})
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// The name is in the metadata because the row it named is gone: this record
	// is the only remaining trace of what was deleted.
	s.record(ctx, actor, audit.Event{
		Action:     audit.ActionWorkspaceDeleted,
		TargetType: "workspace",
		TargetID:   &ws.ID,
		Metadata:   map[string]any{"name": ws.Name, "slug": ws.Slug},
	})
	return nil
}

// writableWorkspace loads a workspace of the actor's organization and refuses
// unless they hold workspace.write *in it*. The row comes back locked.
func (s *Service) writableWorkspace(
	ctx context.Context, q *dbgen.Queries, actor *auth.Identity, id uuid.UUID,
) (dbgen.Workspace, error) {
	ws, err := q.GetWorkspaceInOrganization(ctx, dbgen.GetWorkspaceInOrganizationParams{
		ID: id, OrganizationID: actor.OrgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ws, domain.ErrNotFound
		}
		return ws, fmt.Errorf("look up workspace: %w", err)
	}
	allowed, err := canInWorkspace(ctx, q, actor.UserID, ws.ID, PermWorkspaceWrite)
	if err != nil {
		return ws, err
	}
	if !allowed {
		return ws, fmt.Errorf("%w: changing that workspace requires %s in it",
			domain.ErrForbidden, PermWorkspaceWrite)
	}
	return ws, nil
}

// workspaceName validates a name and derives the slug stored beside it.
//
// The slug is derived rather than asked for. Two fields that have to agree is
// two fields somebody can make disagree, and the only thing the slug is read for
// is the per-organization uniqueness index — so a name that reduces to the same
// slug as another is a name collision, and reported as one.
func workspaceName(raw string) (name, slug string, err error) {
	name = strings.TrimSpace(raw)
	if name == "" {
		return "", "", domain.ValidationErrors{{
			Field: "name", Code: "required", Message: "give the workspace a name",
		}}
	}
	if len(name) > maxWorkspaceName {
		return "", "", domain.ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("a workspace name is at most %d characters", maxWorkspaceName),
		}}
	}
	slug = auth.Slugify(name)
	return name, slug, nil
}

func nameTaken() error {
	return domain.ValidationErrors{{
		Field: "name", Code: "taken",
		Message: "a workspace in this organization already uses that name",
	}}
}
