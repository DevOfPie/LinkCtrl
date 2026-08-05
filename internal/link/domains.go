package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Per-domain ownership (M39).
//
// **Ownership and management only. Nothing here is served.** A hostname
// registered through this file is stored with `verified_at` NULL, no router
// looks a hostname up, and an unrecognized Host still gets the operational 404 —
// asserted, unchanged, by test/integration/hosts_test.go. Verification and
// serving are M40's, and the split is what keeps the alias-hijack half of custom
// domains reviewable on its own.
//
// **One owning workspace per domain**, which is decision D68 and the reason
// there is a column here rather than a join table. Alias uniqueness is per
// domain: `links_domain_alias_key` makes a hostname exactly one alias namespace,
// so two workspaces sharing a hostname would contend for the same aliases with
// nothing to arbitrate between them. Anything that lets more than one workspace
// own a hostname re-opens that.
//
// **`domains.write` becomes an ownership check.** M20 minted the permission for
// one instance-wide setting and there was nothing to own; holding it now decides
// what you may do to *your* domains and nothing about anybody else's. The
// refusal is `ErrForbidden`, so another workspace's admin sees a 403 rather than
// a 404 — they are not being told the hostname does not exist, they are being
// told it is not theirs, which is the true answer and the one that does not
// invite guessing.
//
// No new permission slug. `domains.write` already exists (migration 00800,
// granted to the owner and admin roles), and what M39 adds is a scope check on
// the permission rather than a second kind of permission; see decisions.md, D69.

