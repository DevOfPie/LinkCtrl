// Package audit records what changed, who changed it, and when.
//
// The table has existed since Phase 1 with nothing writing to it. This package
// is the behavior, and it is built before the five milestones that emit events
// rather than after them: retrofitting emission into a shipped feature means
// touching that feature again, and the first version of the audit trail would
// then be whatever those features happened to record.
//
// Two constraints run through everything here.
//
// An audit record must stay readable after the account it names is gone, so the
// actor is stored twice — actor_user_id for joining while the user exists, and
// actor_label as a snapshot taken at write time, which is what a reader
// actually sees. The column has no foreign key for the same reason.
//
// And it records ip_prefix, never an address. A /24 or /48 answers "was this
// change made from the office network or from somewhere else", which is the
// question an audit trail is read to answer; the remaining bits only identify a
// person. TestEventsRecordAPrefixAndNeverAnAddress holds that line.
package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// PermRead guards the read API.
//
// Deliberately not delegable to an API key; internal/auth's NonDelegableScopes
// is the one place that enforces it. The audit log is where a network prefix is
// tied to a named person, which is a different exposure from every other read
// scope, and a token in a CI variable is the wrong custodian for it.
const PermRead = "audit.read"

// PermReadInstance guards the instance-wide read surface (D98).
//
// A second permission rather than a wider reading of the first, because the rows
// it reaches belong to no organization: an act that changed every tenant is not
// any tenant's to read, and answering it out of audit.read would mean picking an
// organization whose holders get to see it. That is the choice D38 refused and
// D98 replaced with a principal.
//
// Not delegable to a key either, and for audit.read's own reason: these are the
// same columns, carrying the same ip_prefix tied to the same named actors.
const PermReadInstance = "audit.read.instance"

// Actions are the vocabulary of the log. String constants rather than an enum
// type because they are stored verbatim and read by operators, and because
// later milestones add to this list without coordinating with this file.
//
// Dotted noun.verb, matching the permission slugs an operator already reads.
const (
	ActionDomainRootRedirectChanged = "domain.root_redirect_changed"

	// Registering a hostname (M39). The three that M39 exists to record: a
	// domain arriving, being renamed and going away.
	//
	// These are the unowned changes of the phase. Every other administrative
	// action here happens inside one organization and is answerable by asking
	// somebody in it; a hostname is a public name, and "who put this domain on
	// this instance" has, until now, had no answer at all. That is why they are
	// part of the milestone rather than a nicety trimmed from it.
	//
	// Recorded even though nothing is served on the hostname yet. The
	// registration is the act worth a record — by the time M40 verifies it and
	// traffic arrives, the interesting question is who asked for it and when.
	ActionDomainCreated = "domain.created"
	ActionDomainRenamed = "domain.renamed"
	ActionDomainDeleted = "domain.deleted"
	// The verification gate (M40). These two are the only events in this list
	// that a job writes as well as a person: a hostname starts being served when
	// the DNS challenge passes, and stops when it has failed for longer than the
	// grace window. "who started serving links on this name, and when did it
	// stop" is the question a custom domain makes worth asking.
	ActionDomainVerified   = "domain.verified"
	ActionDomainUnverified = "domain.unverified"

	// The invitation lifecycle (M27). Three actions rather than one with a
	// state in the metadata: an operator asking "who let this person in" is
	// reading for an action, which is a thing to grep a page for rather than a
	// query to run.
	//
	// This said the read API "already supports" a filter on `action`. It does
	// not, and never did — the endpoint pages by cursor and takes no action
	// parameter, and there is no index on the column to serve one (F45). The
	// sentence was written a day after that API shipped, which is how a claim
	// about a neighbouring feature comes to be wrong in a comment nobody
	// re-reads. The three-actions-rather-than-one choice stands on its own: a
	// state machine in jsonb is not readable by anything, filter or no filter.
	//
	// The redeemed event is the one with a different actor. Issuing and revoking
	// are recorded against the administrator who did them; redeeming is recorded
	// against the person who joined, because that is who took the action, and it
	// is what makes "invited alice@…, alice@… joined" two records that can be
	// read as one fact.
	ActionInvitationCreated  = "invitation.created"
	ActionInvitationRevoked  = "invitation.revoked"
	ActionInvitationRedeemed = "invitation.redeemed"

	// Team management (M28). The membership actions are the counterpart of the
	// invitation ones: an invitation records how somebody was offered a place,
	// these record what happened to it afterwards, and reading the two together
	// is how "who gave this person admin" is answered months later.
	//
	// member.added is the workspace-scoped grant specifically. Joining by
	// redeeming an invitation is already invitation.redeemed, and recording it
	// twice would make one join look like two.
	ActionMemberAdded       = "member.added"
	ActionMemberRemoved     = "member.removed"
	ActionMemberRoleChanged = "member.role_changed"

	// Tenancy (M28). Deleting a workspace cascades every link in it, so its
	// record is the only trace left of what was there — the metadata carries the
	// name, because the row it names is gone.
	ActionWorkspaceCreated = "workspace.created"
	ActionWorkspaceRenamed = "workspace.renamed"
	ActionWorkspaceDeleted = "workspace.deleted"

	// The organization lifecycle (M28, M28.5). The deletion record is the one
	// that outlives its subject: `audit_logs.organization_id` carries no foreign
	// key, so this row survives the organization it describes, and the metadata
	// carries the name and slug because the row that held them is gone. An audit
	// trail erased by the teardown it records would be the one shape this table
	// must not have.
	ActionOrganizationCreated = "organization.created"
	ActionOrganizationDeleted = "organization.deleted"

	// Destination blocking (M30). The one action here that records something
	// that did *not* happen: every other constant above names a change that was
	// made, and this names a change that was refused.
	//
	// It is recorded anyway, and it is the reason blocking and the audit writer
	// were planned as one pair of milestones. A refusal nobody can count is a
	// policy nobody can tune: the metadata carries the tier and the rule, so
	// "which heuristic is producing all the false positives" is a question the
	// log can answer, and M31's review queue is built on the answer.
	//
	// The metadata's `url_defanged` is the attempted destination, stored inert
	// rather than raw. Defanging happens at the write and not at display,
	// because this table is read verbatim by the audit API and every consumer
	// written after this one would otherwise have to remember.
	ActionDestinationBlocked = "destination.blocked"
)

