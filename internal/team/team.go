// Package team manages the people in an organization, the workspaces they act
// in, and the creation of organizations themselves.
//
// It is the other half of internal/invite. An invitation decides who may join;
// everything here decides what happens after they have — which role they hold,
// which workspaces that role reaches, and how somebody leaves. Until this
// package existed the only correction available was SQL against the database.
//
// Three decisions shape it.
//
// **A member manages only ranks strictly below their own, and owners are the
// exception** (D30). An admin re-roles and removes editors and viewers, never
// another admin and never themselves; only an owner manages admins, and an
// owner manages other owners because an owner already holds everything, so
// there is no escalation left for the rule to prevent. The last owner of an
// organization cannot be removed or demoted by anybody, which is what stops one
// being orphaned. The table this implements is written out in
// docs/build-notes/phase-details/m28.md, before this code existed.
//
// **A workspace-scoped membership only ever adds** (D31). Permissions are the
// union of every membership matching the workspace and the effective role is
// the lowest rank among them, which is what GetUserPermissions and
// GetUserRoleInWorkspace already compute. So granting somebody a role in one
// workspace widens what they can do there and narrows nothing anywhere — the
// evaluator is not touched by this milestone, and the control that issues one
// says so.
//
// **A workspace holding any link refuses to be deleted** (D32). links, tags and
// folders all cascade from workspaces, Phase 1 has no trash to restore from, and
// archiving is deliberately not an escape hatch: an archived link keeps its
// alias and its click history. The guard goes in front of the cascade, and the
// links have to be deleted first.
package team

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The permissions this package enforces.
//
// members.write is the same slug internal/invite guards issuing an invitation
// with, named again here rather than imported: the two are separate call sites
// for one permission, and a package that enforces a permission says which one in
// its own vocabulary. members.read is enforced for the first time anywhere by
// this package — it has been seeded since Phase 1 with nothing reading it.
//
// workspace.write already guards changing a workspace's settings; creating,
// renaming and deleting one are the same authority over the same object, so no
// new permission is introduced for them. orgs.create is new (D16, 01300).
const (
	PermMembersRead    = "members.read"
	PermMembersWrite   = "members.write"
	PermWorkspaceRead  = "workspace.read"
	PermWorkspaceWrite = "workspace.write"
	PermOrgsCreate     = "orgs.create"
)

// Service manages members, workspaces and organization creation.
type Service struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	cfg  Config
	log  *slog.Logger
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree.
type Config struct {
	// Audit records every change made here. Nil records nothing.
	Audit audit.Recorder
	Log   *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), cfg: cfg, log: cfg.Log}
}

// record writes one audit event, logging rather than failing when it cannot.
//
// After the write and outside its transaction, like every other audit emission
// in this tree: the change is what the administrator asked for, and failing it
// because the record could not be written would trade a missing audit line for
// a change that did not happen.
func (s *Service) record(ctx context.Context, actor *auth.Identity, e audit.Event) {
	if s.cfg.Audit == nil {
		return
	}
	if err := s.cfg.Audit.Record(ctx, actor, e); err != nil {
		s.log.Warn("team change made but the audit record was not written",
			slog.String("action", e.Action), slog.Any("error", err))
	}
}

// ownerRank reads the rank of the built-in owner role.
//
// Read rather than written as a constant, because the exception D30 carves out
// is defined on rank and the ranks live in 00700_seed.sql. A constant here would
// be a second statement of the same number, and the way that goes wrong is that
// somebody edits the seed.
func (s *Service) ownerRank(ctx context.Context, q *dbgen.Queries) (int32, error) {
	role, err := q.GetBuiltinRoleBySlug(ctx, "owner")
	if err != nil {
		return 0, fmt.Errorf("look up owner role: %w", err)
	}
	return role.Rank, nil
}

