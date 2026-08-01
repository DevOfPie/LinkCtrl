package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Workspace is one entry in the switcher.
//
// Deliberately not the database row: the switcher needs a label and two flags,
// and handing the whole workspace out would put analytics retention and soft
// deletion on a JSON surface nobody asked for.
type Workspace struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
	// Organization is carried because a workspace name is only unique inside
	// one. Two organizations both calling a workspace "Marketing" is normal, and
	// a switcher that showed the workspace name alone would be unreadable.
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	IsPersonal       bool      `json:"is_personal"`
	// Current is where this request is acting. Computed against the identity
	// rather than stored, because "current" is a property of the request.
	Current bool `json:"current"`
	// Default marks the pinned workspace: where a new session starts. No entry
	// carries it when the user is on last-used, which is the default state.
	Default bool `json:"default"`
}

// resolveWorkspace answers "which workspace is this user acting in", and is the
// only path by which that question is answered.
//
// One function, four callers — login, session authentication, the CLI's
// identity lookup, and an API key that names no workspace of its own. Before
// this milestone each of them called the same query directly, which was fine
// while the answer was "the oldest one"; with a preference and a switcher in
// play, four call sites would be four chances for one of them to keep resolving
// the old way, and the bug would only appear once somebody held two
// memberships. Everything about the precedence lives in the query.
//
// sessionID is nil for the three callers that have no session.
func (s *Service) resolveWorkspace(ctx context.Context, userID uuid.UUID, sessionID *uuid.UUID) (dbgen.Workspace, error) {
	ws, err := s.q.ResolveWorkspaceForUser(ctx, dbgen.ResolveWorkspaceForUserParams{
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A user with no membership at all. Registration provisions one in
			// the same transaction as the user, so this is a broken instance
			// rather than a state a caller can reach — and saying so is more
			// use than "no rows".
			return ws, fmt.Errorf("auth: user %s belongs to no workspace: %w", userID, err)
		}
		return ws, fmt.Errorf("resolve workspace: %w", err)
	}
	return ws, nil
}

// Workspaces lists what the actor may switch to, newest information first: the
// current one is flagged, and so is the pinned default if there is one.
//
// Readable with any credential, including an API key. There is no permission
// for it because it exposes nothing but the caller's own memberships, which is
// the same reason the notification inbox has none.
func (s *Service) Workspaces(ctx context.Context, actor *Identity) ([]Workspace, error) {
	if actor == nil {
		return nil, domain.ErrUnauthorized
	}
	rows, err := s.q.ListWorkspacesForUser(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	out := make([]Workspace, 0, len(rows))
	for _, r := range rows {
		out = append(out, Workspace{
			ID:               r.ID,
			Name:             r.Name,
			Slug:             r.Slug,
			OrganizationID:   r.OrganizationID,
			OrganizationName: r.OrganizationName,
			IsPersonal:       r.IsPersonal,
			Current:          r.ID == actor.WorkspaceID,
			Default:          r.IsDefault,
		})
	}
	return out, nil
}

// SwitchWorkspace moves the caller's session, and remembers the choice.
//
// Two writes in one transaction, because they mean different things and both
// have to happen: the session moves so the next request is already in the new
// workspace, and the user's last-used is updated so the next *session* starts
// there too. Half of that would be a switcher that either forgets on sign-in or
// does not take effect until one.
//
// Requires a session, like changing a password does. An API key acts in the
// workspace its own row names, so a key switching would change nothing about
// its own requests while quietly repointing where its owner lands — a side
// effect on somebody else's browser, from a credential that cannot see it.
func (s *Service) SwitchWorkspace(ctx context.Context, actor *Identity, workspaceID uuid.UUID) error {
	if err := requireSessionActor(actor, "switching workspace"); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	n, err := q.SetLastWorkspaceForUser(ctx, dbgen.SetLastWorkspaceForUserParams{
		UserID: actor.UserID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("remember workspace: %w", err)
	}
	if n == 0 {
		// Not a member, or no such workspace. One answer for both, so an id
		// cannot be probed for existence.
		return domain.ErrNotFound
	}

	if _, err := q.SetSessionWorkspace(ctx, dbgen.SetSessionWorkspaceParams{
		SessionID: actor.SessionID, UserID: actor.UserID, WorkspaceID: workspaceID,
	}); err != nil {
		return fmt.Errorf("move session: %w", err)
	}

	return tx.Commit(ctx)
}

// SetDefaultWorkspace pins where new sessions start, or clears the pin.
//
// nil means "follow last-used", which is what the control offers as its first
// option and what every account is on until somebody chooses otherwise (D22).
// The derived behaviour stays the default; this exists for the person it
// annoys.
//
// Session-only for the same reason as SwitchWorkspace: it is an account
// preference, and a leaked key must not be able to decide where its owner's
// browser lands.
func (s *Service) SetDefaultWorkspace(ctx context.Context, actor *Identity, workspaceID *uuid.UUID) error {
	if err := requireSessionActor(actor, "setting the default workspace"); err != nil {
		return err
	}
	n, err := s.q.SetDefaultWorkspaceForUser(ctx, dbgen.SetDefaultWorkspaceForUserParams{
		UserID: actor.UserID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("set default workspace: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}