// The dispute lifecycle (M31).
//
// Declared here rather than in internal/dispute, which is where they lived until
// M45. The vocabulary having two homes meant anything enumerating it from this
// package was silently short by two, and that is not hypothetical: the action
// count in docs/SECURITY.md was wrong twice, at M32.5 and again at 0.2.0, and
// [F18](../../docs/build-notes/deferred-findings.md) is the mechanical cause
// both times. internal/dispute now refers to these rather than declaring its
// own, so there is one list and AllActions can be complete.
//
// The decisions are recorded and the filing is not, and that asymmetry is
// deliberate: a filing's whole record is the dispute row, which carries who and
// when and outlives the account. A decision has an effect *outside* that row —
// an entry gone from the instance-wide blocklist — and the audit log is the only
// place that effect is otherwise visible.
const (
	ActionDisputeAllowed = "dispute.allowed"
	ActionDisputeUpheld  = "dispute.upheld"

	// Bot blocking (M32.5). Two actions because they are two grants: changing a
	// link needs links.update, changing the domain needs domains.write, and the
	// second decides for every link on the instance at once. An operator asking
	// "who made this domain refuse crawlers" is asking a different question from
	// "who turned it on for this link", and one action with a target type would
	// make them the same query.
	//
	// What is deliberately NOT here is the refusal itself. Every blocked request
	// would be a row, and a crawler that finds a blocked link asks for it
	// thousands of times — the growth this table has a warning threshold for. A
	// refusal is traffic and is counted as traffic: a click event with is_bot
	// true, and a metric. These two record the administrative decision that made
	// the refusals happen, which is the thing anyone would later want to ask
	// about.
	ActionLinkBotBlockingChanged   = "link.bot_blocking_changed"
	ActionDomainBotBlockingChanged = "domain.bot_blocking_changed"

	// Webhooks (M42). Registering one is an administrative change with a
	// consequence nothing else in this log has: from that moment the instance
	// makes outbound connections to an address somebody chose, on a schedule
	// nobody is watching. "Who pointed this workspace's events at that host, and
	// when" is therefore a question the log has to answer.
	//
	// The URL is in the metadata, and it is stored **defanged** — the same
	// treatment `destination.blocked` gives an attempted destination, for the
	// same reason: this table is read verbatim by the audit API, and a URL
	// somebody registered in order to be sent workspace events is not a URL a
	// reader should follow by reflex.
	//
	// Rotation is its own action rather than an `updated` with a metadata flag,
	// because it is the one change that invalidates every signature a receiver
	// has already learned to verify, and somebody debugging "our verification
	// broke at 14:03" needs to find it by action name.
	ActionWebhookCreated       = "webhook.created"
	ActionWebhookUpdated       = "webhook.updated"
	ActionWebhookDeleted       = "webhook.deleted"
	ActionWebhookSecretRotated = "webhook.secret_rotated"

	// Automation rules (M43). A rule is a standing instruction the scheduler
	// runs unattended, so the three administrative actions are here for the
	// reason the webhook ones are — somebody has to be able to ask who left this
	// instruction behind, and when.
	ActionAutomationRuleCreated = "automation.rule_created"
	ActionAutomationRuleUpdated = "automation.rule_updated"
	ActionAutomationRuleDeleted = "automation.rule_deleted"

	// ActionAutomationFired is the fourth, and it is the one with no person in
	// it. Every other action in this log was taken by somebody who was signed in
	// or holding a key; this one was taken by a rule, on a clock, and the actor
	// label says so.
	//
	// It is recorded for every firing whatever the actions did, which is what
	// makes "why is this link archived?" answerable. Without it the only trace
	// of an automated archive would be a link whose status changed with nothing
	// in the log beside it, and that is indistinguishable from a bug.
	//
	// **It is deliberately not a trigger source.** domain/automation.go declares
	// this action as something every automation action writes, and no trigger
	// reads it; TestNoAutomationActionWritesATriggerSource is what keeps that
	// true, because a trigger that read the log's automation rows would be the
	// cycle M43 exists to make impossible.
	ActionAutomationFired = "automation.fired"

	// API key rotation (M44). The one action in this log a key takes on itself,
	// and the reason it is recorded is that it is the only administrative change
	// no human is present for.
	//
	// A key rotating itself is not escalation — the successor is identical or
	// narrower, bound to the same workspace — but it is how a credential
	// survives, and D9 accepts openly that a leaked key can rotate itself across
	// generations. That trade is only bearable if the chain is *visible*, which
	// means every link in it appears here with the prefix it came from and the
	// prefix it became. Somebody reading "which credential has been alive in this
	// organization since March" reconstructs it from these records.
	//
	// Minting is not here, and its absence is not an oversight: it requires an
	// interactive session, so the person is the record, and M21 scoped this log
	// to what an operator needs after an incident rather than to everything that
	// happens.
	ActionAPIKeyRotated = "apikey.rotated"

	// One administrator revoking somebody *else's* key.
	//
	// Revoking used to be absent for the reason minting still is — the only
	// person who could revoke a key was the person holding it, so the session
	// was the record. That stopped being true when an organization-wide
	// apikeys.write gained a revoke path over any key in the organization, which
	// it needed because an orphaned or leaked credential otherwise had no
	// answer but its owner's cooperation.
	//
	// Only that arm is recorded. Somebody revoking their own key is still the
	// person being the record; here the credential's owner was not present, may
	// not know, and "who stopped it" is the question afterwards.
	ActionAPIKeyRevoked = "apikey.revoked"

	// One administrator cutting their organization out of somebody else's
	// **account-wide** key (M54).
	//
	// A separate action rather than a flag on the one above, because the two
	// records answer differently and an operator reading either has to know which
	// happened without opening the metadata. `apikey.revoked` says a credential
	// stopped existing. This one says a credential this organization can no
	// longer be reached by is still working for its owner somewhere else — which
	// is the honest description of what an administrator may do to a key minted
	// by an account they hold no authority over, and the reason the outright
	// revoke is refused for one.
	//
	// It is also the distinction the incident question turns on. "Was that key
	// stopped" has two answers now, and a vocabulary that flattened them would
	// leave somebody believing a leaked credential was dead when it is merely
	// elsewhere.
	//
	//nolint:gosec // G101: an audit action slug, not a credential. The other
	// entries escape the heuristic only by not containing "revoked" beside a
	// word it reads as secret-shaped.
	ActionAPIKeyReachRevoked = "apikey.reach_revoked"

	// A password recovered from a mailbox (M51).
	//
	// The first action in this vocabulary with **no organization**, and that is
	// the fact worth stating rather than an implementation detail. Every other
	// entry is written by somebody standing in a tenant; this one is written by
	// somebody holding no credential at all — that is what recovery is — so
	// there is no workspace they were in and no honest way to choose among the
	// organizations their account may belong to. The row lands with a NULL
	// `organization_id` and is read through `audit.read.instance`, which is the
	// right audience for "an account on this box was recovered".
	//
	// Only the recovery is here. An ordinary password change is not, and its
	// absence is the same one minting a key has: the person held a session, so
	// the session is the record. A reset is the case where they did not, and
	// "who set this password, and from what network" is the question afterwards.
	ActionPasswordReset = "password.reset"

	// An account deleted by the person who owns it (M52).
	//
	// **Instance-wide, for the reason `password.reset` above is**, and the
	// reasoning is F36's rather than a new one. An account is not a tenant: it
	// may belong to several organizations, none of which it is *about*, and
	// filing this under whichever one the person happened to be standing in
	// would be the misattribution F36 names — visible to one tenant with no
	// claim to it, invisible to the others it changed. An account that belongs
	// to nothing at all, which is the ordinary state to delete from once every
	// solely-owned organization has been handed over, would have no
	// organization to be filed under in the first place.
	//
	// **It is written inside the deleting transaction**, which no other action
	// in this vocabulary is, and the reason is specific to this one: the actor
	// *is* the subject. A record written after the commit sits in the window
	// between deletion and erasure carrying the account's address, and if the
	// hourly sweep lands in that window the address stays in `audit_logs`
	// forever, because `anonymized_at` is by then already set. Record's own
	// documentation contemplated a caller inside a transaction; RecordTx is
	// what finally gives one a way to join it.
	ActionAccountDeleted = "account.deleted"

	// The second factor's four lifecycle events (M53).
	//
	// **Instance-wide, on the reasoning `password.reset` and `account.deleted`
	// already established.** A second factor is a property of a person, not of a
	// tenant: the account may belong to several organizations or to none, and
	// filing "this person now has a second factor" under whichever workspace they
	// were standing in is F36's misattribution again. `audit.read.instance` is the
	// audience, and it is the right one — who on this box can be signed in as, and
	// what it takes, is the operator's question.
	//
	// **Four rather than two**, and the two beyond what m53.md names by name are
	// here for the same reason those two are. Enabling is the moment an account
	// stops being reachable by password alone; regenerating voids ten standing
	// credentials at once. Both are credential-lifecycle changes with the same
	// audience as the two the milestone spells out, and a vocabulary that recorded
	// the removal of a factor but not its arrival would answer half of the only
	// question it is read for.
	ActionMFAEnabled  = "mfa.enabled"
	ActionMFADisabled = "mfa.disabled"

	// A recovery code spent at the sign-in prompt.
	//
	// The one m53.md asks for by name, and the reason it asks: a recovery code
	// being spent is the signal that either the phone is gone or somebody else has
	// it. The actor is the account itself — there is no session at the prompt and
	// nobody else was present — which is the attribution `password.reset` makes
	// for the same situation.
	ActionMFARecoveryCodeUsed = "mfa.recovery_code_used"

	ActionMFARecoveryCodesRegenerated = "mfa.recovery_codes_regenerated"
)