// mayManage reports whether an actor at actorRank may re-role or remove a
// membership at targetRank. It is D30 in one expression.
//
// Strictly below, so an admin cannot reach another admin — nor themselves, since
// self is not strictly below self, and an admin who wants out asks an owner.
// Owners are the single exception: an owner already holds everything, so
// managing a peer grants them nothing, and without it a co-owner who leaves
// would be removable only by SQL.
//
// Evaluated on rank and never on identity, which is what makes a second owner
// manageable by the first without either being special. An identity whose role
// did not resolve carries auth.NoRoleRank and fails both limbs, which is the
// direction this must fail in.
func mayManage(actorRank, targetRank, ownerRank int32) bool {
	if actorRank <= ownerRank {
		return true
	}
	return targetRank > actorRank
}

// resolveRole turns a role slug into its seeded row, applying the ceiling
// m28.md states as "nobody grants a role binding tighter than their own".
//
// The same ceiling an invitation carries (D28), and deliberately so: the two are
// one question — what authority may this actor hand out — asked at two moments.
// A role above the actor's own rank is refused rather than clamped, because
// silently granting less than was asked for is how somebody ends up believing
// they promoted a colleague who was not promoted.
func (s *Service) resolveRole(
	ctx context.Context, q *dbgen.Queries, actor *auth.Identity, slug string,
) (dbgen.GetBuiltinRoleBySlugRow, error) {
	var role dbgen.GetBuiltinRoleBySlugRow
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return role, domain.ValidationErrors{{
			Field: "role", Code: "required", Message: "choose a role",
		}}
	}
	role, err := q.GetBuiltinRoleBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return role, domain.ValidationErrors{{
				Field: "role", Code: "invalid",
				Message: "that is not a role: choose owner, admin, editor or viewer",
			}}
		}
		return role, fmt.Errorf("look up role %q: %w", slug, err)
	}
	if role.Rank < actor.RoleRank {
		return role, fmt.Errorf(
			"%w: you cannot grant a role above your own (%s)", domain.ErrForbidden, actor.Role)
	}
	return role, nil
}

// Role is one choice in a role control.
type Role struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Rank        int32  `json:"rank"`
}

// Roles lists the roles this actor may assign: their own rank and below, most
// powerful first.
//
// Read from the seeded rows rather than listed in Go, so a control cannot offer
// something resolveRole will then refuse. The same shape internal/invite's
// Roles has, for the same reason.
func (s *Service) Roles(ctx context.Context, actor *auth.Identity) ([]Role, error) {
	if !actor.Can(PermMembersWrite) {
		return nil, fmt.Errorf("%w: assigning a role requires %s", domain.ErrForbidden, PermMembersWrite)
	}
	rows, err := s.q.ListBuiltinRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	out := make([]Role, 0, len(rows))
	for _, r := range rows {
		if r.Rank < actor.RoleRank {
			continue
		}
		out = append(out, Role{Slug: r.Slug, Name: r.Name, Description: r.Description, Rank: r.Rank})
	}
	return out, nil
}

// canInWorkspace answers whether a user holds a permission in a workspace that
// is not the one their request is acting in.
//
// Needed because an Identity carries the permissions for one workspace — the
// current one — and renaming or deleting a *different* workspace in the same
// organization is a decision about that one. Under D31 a member can hold
// workspace.write in one workspace and nothing in the next, so trusting the
// identity's own set here would let a workspace-scoped admin delete a workspace
// they have no access to at all.
func canInWorkspace(
	ctx context.Context, q *dbgen.Queries, userID, workspaceID uuid.UUID, permission string,
) (bool, error) {
	perms, err := q.GetUserPermissions(ctx, dbgen.GetUserPermissionsParams{
		UserID: userID, ID: workspaceID,
	})
	if err != nil {
		return false, fmt.Errorf("load permissions for workspace %s: %w", workspaceID, err)
	}
	for _, p := range perms {
		if p == permission {
			return true, nil
		}
	}
	return false, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
