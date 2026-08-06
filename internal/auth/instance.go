package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The instance-level permissions (D98), named here for the reason
// NonDelegableScopes names slugs that belong to other packages: this is the
// package that resolves an identity, so it is the package that has to know which
// permissions arrive from somewhere other than a membership. The canonical
// constants stay where the feature lives — dispute.PermReview,
// dispute.PermDecide, audit.PermReadInstance — and those packages import this
// one, so the dependency cannot run the other way.
const (
	// PermInstanceAdmin is the principal itself: holding it confers
	// instance-level review on another account, and confers nothing else.
	//
	// It is not in InstanceGrantable below, and that omission is D98's
	// delegation bound made structural — "the principal may grant instance-level
	// review, and a holder of it may not". A principal cannot mint a second
	// principal, so the set of people who may delegate cannot grow, which is the
	// property the constraint exists to protect. Without it the first delegatee
	// appoints the next and the bound is gone in two hops.
	PermInstanceAdmin = "instance.admin"

	// PermDestinationsReview is the reading half of the dispute permission: list
	// the queue and inspect what is in it.
	PermDestinationsReview = "destinations.review"

	// PermDestinationsDecide is the deciding half: allow or uphold, which lifts
	// an entry from the instance-wide blocklist.
	PermDestinationsDecide = "destinations.decide"

	// PermAuditReadInstance reads the audit records of acts that belong to the
	// instance rather than to any tenant.
	PermAuditReadInstance = "audit.read.instance"

	// PermDomainsWriteInstance administers the instance default domain: its root
	// redirect and its bot policy.
	//
	// `domains.write` is a role permission and stays one, because a workspace
	// administering its own registered hostname is M39's whole point. The
	// instance default is not any tenant's — it is the hostname every
	// workspace's links are served on until it registers one — and the guard
	// answered `true` for it on the bare role permission, so on a
	// multi-organization instance every owner and admin could repoint it. Under
	// `SIGNUP_MODE=open` that is one registration away (F70, D100).
	//
	// Named to sort beside `domains.write` for the reason `audit.read.instance`
	// is named beside `audit.read`: the reader comparing the two is the reader
	// this permission is for.
	PermDomainsWriteInstance = "domains.write.instance"
)

// InstancePrincipalScopes is everything the principal holds, enumerated.
//
// Enumerated and not implied, which is D98's own wording and the load-bearing
// part of it: this is not a general instance-administration role. Its reach is
// the three findings that needed it — the dispute queue, the blocklist entries
// those decisions lift, and the instance-wide audit surface — and nothing
// inherits from holding it. A permission added to this list later is a decision
// somebody made, visible in a diff, rather than a consequence of the principal
// existing.
var InstancePrincipalScopes = []string{
	PermInstanceAdmin,
	PermDestinationsReview,
	PermDestinationsDecide,
	PermAuditReadInstance,
	PermDomainsWriteInstance,
}

// InstanceGrantable is what the principal may confer on somebody else.
//
// The dispute queue, both halves. A reviewer who could read but not decide would
// be watching a queue they cannot work, and F15's problem was never that owners
// could decide — it was that every owner on the instance could.
//
// PermAuditReadInstance is deliberately absent as well as PermInstanceAdmin. The
// instance audit surface ties an ip_prefix to a named actor, which is the
// disclosure limb of D18, and D98 gives it to the principal rather than to
// "instance-level review". Widening it is a decision, and this list is where
// somebody would have to make it.
//
// PermDomainsWriteInstance is absent for the same reason (D100). The principal
// administers the instance default domain; conferring *that* is not what D98
// decided the principal may delegate, which was instance-level review of
// disputes and nothing beside it.
var InstanceGrantable = map[string]struct{}{
	PermDestinationsReview: {},
	PermDestinationsDecide: {},
}

// grantInstancePrincipal confers every scope in InstancePrincipalScopes.
//
// Takes a *dbgen.Queries so the caller's transaction owns it: the setup flow
// must not be able to commit an account that claimed the instance without the
// grants that let it administer one.
//
// granted_by is NULL. Nobody conferred this — the instance had no principal a
// moment ago, and naming the new principal as their own grantor would read as a
// self-appointment that never happened.
//
// **Zero rows is an error here, and only here.** The statement selects the
// permission by slug, so a slug this file and the migration disagree about
// confers nothing and reports success — leaving an instance that has been
// claimed and has nobody who can administer it. The account is brand new, so it
// held none of these a moment ago and one row per scope is the only correct
// answer; failing the transaction is what makes the disagreement impossible to
// commit. Elsewhere zero legitimately means "already held", which is why
// instance.Service does not make the same check.
func grantInstancePrincipal(ctx context.Context, q *dbgen.Queries, userID uuid.UUID) error {
	for _, scope := range InstancePrincipalScopes {
		n, err := q.GrantInstancePermission(ctx, dbgen.GrantInstancePermissionParams{
			UserID: userID, Permission: scope,
		})
		if err != nil {
			return fmt.Errorf("confer %s on the instance principal: %w", scope, err)
		}
		if n == 0 {
			return fmt.Errorf(
				"confer %s on the instance principal: no such permission; the migration "+
					"that inserts it and this list have gone out of step", scope)
		}
	}
	return nil
}