// ActionSessionMintedByAddon is a session minted on an add-on's word (M65).
//
// The one action in this vocabulary whose *authority* is not this product's. Every
// other record here says a person or a key did something; this one says a module
// an operator installed vouched for somebody and the host believed it, which is a
// different fact and is why it is a different action rather than a metadata key on
// something existing.
//
// Two events under one action, told apart by `second_factor_required` in the
// metadata: the mint that produced a session, and the one that stopped at the
// second-factor prompt. Both are worth a record — the second is where an operator
// sees that a provider is asserting identities whose accounts then fail to
// complete — and splitting them into two actions would put the same question in
// two places.
const ActionSessionMintedByAddon = "session.minted_by_addon"

// The add-on lifecycle (M67).
//
// Declared here and not in internal/addon, for the reason the dispute comment
// above gives: a vocabulary with two homes makes everything that enumerates it
// from this package short by however many live elsewhere, which is F18 and is why
// the count in docs/SECURITY.md was wrong twice. These arrived in internal/addon
// and moved before the milestone landed.
//
// Instance-wide, and not by convention: an add-on is installed once for the whole
// box and belongs to no organization, so filing the record under whichever tenant
// the principal happened to be standing in is the misattribution F36 names.
//
// Two actions rather than one with a direction in the metadata, because they are
// two different questions an operator asks — what has been put on this box, and
// what has been taken off it — and the second is the one asked after an incident.
const (
	ActionAddonInstalled = "addon.installed"
	ActionAddonRemoved   = "addon.removed"
)