// Domain is a hostname as the dashboard and the API see it.
//
// The settings columns — root redirect, bot blocking — are deliberately absent.
// They belong to the instance default and are read and written through
// DomainSettings; putting them on a registered hostname would be configuring how
// something serves before anything serves it.
type Domain struct {
	ID       uuid.UUID `json:"id"`
	Hostname string    `json:"hostname"`
	// Scope is who owns it: "instance", "organization" or "workspace".
	Scope DomainScope `json:"scope"`
	// IsDefault marks the instance default, the hostname every workspace's links
	// are on today.
	IsDefault bool `json:"is_default"`
	// Verified is the gate. False means no router resolves an alias on this
	// hostname, whoever points DNS at this instance.
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// SSLStatus is what this instance last recorded about the certificate:
	// `none` until verified, `pending` once it will answer Caddy's on-demand
	// ask, `active` once that ask has been answered. It is never more than that,
	// because the app does not speak ACME (decision D3) and the certificate is
	// Caddy's.
	SSLStatus string `json:"ssl_status"`
	// RootRedirectURL is where this hostname's own root sends a visitor. Empty
	// answers 404.
	RootRedirectURL string `json:"root_redirect_url,omitempty"`
	// Verification is the DNS challenge and the state of the last check. Absent
	// on the instance default, which is not verified by anybody.
	Verification *DomainVerification `json:"verification,omitempty"`
	// LinkCount is how many links are on it, which is what deleting one is
	// refused for.
	LinkCount int64 `json:"link_count"`
	// Manageable reports whether *this* actor may rename or delete it. A
	// rendering hint and never authorization: every write re-judges on arrival.
	Manageable bool      `json:"manageable"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// DomainScope names which of D68's three legal ownership states a row is in.
type DomainScope string

const (
	// ScopeInstance is the default domain: both owner columns NULL, shared by
	// every workspace on the instance.
	ScopeInstance DomainScope = "instance"
	// ScopeOrganization is a domain owned by an organization and usable by every
	// workspace in it. Legal in the schema, and nothing registers one at M39 —
	// the surface here is per workspace, which is what the scope row promised.
	ScopeOrganization DomainScope = "organization"
	// ScopeWorkspace is a domain one workspace owns and administers.
	ScopeWorkspace DomainScope = "workspace"
)

// domainScope classifies a row by its two owner columns, which is the same
// enumeration the `domains_ownership_states` CHECK holds.
func domainScope(organizationID, workspaceID *uuid.UUID) DomainScope {
	switch {
	case workspaceID != nil:
		return ScopeWorkspace
	case organizationID != nil:
		return ScopeOrganization
	default:
		return ScopeInstance
	}
}

// canAdminister answers whether the actor may write to a domain with these
// owner columns.
//
// Three limbs, one per legal state:
//
//   - **Workspace-owned** — the actor's own workspace, and nothing else. This is
//     the check M39 exists for: another workspace's admin holds `domains.write`
//     in their own workspace and it buys them nothing here.
//
//   - **Organization-owned** — the actor's own organization. Nothing registers
//     one yet; the limb is here because the CHECK permits the state and a
//     predicate that fell through to "allowed" for a state it did not name is
//     the kind of gap this milestone is about.
//
//   - **The instance default** — `domains.write.instance`, which reaches a person
//     only through `instance_grants` and is held by the instance principal.
//
//     It was `domains.write` until 0.2.0, which migration 00800 grants to the
//     owner and admin *roles*. M39 did not widen that and this limb was
//     unchanged through it — but the reach was real and F70 recorded it: the
//     instance default is the hostname every workspace's links are served on
//     until it registers its own, so on a multi-organization instance every
//     owner and admin could repoint its root and change its bot policy, and
//     under `SIGNUP_MODE=open` one registration reaches that. D38 refused to
//     close it because there was no instance-level principal to name; D98 built
//     one, and D100 moves it there.
func canAdminister(actor *auth.Identity, organizationID, workspaceID *uuid.UUID) bool {
	// The instance default first, because it is the limb that does *not* read
	// the role permission. Checking `domains.write` up front would let a
	// workspace admin past the gate and leave the answer to the switch below,
	// which is the arrangement that made this reachable in the first place.
	if organizationID == nil && workspaceID == nil {
		return actor.Can(auth.PermDomainsWriteInstance)
	}
	if !actor.Can(PermDomainsWrite) {
		return false
	}
	// **The organization limb reads the identity's union, and D44 says it should
	// read the covering membership** (F117). *"A write is authorized by the
	// membership whose scope covers the object being written, not by the
	// identity's union"* — so a workspace-scoped admin's union would reach an
	// organization-owned row here.
	//
	// Latent, and checked in four directions rather than assumed: `CreateDomain`'s
	// only caller writes both owner columns, no update statement touches
	// `domains.workspace_id`, the seeded default row has a NULL organization, and
	// `domains.workspace_id` is `ON DELETE CASCADE`, so deleting a workspace
	// removes the row rather than promoting it to organization-owned. The state
	// is unconstructible through any shipped or migrated path; the CHECK at
	// `02500_domain_ownership.sql` is what legalises it in the schema.
	//
	// Not repaired, because the obvious repair is wrong. Replacing all three
	// limbs with the organization-wide check would take the workspace limb with
	// it, and that one is correctly the union under D31; and `Domains()` calls
	// this per row, so a per-call authority load is a query per row. It becomes
	// real the moment anything registers an organization-owned hostname, and
	// whoever adds that surface has no reason to look here — which is what this
	// comment is for.
	switch {
	case workspaceID != nil:
		return *workspaceID == actor.WorkspaceID
	default:
		return *organizationID == actor.OrgID
	}
}

// Domains lists the hostnames this workspace may use.
//
// Readable by anyone who can read links, like DomainSettings and for the same
// reason: the hostname a link is served on is printed beside every link in the
// product already. Managing one is what needs `domains.write`, and the
// `Manageable` flag on each row says which of them this actor may.
func (s *Service) Domains(ctx context.Context, actor *auth.Identity) ([]Domain, error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading domains requires %s", domain.ErrForbidden, PermRead)
	}
	rows, err := s.q.ListDomains(ctx, dbgen.ListDomainsParams{
		OrganizationID: &actor.OrgID, WorkspaceID: &actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	out := make([]Domain, 0, len(rows))
	for _, r := range rows {
		d := Domain{
			ID: r.ID, Hostname: r.Hostname,
			Scope:      domainScope(r.OrganizationID, r.WorkspaceID),
			IsDefault:  r.IsDefault,
			Verified:   r.VerifiedAt != nil,
			VerifiedAt: r.VerifiedAt,
			SSLStatus:  r.SslStatus,
			LinkCount:  r.LinkCount,
			Manageable: canAdminister(actor, r.OrganizationID, r.WorkspaceID) && !r.IsDefault,
			CreatedAt:  r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
		if r.RootRedirectUrl != nil {
			d.RootRedirectURL = *r.RootRedirectUrl
		}
		// The challenge, on every row the caller may administer. A workspace has
		// to be able to read the record it must publish without pressing anything
		// first, or the page that lists a registered hostname is a page that says
		// "unverified" and offers no way forward.
		d.Verification = s.verificationOf(dbgen.Domain{
			ID: r.ID, Hostname: r.Hostname, IsDefault: r.IsDefault,
			VerifiedAt:               r.VerifiedAt,
			VerificationToken:        r.VerificationToken,
			VerificationCheckedAt:    r.VerificationCheckedAt,
			VerificationFailingSince: r.VerificationFailingSince,
			VerificationError:        r.VerificationError,
		})
		out = append(out, d)
	}
	return out, nil
}

// RegisterDomain records a hostname as belonging to the actor's workspace.
//
// Both owner columns are written, from the one resolved identity: the workspace
// because that is who owns it, and the organization because the workspace
// implies it and the CHECK requires the pair. Reading the organization off the
// actor rather than looking it up again is what makes the pair consistent — they
// come from the same membership resolution.
//
// The hostname is stored **unverified**. Nothing checks whether the person
// registering it controls the name, and nothing here pretends to: that is what
// M40's verification is, and a hostname registered here is not a routing target
// until it happens.
//
// **Bounded per workspace** (M40, reopened). Registration is the cheapest thing
// on this surface and the most expensive one downstream: every row it writes
// becomes a recurring DNS lookup the instance owes, so an unbounded surface let
// one workspace decide how much re-verification everybody else got. See
// domain.MaxDomainsPerWorkspace for the number and what it is bounding.
func (s *Service) RegisterDomain(
	ctx context.Context, actor *auth.Identity, rawHostname string,
) (*Domain, error) {
	if !actor.Can(PermDomainsWrite) {
		return nil, fmt.Errorf("%w: registering a domain requires %s",
			domain.ErrForbidden, PermDomainsWrite)
	}

	hostname, errs := domain.ValidateHostname(rawHostname)
	if len(errs) > 0 {
		return nil, errs
	}
	// The cap is checked before the name is looked up, because what it bounds is
	// the registration and not the name: a workspace at its limit gets the same
	// answer whether or not the hostname it typed is free.
	count, err := s.q.CountWorkspaceDomains(ctx, &actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("count workspace domains: %w", err)
	}
	if count >= domain.MaxDomainsPerWorkspace {
		return nil, domain.ValidationErrors{{
			Field: "hostname", Code: "too_many",
			Message: fmt.Sprintf("a workspace may have at most %d hostnames; every registered "+
				"hostname is re-checked against DNS on a cadence, so this bounds the work one "+
				"workspace can create for the whole instance. Remove one you no longer serve.",
				domain.MaxDomainsPerWorkspace),
		}}
	}
	if taken, err := s.hostnameTaken(ctx, hostname); err != nil {
		return nil, err
	} else if taken {
		return nil, domain.ValidationErrors{hostnameTakenError(hostname)}
	}

	orgID, wsID := actor.OrgID, actor.WorkspaceID
	// The challenge token, minted at registration so the page can print the DNS
	// record to publish the moment somebody adds a hostname (M40). Registration
	// itself proves nothing and still does — the token is what will.
	token := domain.NewVerificationToken()
	row, err := s.q.CreateDomain(ctx, dbgen.CreateDomainParams{
		ID: uuid.Must(uuid.NewV7()), OrganizationID: &orgID, WorkspaceID: &wsID,
		Hostname: hostname, VerificationToken: &token,
	})
	if err != nil {
		// The unique index is the real guarantee; the check above only makes
		// this rare and makes the ordinary refusal a sentence.
		if isUniqueViolation(err) {
			return nil, domain.ValidationErrors{hostnameTakenError(hostname)}
		}
		return nil, fmt.Errorf("register domain: %w", err)
	}

	s.recordDomainEvent(ctx, actor, audit.ActionDomainCreated, row.ID, map[string]any{
		"hostname": row.Hostname,
		// Recorded because it is the thing this milestone added and the thing a
		// reader of the log will be asking about: whose hostname is this.
		"workspace_id": wsID.String(),
	})

	out := domainFromRow(row, 0, true)
	out.Verification = s.verificationOf(row)
	return out, nil
}

// RenameDomain changes a registered hostname.
//
// The hostname is the only field a registration has, and it is changeable only
// because nothing serves it yet — the row's aliases, click history and reserved
// aliases all hang off `domain_id` and are untouched by the name. Once M40
// verifies a hostname, a rename has to invalidate that verification, and the
// bullet that says so belongs to M40 rather than being written here against
// behaviour that does not exist. See decisions.md, D69.
func (s *Service) RenameDomain(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, rawHostname string,
) (*Domain, error) {
	current, err := s.domainForWrite(ctx, actor, id, "renaming")
	if err != nil {
		return nil, err
	}

	hostname, errs := domain.ValidateHostname(rawHostname)
	if len(errs) > 0 {
		return nil, errs
	}
	if hostname != current.Hostname {
		if taken, terr := s.hostnameTaken(ctx, hostname); terr != nil {
			return nil, terr
		} else if taken {
			return nil, domain.ValidationErrors{hostnameTakenError(hostname)}
		}
	}

	// A new token with the new name (M40). The published record lives under the
	// old hostname and proves nothing about this one, so the rename clears
	// verification — the statement does it, and this is the value it re-mints.
	token := domain.NewVerificationToken()
	row, err := s.q.RenameDomain(ctx, dbgen.RenameDomainParams{
		ID: id, Hostname: hostname, VerificationToken: &token,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, domain.ValidationErrors{hostnameTakenError(hostname)}
		}
		return nil, fmt.Errorf("rename domain: %w", err)
	}

	// Both names, because "the domain is now go.example.com" does not tell a
	// reader whether that was a change or which name it replaced, and the
	// previous one is unrecoverable a moment later.
	s.recordDomainEvent(ctx, actor, audit.ActionDomainRenamed, row.ID, map[string]any{
		"from": current.Hostname,
		"to":   row.Hostname,
	})

	// Counted rather than assumed zero. It is zero on every registered hostname
	// today, because nothing serves one — and a response that hard-coded that
	// would start lying the moment M40 makes it false, on the field that decides
	// whether the page offers to remove the domain.
	// Two caches, one write. The verified set is what the host router resolves
	// against, and a renamed domain has just left it; the id-to-hostname map is
	// what short URLs are built from, and it now holds the old name. Both are
	// dropped on every replica, not only this one — which is the bullet D69
	// deferred to this milestone.
	s.hostnames.Delete(row.ID)
	s.invalidateHosts(ctx)

	links, err := s.q.CountLinksOnDomain(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("count links on domain: %w", err)
	}
	out := domainFromRow(row, links, true)
	out.Verification = s.verificationOf(row)
	return out, nil
}

// DeleteDomain removes a registered hostname.
//
// Soft, unlike a folder, and the difference is what the row is. A folder holds
// nothing but a name; a domain is the namespace its links' aliases live in, and
// `links.domain_id` is NOT NULL with no cascade — every click event and every
// reserved alias still points at the row. So the row stays and stops being
// listed.
//
// Refused while any link is on it. Zero links is the only state a registered
// hostname can be in today, because nothing serves one; the guard exists so that
// it is already true when M40 makes the state reachable, rather than being
// remembered then.
func (s *Service) DeleteDomain(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	current, err := s.domainForWrite(ctx, actor, id, "deleting")
	if err != nil {
		return err
	}

	links, err := s.q.CountLinksOnDomain(ctx, id)
	if err != nil {
		return fmt.Errorf("count links on domain: %w", err)
	}
	if links > 0 {
		return domain.ValidationErrors{{
			Field: "hostname", Code: "in_use",
			Message: fmt.Sprintf("%d links are on %s; a hostname cannot be removed while "+
				"links are served on it, because every one of them would stop resolving",
				links, current.Hostname),
		}}
	}

	n, err := s.q.SoftDeleteDomain(ctx, id)
	if err != nil {
		return fmt.Errorf("delete domain: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}

	s.recordDomainEvent(ctx, actor, audit.ActionDomainDeleted, id, map[string]any{
		"hostname": current.Hostname,
	})
	// A removed hostname stops being served everywhere, not only here. It cannot
	// have links on it — the guard above refuses that — but it can have been
	// verified, and a replica that never heard about the removal would go on
	// answering for a name this instance no longer claims.
	s.hostnames.Delete(id)
	s.invalidateHosts(ctx)
	return nil
}

// domainForWrite reads a domain and judges the actor against it, which is the
// one place the ownership rule is applied to a write.
//
// Not found before forbidden: an id that names nothing is a 404 whoever asks,
// because there is nothing to be refused access to. An id that names somebody
// else's hostname is a 403 — see the file comment.
func (s *Service) domainForWrite(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, verb string,
) (dbgen.Domain, error) {
	row, err := s.q.GetDomainByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbgen.Domain{}, domain.ErrNotFound
		}
		return dbgen.Domain{}, fmt.Errorf("read domain: %w", err)
	}
	// The instance default is administered through DomainSettings — its root
	// redirect and its bot policy — and its hostname is a placeholder the
	// resolver never reads, matching on `is_default` instead. Renaming or
	// deleting it here would change a name nothing consults, or take away the
	// hostname every link on the instance is served on.
	//
	// Checked **before** the permission, because it is a fact about the object
	// and not about the actor: nobody may rename or delete this row, the
	// principal included, so answering "you lack a permission" would be telling
	// somebody to go and get one that would not help. It discloses nothing
	// either — the default domain's hostname is printed beside every link in the
	// product. The order moved when D100 gave the default its own permission
	// (F70): before that, every owner and admin passed canAdminister here and
	// reached this refusal anyway, so the two orders were indistinguishable.
	if row.IsDefault {
		return dbgen.Domain{}, domain.ValidationErrors{{
			Field: "hostname", Code: "instance_default",
			Message: "this is the instance's default domain: every workspace's links are " +
				"on it, and it is configured by the operator rather than registered here",
		}}
	}
	if !canAdminister(actor, row.OrganizationID, row.WorkspaceID) {
		return dbgen.Domain{}, fmt.Errorf("%w: %s this domain requires %s in the workspace "+
			"that owns it", domain.ErrForbidden, verb, PermDomainsWrite)
	}
	return row, nil
}

// hostnameTaken reports whether an undeleted domain already holds the name.
func (s *Service) hostnameTaken(ctx context.Context, hostname string) (bool, error) {
	if _, err := s.q.GetDomainByHostname(ctx, hostname); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check hostname: %w", err)
	}
	return true, nil
}

// hostnameTakenError names the collision without saying whose it is.
//
// Deliberately silent about the owner. Registration is open to every workspace
// on the instance, so this message is a way to ask whether a neighbour has
// registered a name; "already registered" answers the question the person
// typing has, and nothing more.
func hostnameTakenError(hostname string) domain.FieldError {
	return domain.FieldError{
		Field: "hostname", Code: "conflict",
		Message: fmt.Sprintf("%s is already registered on this instance; a hostname is "+
			"one alias namespace and cannot belong to two workspaces", hostname),
	}
}

// recordDomainEvent writes the audit record, after the write and outside it.
//
// The same trade every administrative write in this package makes: the change is
// what the actor asked for, and failing it because the record could not be
// written would swap a missing log line for an action that did not happen.
// Logged at warn rather than swallowed, so the gap is visible to whoever goes
// looking.
func (s *Service) recordDomainEvent(
	ctx context.Context, actor *auth.Identity, action string,
	id uuid.UUID, metadata map[string]any,
) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "domain", TargetID: &id, Metadata: metadata,
	}); err != nil {
		s.log.Warn("domain changed but the audit record was not written",
			slog.String("action", action), slog.Any("error", err))
	}
}

func domainFromRow(row dbgen.Domain, linkCount int64, manageable bool) *Domain {
	d := &Domain{
		ID: row.ID, Hostname: row.Hostname,
		Scope:      domainScope(row.OrganizationID, row.WorkspaceID),
		IsDefault:  row.IsDefault,
		Verified:   row.VerifiedAt != nil,
		VerifiedAt: row.VerifiedAt,
		SSLStatus:  row.SslStatus,
		LinkCount:  linkCount,
		Manageable: manageable && !row.IsDefault,
		CreatedAt:  row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.RootRedirectUrl != nil {
		d.RootRedirectURL = *row.RootRedirectUrl
	}
	return d
}