// Event is one thing that happened.
//
// The actor is not a field: it is taken from the *auth.Identity passed to
// Record, so a caller cannot record an action against somebody else by
// accident. Everything here describes the change, not who made it.
type Event struct {
	// Action is what happened, from the constants above.
	Action string
	// TargetType and TargetID name the object, when there is one. A setting
	// that belongs to the instance rather than to a row leaves them empty.
	TargetType string
	TargetID   *uuid.UUID
	// Metadata is the detail that varies by action — old and new values, the
	// reason for a refusal. jsonb, per the rule every Phase 2 milestone
	// inherits: a column per action would be a migration per action.
	//
	// Never put a secret or a full IP address in here. It is returned verbatim
	// by the read API.
	Metadata map[string]any
	// OccurredAt defaults to now. Set explicitly only by tests and by a
	// backfill, and it is the partition key, so a value outside every existing
	// partition lands in the default one.
	OccurredAt time.Time
	// InstanceWide marks an act that belongs to the instance rather than to the
	// actor's tenant: the record is written with a NULL organization_id and a
	// NULL workspace_id, whoever made it and wherever they were acting.
	//
	// F36 is what this is for. Changing the default domain's bot policy enforces
	// it on every link in every organization, and the record went into exactly
	// one of them — the actor's — where the tenants it changed could not see it
	// and the tenant it landed in had no particular claim to it. The same is true
	// of a dispute decision, which deletes a row from the instance-wide
	// blocklist.
	//
	// The column was always nullable and the product already used a NULL
	// organization to mean instance-wide for the default `domains` row, so this
	// is that existing convention arriving in the table that describes it. What
	// D98 adds is somebody who can read the result: audit.read.instance, held by
	// the principal. Before that there was nowhere honest to put these rows,
	// which is why F36 sat open rather than being a one-line fix.
	//
	// A field on the event, not a property of the actor, because the same actor
	// makes both kinds of change in the same session.
	InstanceWide bool
}

// Entry is an audit record as a reader sees it.
type Entry struct {
	ID         uuid.UUID  `json:"id"`
	OccurredAt time.Time  `json:"occurred_at"`
	ActorLabel string     `json:"actor_label"`
	ActorID    *uuid.UUID `json:"actor_id,omitempty"`
	// ActorAPIKeyID is set when the action was taken with an API key. Present
	// so a reader can tell an interactive change from an automated one, which
	// is most of the value of an audit log during an incident.
	ActorAPIKeyID *uuid.UUID `json:"actor_api_key_id,omitempty"`
	Action        string     `json:"action"`
	TargetType    string     `json:"target_type,omitempty"`
	TargetID      *uuid.UUID `json:"target_id,omitempty"`
	// WorkspaceID is the workspace the action happened in, when it happened in
	// one. Absent on organization-level actions, which is most of the
	// invitation and membership vocabulary.
	//
	// Returned since 0.2.0. It was stored, indexed, selected and scanned before
	// that and then dropped on the way out (F110) — an undocumented second
	// choice riding on a documented first one, since the query explains at
	// length why the read is not *filtered* by workspace and says nothing about
	// not returning it. Without it a reader cannot tell which workspace a
	// link-scoped action such as `link.bot_blocking_changed` came from: those
	// actions name the link as their target and nothing else, where
	// `workspace.*` actions carry it as `target_id` and are readable already.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
	// IPPrefix is a network, never an address: /24 for IPv4, /48 for IPv6.
	IPPrefix string         `json:"ip_prefix,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Filter is a page request. Org scope comes from the actor, not from here:
// letting a caller name the organization would make this an authorization
// decision taken by the caller.
type Filter struct {
	Cursor string
	Limit  int32
}

// Service writes and reads audit records.
type Service struct {
	q *dbgen.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{q: dbgen.New(pool)}
}

// Recorder is the writing half, as its callers see it.
//
// An interface so a service can take a nil-able dependency and so the emission
// tests do not need a database. The link service holds one of these, not the
// concrete type.
type Recorder interface {
	Record(ctx context.Context, actor *auth.Identity, e Event) error
}

// Record writes one event.
//
// No permission check, and that is deliberate: an audit event is a consequence
// of an action that was already authorized, not an action of its own. Gating it
// would mean an actor who can change a setting but not "write audit" could
// change it silently, which is exactly backwards.
//
// The error is returned rather than swallowed so the caller decides. A caller
// inside a transaction should fail the whole operation; a caller outside one
// generally logs and continues, because losing the change is worse than losing
// the record of it.
func (s *Service) Record(ctx context.Context, actor *auth.Identity, e Event) error {
	return s.RecordTx(ctx, s.q, actor, e)
}

// RecordTx writes one event through the caller's own handle.
//
// The same function as Record — Record is this with the service's pool-backed
// Queries — and it exists because M52 produced the first operation whose actor
// the operation itself destroys. Everything else in this tree records *after* it
// commits, deliberately: losing the change is worse than losing the record of
// it. Account deletion cannot, because the erasure sweep scrubs the address out
// of `audit_logs` by reading `users.anonymized_at`, and a record inserted after
// the commit can arrive after the sweep has already been past. One transaction
// makes the ordering a fact rather than a probability.
//
// The cost is the one Record's own documentation named: inside a transaction a
// failed write fails the whole operation. That is the right answer here — an
// account deletion nobody can find afterwards is worse than a deletion that did
// not happen — and it is the wrong answer nearly everywhere else, which is why
// this is a second entry point rather than a change to the first.
func (s *Service) RecordTx(
	ctx context.Context, q *dbgen.Queries, actor *auth.Identity, e Event,
) error {
	if e.Action == "" {
		return errors.New("audit: event has no action")
	}

	at := e.OccurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}

	// Marshalled here rather than left to the driver so a metadata value that
	// cannot be encoded fails loudly at the call site instead of writing a
	// record whose detail is missing.
	metadata := []byte("{}")
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("audit: encode metadata for %s: %w", e.Action, err)
		}
		metadata = b
	}

	params := dbgen.InsertAuditLogParams{
		// v7, so the primary key is time-ordered within a partition and the
		// keyset tiebreak on (occurred_at, id) is stable rather than random.
		ID:         uuid.Must(uuid.NewV7()),
		OccurredAt: at,
		ActorLabel: actorLabel(actor),
		Action:     e.Action,
		TargetID:   e.TargetID,
		Metadata:   metadata,
	}
	if e.TargetType != "" {
		params.TargetType = &e.TargetType
	}
	if actor != nil {
		// The tenancy columns are the actor's, except when the act was not the
		// tenant's. An instance-wide event leaves both NULL — the actor is still
		// recorded in full, because who did it is exactly the question, but the
		// change belongs to the instance and stamping it with whichever
		// organization the person happened to be standing in is the
		// misattribution F36 names.
		if !e.InstanceWide {
			if actor.OrgID != uuid.Nil {
				org := actor.OrgID
				params.OrganizationID = &org
			}
			if actor.WorkspaceID != uuid.Nil {
				ws := actor.WorkspaceID
				params.WorkspaceID = &ws
			}
		}
		if actor.UserID != uuid.Nil {
			uid := actor.UserID
			params.ActorUserID = &uid
		}
		params.ActorApiKeyID = actor.APIKeyID
	}
	// The one place an address becomes a prefix. Taking the address from the
	// context and reducing it here means no caller ever holds a full address
	// destined for this table, so no caller can forget to reduce it.
	if prefix := auth.AnonymizeIP(auth.ClientIPFrom(ctx)); prefix != "" {
		params.IpPrefix = &prefix
	}

	if err := q.InsertAuditLog(ctx, params); err != nil {
		return fmt.Errorf("audit: write %s: %w", e.Action, err)
	}
	return nil
}

// RecordAPIKeyRotation satisfies auth.APIKeyAuditor.
//
// It lives here rather than being a plain Record call from the key service
// because the dependency runs one way: this package imports internal/auth, to
// resolve an actor into the label it stores, so internal/auth cannot import
// this one. auth declares the narrow interface it needs and this is the
// implementation on the other side of that seam.
//
// Only prefixes go into the metadata. The prefix is the public half of a token
// by construction — it is stored, indexed and shown in the key list — and the
// secret is not in the row this is written from, so there is nothing here for
// the "never put a secret in metadata" rule to catch.
func (s *Service) RecordAPIKeyRotation(
	ctx context.Context, actor *auth.Identity, ev auth.APIKeyRotation,
) error {
	predecessor := ev.PredecessorID
	return s.Record(ctx, actor, Event{
		Action:     ActionAPIKeyRotated,
		TargetType: "api_key",
		// The **predecessor** is the target. Rotation is something that happened
		// to the key being replaced: it acquired a deadline. The successor is
		// named in the metadata, which is what lets a reader walk the chain
		// forwards from any generation.
		TargetID: &predecessor,
		Metadata: map[string]any{
			"prefix":           ev.PredecessorPrefix,
			"successor_id":     ev.SuccessorID.String(),
			"successor_prefix": ev.SuccessorPrefix,
			"grace_expires_at": ev.GraceExpiresAt.UTC().Format(time.RFC3339),
			"scopes":           ev.Scopes,
			"scopes_narrowed":  ev.ScopesNarrowed,
			"org_wide":         ev.OrgWide,
		},
	})
}

// RecordAPIKeyRevocation satisfies auth.APIKeyAuditor, and is on this side of
// the same seam RecordAPIKeyRotation is.
//
// The owner is in the metadata rather than only in the target, because the
// target is the key and the question this record answers is whose credential
// stopped working. The actor — the administrator — comes from the identity, as
// it does for every record.
func (s *Service) RecordAPIKeyRevocation(
	ctx context.Context, actor *auth.Identity, ev auth.APIKeyRevocation,
) error {
	keyID := ev.KeyID
	return s.Record(ctx, actor, Event{
		Action:     ActionAPIKeyRevoked,
		TargetType: "api_key",
		TargetID:   &keyID,
		Metadata: map[string]any{
			"prefix":   ev.Prefix,
			"owner_id": ev.OwnerID.String(),
		},
	})
}

// RecordAPIKeyReachRevocation satisfies auth.APIKeyAuditor, and is the M54 half
// of the method above.
//
// The organization is not in the metadata: every record already carries the
// organization it was written in, and that is exactly the organization cut out
// of the key's reach. Repeating it would be a second copy of the same fact,
// which is how the two come to disagree.
func (s *Service) RecordAPIKeyReachRevocation(
	ctx context.Context, actor *auth.Identity, ev auth.APIKeyRevocation,
) error {
	keyID := ev.KeyID
	return s.Record(ctx, actor, Event{
		Action:     ActionAPIKeyReachRevoked,
		TargetType: "api_key",
		TargetID:   &keyID,
		Metadata: map[string]any{
			"prefix":   ev.Prefix,
			"owner_id": ev.OwnerID.String(),
		},
	})
}

// RecordMFAChange satisfies auth.MFAAuditor, and is on this side of the same
// seam RecordAPIKeyRotation is (M53).
//
// It exists here rather than as a plain Record call from internal/auth because
// the dependency runs one way: this package imports internal/auth to resolve an
// actor into the label it stores, so internal/auth cannot import this one. auth
// declares the narrow interface it needs and this is the implementation.
//
// **The metadata carries no secret and no code.** Not the TOTP secret, not a
// recovery code, not a hash of one — the rule the whole vocabulary is held to, and
// worth restating here because this is the first action whose subject *is* a
// secret. What a reader gets is how many recovery codes are left, which is the one
// number that changes an operator's answer to "should I be worried about this
// account".
//
// The target is the account, `TargetType` "user", which is what
// `password.reset` and `account.deleted` already use for the same subject.
func (s *Service) RecordMFAChange(
	ctx context.Context, actor *auth.Identity, ev auth.MFAChange,
) error {
	action, ok := mfaActions[ev.Kind]
	if !ok {
		return fmt.Errorf("audit: unknown second-factor change %q", ev.Kind)
	}
	userID := ev.UserID
	return s.Record(ctx, actor, Event{
		Action:     action,
		TargetType: "user",
		TargetID:   &userID,
		Metadata: map[string]any{
			"recovery_codes_remaining": ev.RecoveryCodesRemaining,
		},
		InstanceWide: true,
	})
}

// RecordAddonSessionMint satisfies auth.SessionAuditor.
//
// It lives here for the reason RecordAPIKeyRotation and RecordMFAChange do: this
// package imports internal/auth to resolve an actor into the label it stores, so
// internal/auth cannot import this one, and the narrow interface is declared on
// that side.
//
// **The metadata carries provenance and no identity.** `minted_by` is the string
// m65.md names — `addon:<name>` — and `issuer` is the provider as it named itself.
// The external *subject*, the address the assertion carried and the display name
// are all deliberately absent: the erasure sweep scrubs this column by the keys it
// knows about, its coverage was counted site by site when F177 closed, and a
// writer that put a person's provider identifier here would be a site the sweep
// does not reach. `addon` repeats the name outside the prefixed label so that a
// reader filtering by add-on does not have to know how the label is spelled.
func (s *Service) RecordAddonSessionMint(
	ctx context.Context, actor *auth.Identity, ev auth.AddonSessionMint,
) error {
	var target *uuid.UUID
	if actor != nil && actor.UserID != uuid.Nil {
		id := actor.UserID
		target = &id
	}
	return s.Record(ctx, actor, Event{
		Action:     ActionSessionMintedByAddon,
		TargetType: "user",
		TargetID:   target,
		Metadata: map[string]any{
			"minted_by":              ev.MintedBy,
			"addon":                  ev.Addon,
			"issuer":                 ev.Issuer,
			"second_factor_required": ev.SecondFactorRequired,
		},
	})
}

// mfaActions maps the seam's vocabulary onto this package's.
//
// A map rather than a switch with a default, so a kind added on the other side of
// the seam and not here is an error at the call site instead of a record filed
// under whichever action the default picked.
var mfaActions = map[auth.MFAChangeKind]string{
	auth.MFAEnabled:                  ActionMFAEnabled,
	auth.MFADisabled:                 ActionMFADisabled,
	auth.MFARecoveryCodeUsed:         ActionMFARecoveryCodeUsed,
	auth.MFARecoveryCodesRegenerated: ActionMFARecoveryCodesRegenerated,
}

// actorLabel is the snapshot stored beside the actor's id.
//
// Email, because that is how a person is identified everywhere else in this
// product and it stays meaningful after the account is deleted. Falling back to
// the display name, then to a fixed string: a record with no readable actor is
// still a record that something happened, and dropping the event instead would
// lose more than it protects.
//
// **What this writes is not the last word on it.** M52's erasure sweep replaces
// the label of an erased actor with a constant tombstone — internal/account's
// TombstoneLabel — so what a reader sees is the address until the account is
// deleted and swept, and a fixed string afterwards. Correlating one erased
// actor's entries is `actor_user_id`, which erasure leaves alone (D148); this
// function is why there is anything to replace rather than a row to delete.
//
// It said *"if the account is ever deleted"* between M45 and M52, and that was
// correct at the time: no path in this product deleted a user, `users` appeared
// in none of the schema's DELETE statements, and neither `deleted_at` nor
// `anonymized_at` had a writer (F44). The snapshot was the right design for a
// lifecycle nobody had built; M52 built it.
func actorLabel(actor *auth.Identity) string {
	if actor == nil {
		return "system"
	}
	if actor.Email != "" {
		return actor.Email
	}
	if actor.Name != "" {
		return actor.Name
	}
	return "unknown"
}

// defaultPageLimit and maxPageLimit bound a page. The same shape as the link
// list, so a client that already paginates one surface paginates this one.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// List returns a page of the audit records the actor's own authority covers in
// their organization, newest first.
//
// Two questions, deliberately asked separately. `Can` answers whether they may
// read the audit log at all — the ordinary permission check every other endpoint
// makes. The authority load answers *which rows*, and it is the repair for F31:
// a workspace-scoped admin resolves audit.read while acting in their workspace,
// and until now that read the whole organization, including workspaces where
// they hold no membership at all. `auth.MembershipAuthority` already states the
// rule for writes — a workspace-scoped membership grants authority over its own
// workspace, not over the organization (D44) — and this is the same rule
// reaching a read.
//
// An organization-wide membership is unaffected in either direction: it reaches
// the organization-wide scope, so it still reads every row, which is what
// audit.sql argues for and what M21 shipped.
func (s *Service) List(ctx context.Context, actor *auth.Identity, f Filter) (*domain.Page[Entry], error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading the audit log requires %s", domain.ErrForbidden, PermRead)
	}

	authority, err := auth.LoadMembershipAuthority(ctx, s.q, actor.UserID, actor.OrgID, PermRead)
	if err != nil {
		return nil, err
	}
	orgWide, workspaces := authority.Scopes()

	limit := f.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	org := actor.OrgID
	params := dbgen.ListAuditLogsParams{
		OrganizationID: &org,
		OrgWide:        orgWide,
		// Never nil: a nil uuid[] parameter reaches Postgres as NULL, and
		// `workspace_id = ANY(NULL)` is NULL rather than false, which would make
		// the whole predicate unknown and return nothing at all. An empty array
		// is the honest encoding of "no workspace grants it", and it is
		// unreachable in practice — Can already refused an actor who holds the
		// permission nowhere — which is exactly why it must not be left to be
		// discovered later.
		WorkspaceIds: workspaces,
		// One extra row answers "is there another page" without a second query
		// and without a count over a table designed to grow forever.
		PageLimit: limit + 1,
	}
	if workspaces == nil {
		params.WorkspaceIds = []uuid.UUID{}
	}
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		params.CursorOccurred = &cur.OccurredAt
		params.CursorID = &cur.ID
	}

	rows, err := s.q.ListAuditLogs(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	return pageOf(rows, limit), nil
}

// ListInstance returns a page of the instance-wide audit records — the ones that
// belong to no organization — newest first.
//
// The other half of F36. Marking an act instance-wide keeps it out of a tenant's
// log where it never belonged; this is where it goes instead, and without it the
// repair would have been a deletion rather than a correction. Gated on
// audit.read.instance, which only the instance principal holds (D98).
//
// Its own cursor namespace, because it is its own ordering: a cursor from the
// organization log names a position in a different result set, and the two are
// never mixed.
func (s *Service) ListInstance(
	ctx context.Context, actor *auth.Identity, f Filter,
) (*domain.Page[Entry], error) {
	if !actor.Can(PermReadInstance) {
		return nil, fmt.Errorf("%w: reading the instance audit log requires %s",
			domain.ErrForbidden, PermReadInstance)
	}

	limit := f.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	params := dbgen.ListInstanceAuditLogsParams{PageLimit: limit + 1}
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		params.CursorOccurred = &cur.OccurredAt
		params.CursorID = &cur.ID
	}

	rows, err := s.q.ListInstanceAuditLogs(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list instance audit log: %w", err)
	}
	return pageOf(rows, limit), nil
}

// pageOf turns one over-fetched row set into a page. Shared by the two lists so
// the has-more and cursor arithmetic exists once; both queries select the same
// columns and sqlc gives both the same row type.
func pageOf(rows []dbgen.AuditLog, limit int32) *domain.Page[Entry] {
	page := &domain.Page[Entry]{Items: make([]Entry, 0, limit)}
	if len(rows) > int(limit) {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, toEntry(r))
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.OccurredAt, last.ID)
	}
	return page
}

func toEntry(r dbgen.AuditLog) Entry {
	e := Entry{
		ID:            r.ID,
		OccurredAt:    r.OccurredAt,
		ActorLabel:    r.ActorLabel,
		ActorID:       r.ActorUserID,
		ActorAPIKeyID: r.ActorApiKeyID,
		Action:        r.Action,
		TargetID:      r.TargetID,
		WorkspaceID:   r.WorkspaceID,
	}
	if r.TargetType != nil {
		e.TargetType = *r.TargetType
	}
	if r.IpPrefix != nil {
		e.IPPrefix = *r.IpPrefix
	}
	// A record whose metadata does not decode is still returned, without it.
	// The alternative — failing the page — would let one malformed row hide
	// every event around it, which is the opposite of what this API is for.
	if len(r.Metadata) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Metadata, &m); err == nil && len(m) > 0 {
			e.Metadata = m
		}
	}
	return e
}

// cursor is a position in the (occurred_at DESC, id DESC) ordering.
//
// Version-prefixed and fixed-arity, like the link cursor: a cursor minted by an
// older build decodes to the wrong number of fields and is refused, rather than
// being read as a position it does not describe. There is only one ordering
// here, so unlike the link cursor it does not carry a sort.
type cursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
}

func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join([]string{
		"1", at.UTC().Format(time.RFC3339Nano), id.String(),
	}, "|")))
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != "1" {
		return cursor{}, errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return cursor{}, err
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return cursor{}, err
	}
	return cursor{OccurredAt: at, ID: id}, nil
}

// AllActions is every action this package declares, and it is exhaustive.
//
// It exists because the vocabulary is enumerated by things outside the code —
// docs/SECURITY.md states a coverage count, and a reader checks a claim like
// that by counting. That count has been wrong twice: it read twelve until M32.5
// while omitting destination.blocked, and eighteen until 0.2.0 while the list
// had grown past it. Both times the number was maintained by hand beside a list
// that was not.
//
// TestAllActionsIsExhaustive parses this file and fails if a constant is
// declared without appearing here, so adding an action and forgetting this list
// is a failing build rather than a documentation defect discovered two
// milestones later. The list is written out rather than derived at runtime
// because reflection cannot see constants, and a slice somebody has to keep in
// step is only safe if something checks.
//
// Ordered as declared, which groups them by subsystem the way the constants are
// grouped; nothing depends on the order.
func AllActions() []string {
	return []string{
		ActionDomainRootRedirectChanged,
		ActionDomainCreated,
		ActionDomainRenamed,
		ActionDomainDeleted,
		ActionDomainVerified,
		ActionDomainUnverified,
		ActionInvitationCreated,
		ActionInvitationRevoked,
		ActionInvitationRedeemed,
		ActionMemberAdded,
		ActionMemberRemoved,
		ActionMemberRoleChanged,
		ActionWorkspaceCreated,
		ActionWorkspaceRenamed,
		ActionWorkspaceDeleted,
		ActionOrganizationCreated,
		ActionOrganizationDeleted,
		ActionDestinationBlocked,
		ActionDisputeAllowed,
		ActionDisputeUpheld,
		ActionLinkBotBlockingChanged,
		ActionDomainBotBlockingChanged,
		ActionWebhookCreated,
		ActionWebhookUpdated,
		ActionWebhookDeleted,
		ActionWebhookSecretRotated,
		ActionAutomationRuleCreated,
		ActionAutomationRuleUpdated,
		ActionAutomationRuleDeleted,
		ActionAutomationFired,
		ActionAPIKeyRotated,
		ActionAPIKeyRevoked,
		ActionAPIKeyReachRevoked,
		ActionPasswordReset,
		ActionAccountDeleted,
		ActionMFAEnabled,
		ActionMFADisabled,
		ActionMFARecoveryCodeUsed,
		ActionMFARecoveryCodesRegenerated,
		ActionSessionMintedByAddon,
		ActionAddonInstalled,
		ActionAddonRemoved,
	}
}
