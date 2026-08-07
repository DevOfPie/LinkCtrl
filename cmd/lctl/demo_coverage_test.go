//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// What the demo instance must show, enumerated (M33.5).
//
// This list is the mechanism the milestone is about. `lctl demo` was written when
// a workspace and twenty links were the whole product, and it stayed that way
// through nine milestones of Phase 2 — not because anybody decided the demo
// should under-sell the phase, but because nothing failed when it did. A rule
// asking people to remember produced exactly one outcome, and this is the
// replacement.
//
// **A milestone that ships something a person can see extends the seeder and
// adds a row here.** That obligation is real and it is deliberate: the build
// fails otherwise. If it ever proves too heavy, the answer is to narrow what
// this list covers, in writing, with the reasoning recorded — never to delete
// the test. A demo nobody enforces rots back to twenty links, and the second
// time it happens nobody will notice for another nine milestones.
//
// Rows are ordered by the milestone they belong to, so the boundary at the
// bottom — what is deliberately *not* seeded yet — reads as the end of a list
// rather than as an omission.

// demoFeature is one thing the demo must show, and the query that proves it does.
type demoFeature struct {
	// Milestone is the number that shipped the feature, so a failure says which
	// page went empty rather than only which query returned zero.
	Milestone string
	// Feature is what somebody opening the demo would be looking at.
	Feature string
	// Query counts the seeded rows. $1 is the demo organization; $2, where the
	// query uses it, is its owner.
	Query string
	// Min is the count below which the feature is not being shown.
	Min int64
	// Max bounds it. Zero means unbounded, except where MaxIsZero says the
	// feature must produce no rows at all.
	Max int64
	// MaxIsZero marks a row whose whole claim is that nothing is seeded — the
	// boundary rows for milestones not yet built.
	MaxIsZero bool
	// Shows is what the demo loses when this row fails, in the words of somebody
	// looking at the instance rather than at the schema.
	Shows string
}

// demoWorkspaces is the scope every query below is written against: the demo
// organization's workspaces, which is what a person signed in as the owner can
// reach.
const demoWorkspaces = `SELECT id FROM workspaces WHERE organization_id = $1`

// quotedScopes renders a slug list as a SQL IN body.
//
// Deliberately not a hand-written string in the row above: the set it renders is
// auth.InstancePrincipalScopes, and a copy of it written here would be short by
// one the moment a scope is added — exactly what happened when D100 added
// domains.write.instance to the four this assertion used to name. The slugs are
// compile-time constants in this repository, so quoting is a formality rather
// than an injection defence, and it is done anyway because a helper that builds
// SQL should not depend on that staying true.
func quotedScopes(scopes []string) string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, "'"+strings.ReplaceAll(s, "'", "''")+"'")
	}
	return strings.Join(out, ", ")
}

func demoCoverage() []demoFeature {
	return []demoFeature{
		{
			Milestone: "M8", Feature: "A catalogue of links",
			Query: `SELECT count(*) FROM links WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   20,
			Shows: "the link list, which is the first page anybody opens",
		},
		{
			Milestone: "M9", Feature: "Click history",
			Query: `SELECT count(*) FROM click_events WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   1000,
			Shows: "every chart on the analytics pages",
		},
		{
			Milestone: "M9", Feature: "Rolled-up analytics",
			Query: `SELECT count(*) FROM link_click_daily WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   50,
			Shows: "the daily series, which reads from the rollup rather than the events",
		},
		{
			Milestone: "M21", Feature: "An audit trail spanning several actions",
			Query: `SELECT count(DISTINCT action) FROM audit_logs WHERE organization_id = $1`,
			Min:   5,
			Shows: "the audit page as a trail rather than as one root-redirect change",
		},
		{
			Milestone: "M22", Feature: "Notifications, some unread",
			Query: `SELECT count(*) FROM notifications
			         WHERE user_id = $2 AND read_at IS NULL
			           AND (workspace_id IS NULL
			                OR workspace_id IN (` + demoWorkspaces + `))`,
			Min:   1,
			Shows: "the notification bell with a count on it",
		},
		{
			Milestone: "M22", Feature: "Notifications already read",
			Query: `SELECT count(*) FROM notifications
			         WHERE user_id = $2 AND read_at IS NOT NULL
			           AND (workspace_id IS NULL
			                OR workspace_id IN (` + demoWorkspaces + `))`,
			Min:   1,
			Shows: "that read and unread are two states, not one empty list",
		},
		{
			Milestone: "M25", Feature: "A second workspace",
			Query: `SELECT count(*) FROM workspaces
			         WHERE organization_id = $1 AND deleted_at IS NULL`,
			Min:   2,
			Shows: "the workspace switcher, which renders nothing when there is one",
		},
		{
			Milestone: "M25", Feature: "Links in every workspace",
			Query: `SELECT count(*) FROM workspaces w
			         WHERE w.organization_id = $1 AND w.deleted_at IS NULL
			           AND EXISTS (SELECT 1 FROM links l WHERE l.workspace_id = w.id)`,
			Min:   2,
			Shows: "that scoping is observable: switching changes what the list holds",
		},
		{
			Milestone: "M27", Feature: "An outstanding invitation",
			Query: `SELECT count(*) FROM invitations
			         WHERE organization_id = $1 AND redeemed_at IS NULL
			           AND revoked_at IS NULL AND expires_at > now()`,
			Min:   1,
			Shows: "the pending half of the invitations page",
		},
		{
			Milestone: "M27", Feature: "A redeemed invitation",
			Query: `SELECT count(*) FROM invitations
			         WHERE organization_id = $1 AND redeemed_at IS NOT NULL`,
			Min:   1,
			Shows: "the other half of the lifecycle, and how the members got there",
		},
		{
			Milestone: "M28", Feature: "More than one member",
			Query: `SELECT count(DISTINCT user_id) FROM memberships WHERE organization_id = $1`,
			Min:   3,
			Shows: "the members page as a list rather than as a single row",
		},
		{
			Milestone: "M28", Feature: "A member who is not an owner",
			Query: `SELECT count(*) FROM memberships m
			          JOIN roles r ON r.id = m.role_id
			         WHERE m.organization_id = $1 AND r.slug <> 'owner'`,
			Min:   2,
			Shows: "rank: manage only strictly below your own, with something to act on",
		},
		{
			Milestone: "M28", Feature: "A workspace-scoped membership",
			Query: `SELECT count(*) FROM memberships
			         WHERE organization_id = $1 AND workspace_id IS NOT NULL`,
			Min:   1,
			Shows: "that a grant adds and never narrows (D31)",
		},
		{
			Milestone: "M30", Feature: "Blocked destination attempts",
			Query: `SELECT count(*) FROM audit_logs
			         WHERE organization_id = $1 AND action = 'destination.blocked'`,
			Min:   3,
			Shows: "that refusals are recorded, which is what makes a tier tunable",
		},
		{
			Milestone: "M30", Feature: "The attempted URL stored defanged",
			Query: `SELECT count(*) FROM audit_logs
			         WHERE organization_id = $1 AND action = 'destination.blocked'
			           AND metadata->>'url_defanged' LIKE '%[:]%'
			           AND metadata->>'url_defanged' NOT LIKE '%://%'`,
			Min:   3,
			Shows: "that a hostile URL is inert in the column, not at display time",
		},
		{
			Milestone: "M31", Feature: "A dispute waiting for review",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'open'`,
			Min:   1,
			Shows: "the review queue with something in it",
		},
		{
			Milestone: "M31", Feature: "A dispute that was allowed",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'allowed'`,
			Min:   1,
			Shows: "a decision that removed an entry from the blocklist",
		},
		{
			Milestone: "M31", Feature: "A dispute that was upheld",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'upheld'`,
			Min:   1,
			Shows: "that looking and saying no is a recorded decision too",
		},
		{
			Milestone: "M45", Feature: "A dispute whose blocklist entry is not its host",
			Query: `SELECT count(*) FROM destination_disputes
			         WHERE blocked_host <> '' AND blocked_host <> host`,
			Min: 1,
			Shows: "the queue naming the entry Allow deletes, which is a different " +
				"host from the one that was typed whenever somebody is refused by " +
				"a subdomain of a listed entry (F33). A demo where the two are " +
				"always equal renders identically with the entry recorded or not",
		},
		{
			Milestone: "M32", Feature: "No reputation feed verdict anywhere",
			Query: `SELECT count(*) FROM destination_disputes
			         WHERE reason_code = 'low_confidence.feed_reputation'`,
			MaxIsZero: true,
			Shows: "that the demo enabled no feed: a feed verdict here would mean " +
				"every seeded destination was sent to a third party",
		},
		{
			Milestone: "M33", Feature: "A link that forwards the path below its alias",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND forward_path`,
			Min: 1,
			Shows: "deep-link forwarding, which is invisible on a demo where every " +
				"link ignores what follows its alias. M33 is the milestone this " +
				"enumeration was missing entirely (F119): it shipped inside the " +
				"window where the obligation existed and nothing enforced it, which " +
				"is the window M33.5 closed — and deleting its one `forwardPath: " +
				"true` from the seeder left the build green",
		},
		{
			Milestone: "M32.5", Feature: "Bot blocking on for exactly one link",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND bot_blocking = 'on'`,
			Min: 1, Max: 1,
			Shows: "the setting, discoverable on a link that has it",
		},
		{
			Milestone: "M32.5", Feature: "Links that inherit the domain's answer",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND bot_blocking = 'inherit'`,
			Min:   10,
			Shows: "precedence, which is invisible when every link says the same thing",
		},

		{
			Milestone: "M34", Feature: "A routing-rule list somebody can read in order",
			// Also narrowed to `match` by M36, so that seeding more split arms
			// can never satisfy this row on M34's behalf.
			Query: `SELECT count(*) FROM routing_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND kind = 'match'`,
			Min: 4,
			Shows: "priority ordering and first-match evaluation, which one rule " +
				"on one link cannot demonstrate",
		},
		{
			Milestone: "M34", Feature: "A routing rule that is switched off",
			// Narrowed to `match` by M36, which seeds a parked split arm on
			// another link. The row's claim is about a *rule* having two states;
			// without the kind filter it started counting M36's arms too and
			// tripped its own ceiling. The equivalent claim for an arm is M36's
			// own row below.
			Query: `SELECT count(*) FROM routing_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND kind = 'match' AND NOT enabled`,
			Min: 1, Max: 1,
			Shows: "that a rule has two states; a list where everything is on " +
				"never shows the control that turns one off",
		},
		{
			Milestone: "M34", Feature: "Rule targets as their own destinations",
			Query: `SELECT count(*) FROM destinations d
			         JOIN links l ON l.id = d.link_id
			         JOIN routing_rules rr ON rr.destination_id = d.id
			         WHERE d.workspace_id IN (` + demoWorkspaces + `)
			           AND d.id <> l.primary_destination_id
			           AND rr.kind = 'match'`,
			Min: 4,
			Shows: "that a rule's target is a destination row like any other, " +
				"which is what puts it through the same tier check",
		},

		{
			Milestone: "M35", Feature: "A password-protected link",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND password_hash IS NOT NULL`,
			Min: 1, Max: 1,
			Shows: "the challenge page, which is the only part of this product a " +
				"visitor sees that is not a redirect",
		},
		{
			Milestone: "M35", Feature: "A one-time link and a click-limited one",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND (one_time OR max_clicks IS NOT NULL)`,
			Min: 2,
			Shows: "that a ceiling and a single use are two settings, not one; " +
				"either alone reads as the other",
		},
		{
			Milestone: "M35", Feature: "A click budget partly spent",
			Query: `SELECT count(*) FROM link_click_budget
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND consumed > 0`,
			Min: 1, Max: 1,
			Shows: "the exact counter beside the limit. A limit with nothing " +
				"against it is indistinguishable from a limit that does not work",
		},
		{
			Milestone: "M35", Feature: "A link that requires a signed URL",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND require_signature`,
			Min: 1, Max: 1,
			Shows: "the 403 an unsigned request gets, and the signing form that " +
				"produces a URL which does not get it",
		},
		{
			Milestone: "M35", Feature: "A workspace signing secret",
			Query: `SELECT count(*) FROM workspaces
			         WHERE organization_id = $1 AND signing_secret IS NOT NULL`,
			Min: 1, Max: 1,
			Shows: "that signing minted a key on first use — without one the " +
				"signature-gated link above refuses everybody, including its owner",
		},

		{
			Milestone: "M36", Feature: "A weighted split with an arm switched off",
			Query: `SELECT count(*) FROM routing_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND kind = 'weighted'`,
			Min: 3, Max: 3,
			Shows: "percentage splits, and that `enabled` is the feature flag — " +
				"a list where every arm is on never shows the control that parks one",
		},
		{
			Milestone: "M36", Feature: "A sequential rotation",
			Query: `SELECT count(*) FROM routing_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND kind = 'sequential'`,
			Min: 2,
			Shows: "that a rotation is a different thing from a percentage, which " +
				"one weighted split alone reads as the only kind there is",
		},
		{
			Milestone: "M36", Feature: "A fallback destination",
			Query: `SELECT count(*) FROM routing_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND kind = 'fallback'`,
			Min: 1, Max: 1,
			Shows: "where a link sends anybody no rule and no arm claimed, without " +
				"the link's own destination having been edited",
		},
		{
			Milestone: "M36", Feature: "Arms with different weights",
			Query: `SELECT count(DISTINCT d.weight) FROM destinations d
			          JOIN routing_rules rr ON rr.destination_id = d.id
			         WHERE d.workspace_id IN (` + demoWorkspaces + `)
			           AND rr.kind = 'weighted'`,
			Min: 2,
			Shows: "that a weight is a setting; every arm at 100 is indistinguishable " +
				"from a split that ignores weights",
		},
		{
			Milestone: "M36", Feature: "Clicks attributed to a destination",
			Query: `SELECT count(*) FROM click_events
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND destination_id IS NOT NULL`,
			Min: 100,
			Shows: "the per-destination breakdown. A split with no attribution is " +
				"a coin flip with extra steps",
		},
		{
			Milestone: "M36", Feature: "The breakdown reaches more than one arm",
			Query: `SELECT count(DISTINCT destination_id) FROM click_events
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND destination_id IS NOT NULL`,
			Min: 2,
			Shows: "a comparison rather than a single bar, which is the only shape " +
				"an A/B test can be read from",
		},
		{
			Milestone: "M36", Feature: "The breakdown is rolled up, not read raw",
			Query: `SELECT count(*) FROM link_dimension_daily
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND dimension = 'destination'`,
			Min: 10,
			Shows: "that the link detail page reads a rollup like every other " +
				"breakdown, rather than scanning click_events per request",
		},

		{
			Milestone: "M37", Feature: "A country breakdown wide enough to shade a map",
			Query: `SELECT count(DISTINCT value) FROM link_dimension_daily
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND dimension = 'country'`,
			Min: 12,
			Shows: "the choropleth as a map rather than as one shaded country: a " +
				"five-band scale needs a spread to band, and traffic from three " +
				"countries renders three shapes and 171 empty ones",
		},
		{
			Milestone: "M37", Feature: "Countries the map can actually draw",
			Query: `SELECT count(DISTINCT d.value) FROM link_dimension_daily d
			         WHERE d.workspace_id IN (` + demoWorkspaces + `)
			           AND d.dimension = 'country'
			           AND d.value IN ('US','GB','DE','IN','FR','CA','AU','NL','BR',
			                           'JP','ES','IT','SE','PL','MX','SG','IE','CH',
			                           'NO','ZA')`,
			Min: 12,
			Shows: "that the seeded codes are ones the 110m map has shapes for — a " +
				"demo full of territories the map cannot draw would show an empty " +
				"world and a long \"counted but not drawn\" line",
		},

		{
			Milestone: "M38", Feature: "A folder tree, not a flat list",
			Query: `SELECT count(*) FROM folders
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL AND parent_id IS NOT NULL`,
			Min: 3,
			Shows: "that folders nest — with none inside another, the tree page is " +
				"a list, the move control has nowhere to move anything to, and the " +
				"depth cap is a number in help text rather than a rule",
		},
		{
			Milestone: "M38", Feature: "Links filed in folders, and links in none",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL AND folder_id IS NOT NULL`,
			Min: 6,
			// Bounded above as well, and the ceiling is the point: filing every
			// link would empty the "No folder" filter, and the folder counts
			// would stop being visibly a subset of the catalogue.
			Max: 15,
			Shows: "the links list filtered by folder, and the \"No folder\" filter " +
				"beside it — one of them is empty if every link is filed or none is",
		},

		{
			Milestone: "M39", Feature: "A hostname registered to a workspace",
			// Workspace-owned, which is the whole of what M39 added: the
			// instance default (00700) has both owner columns NULL and would
			// satisfy a query that only counted rows in `domains`.
			Query: `SELECT count(*) FROM domains
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL`,
			Min: 2,
			Shows: "the domains page with something on it, in more than one " +
				"workspace — with one registration the page is a list, and the " +
				"rule it exists for (a hostname belongs to exactly one workspace) " +
				"is invisible until switching workspace changes what is shown",
		},
		{
			// M39 asserted this count was *zero*, on the grounds that a verified
			// row would be the demo promising serving M40 had not built. M40
			// built it, so the row becomes a real one rather than being deleted —
			// the same move M34 and M36 made with their own boundary rows.
			//
			// Bounded above as well as below, and the ceiling is the whole
			// assertion. Exactly one of the two registered hostnames verifies;
			// the other stays registered and failing. A demo where both work
			// shows one state, and a reader cannot see that verification decides
			// anything unless the page shows a hostname where it has not.
			Milestone: "M40", Feature: "One hostname verified, one not",
			Query: `SELECT count(*) FROM domains
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL AND verified_at IS NOT NULL`,
			Min: 1, Max: 1,
			Shows: "the domains page with both states on it — a hostname serving " +
				"links and a hostname that is not, which is what the verification " +
				"gate is for",
		},
		{
			Milestone: "M40", Feature: "A link served on a custom hostname",
			// Scoped to a non-default domain, because every other link in the
			// demo is on the instance default and a query counting links with a
			// domain would be satisfied by all of them.
			Query: `SELECT count(*) FROM links l
			          JOIN domains d ON d.id = l.domain_id
			         WHERE l.workspace_id IN (` + demoWorkspaces + `)
			           AND l.deleted_at IS NULL
			           AND NOT d.is_default AND d.verified_at IS NOT NULL`,
			Min: 1,
			Shows: "a short URL built from the workspace's own hostname, and the " +
				"links list's hostname filter — with no such link the filter has " +
				"one option and the feature is invisible",
		},
		{
			Milestone: "M40", Feature: "A verified hostname's own root redirect",
			Query: `SELECT count(*) FROM domains
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL AND root_redirect_url IS NOT NULL`,
			Min: 1,
			Shows: "that a custom hostname's bare domain goes somewhere its owner " +
				"chose rather than answering 404",
		},

		{
			// M41 asserted this count was *zero* until M41 was built, which is
			// the boundary-row move M34, M36, M39 and M40 each made before it.
			//
			// Bounded above as well as below, and the ceiling is the assertion.
			// Exactly one link carries a stored style; every other link's code
			// is drawn at the default. A demo where all of them are styled shows
			// one state, and a reader cannot see that a style is a preference —
			// nor that "back to black on white" is a button that appears only on
			// a link that has one.
			// **The ceiling moved to two under M50**, which added a named code
			// to the same link. It is still a ceiling and still says the same
			// thing: exactly one link carries stored codes, and every other
			// link's code is drawn at the default with no row at all.
			Milestone: "M41", Feature: "A QR code somebody has styled",
			Query: `SELECT count(*) FROM qr_codes
			         WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min: 2, Max: 2,
			Shows: "the QR panel with a code that is not black on white, and the " +
				"reset button beside it — with no styled code the panel shows one " +
				"state and the style form looks like it does nothing",
		},
		{
			// M50. Two codes on one link, each named, and scan history against
			// both.
			//
			// **Three assertions rather than one, because the feature is the
			// difference between them.** A link with two rows shows a list; a
			// link whose two rows are both unnamed shows a list nobody can read;
			// and a link whose two codes have no scans between them shows a
			// breakdown of zeroes. The whole value of per-code identity is
			// telling two numbers apart, so a demo that seeds the codes and not
			// the traffic demonstrates nothing.
			Milestone: "M50", Feature: "Two QR codes on one link, told apart",
			Query: `SELECT count(*) FROM qr_codes
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND label <> ''`,
			Min: 2, Max: 2,
			Shows: "the QR panel listing a link's codes by name, with a scan count " +
				"beside each — one code is a list of one, and unnamed codes are a " +
				"list nobody can read",
		},
		{
			Milestone: "M50", Feature: "A named code with an identity in its payload",
			Query: `SELECT count(*) FROM qr_codes
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND slug <> ''`,
			Min: 1, Max: 1,
			Shows: "a code whose picture encodes ?src=qr&qrc=<slug> beside one whose " +
				"picture does not, which is what makes the two scannable apart",
		},
		{
			Milestone: "M50", Feature: "Scan history against more than one code",
			Query: `SELECT count(DISTINCT referrer_host) FROM click_events
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND (referrer_host = 'qr' OR referrer_host LIKE 'qr:%')`,
			Min: 2,
			Shows: "two rows in the per-code breakdown with different numbers in " +
				"them — with traffic against only one code the panel shows a " +
				"column of zeroes beside a column of clicks",
		},
		{
			Milestone: "M41", Feature: "Campaigns, more than one, and one of them over",
			Query: `SELECT count(*) FROM campaigns
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL`,
			Min: 3,
			Shows: "the campaigns page as a list rather than as a single row, with " +
				"a schedule column that means something — one campaign has closed, " +
				"and its link still redirects, which is what \"the dates enforce " +
				"nothing\" looks like",
		},
		{
			Milestone: "M41", Feature: "Links carrying a campaign, and links carrying none",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND deleted_at IS NULL AND campaign_id IS NOT NULL`,
			Min: 4,
			// Bounded above for the reason the folder row is: labelling every
			// link would empty the "No campaign" filter, and the campaign counts
			// would stop being visibly a subset of the catalogue.
			Max: 15,
			Shows: "the links list filtered by campaign, and the \"No campaign\" " +
				"filter beside it — one of them is empty if every link is labelled " +
				"or none is",
		},

		{
			// M42 asserted this count was *zero* until M42 was built, which is
			// the boundary-row move M34, M36, M39, M40 and M41 each made before
			// it.
			//
			// Bounded above as well as below, and the ceiling carries the claim.
			// Exactly two registrations: one enabled and one paused. A page where
			// every webhook says the same thing shows one state, and a reader
			// cannot see that the pause button does anything.
			Milestone: "M42", Feature: "Two webhooks, one enabled and one paused",
			Query: `SELECT count(*) FROM webhooks
			         WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min: 2, Max: 2,
			Shows: "the webhooks page with both states on it — where this " +
				"workspace's events go, and a registration that is switched off",
		},
		{
			Milestone: "M42", Feature: "A delivery log with a success and a failure",
			Query: `SELECT count(DISTINCT d.status) FROM webhook_deliveries d
			          JOIN webhooks w ON w.id = d.webhook_id
			         WHERE w.workspace_id IN (` + demoWorkspaces + `)`,
			Min: 2,
			Shows: "the panel somebody opens when events stopped arriving. One " +
				"delivered row alone reads as \"it works\"; the abandoned one " +
				"beside it is what shows the attempt count and the \"no answer\" " +
				"response, which is the cell that actually gets read",
		},
		{
			// Not a display claim — a safety claim, and the one worth asserting
			// rather than trusting. The demo is a public instance, and a pending
			// delivery seeded into it would be the seeder handing the scheduler
			// an outbound connection to make on somebody else's behalf.
			//
			// It stays true because the seeder's link service is built with no
			// emitter, so seeding a catalogue queues nothing, and because the two
			// history rows it writes directly are both terminal. Anything that
			// changed either would show up here.
			Milestone: "M42", Feature: "The seeder queues no outbound delivery",
			Query: `SELECT count(*) FROM webhook_deliveries d
			          JOIN webhooks w ON w.id = d.webhook_id
			         WHERE w.workspace_id IN (` + demoWorkspaces + `)
			           AND d.status = 'pending'`,
			MaxIsZero: true,
			Shows: "that seeding the demo dials nobody: every seeded delivery is " +
				"already finished, and the hostnames are .example names that " +
				"cannot resolve for anyone",
		},

		{
			// M43 asserted this count was *zero* until M43 was built, which is
			// the boundary-row move M34, M36, M39, M40, M41 and M42 each made
			// before it.
			//
			// Bounded above as well as below, and the ceiling carries the claim.
			// Exactly three rules: two enabled and one paused. A page where every
			// rule says the same thing shows one state, and a reader cannot see
			// that the pause button does anything.
			Milestone: "M43", Feature: "Three automation rules, one of them paused",
			Query: `SELECT count(*) FROM automation_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min: 3, Max: 3,
			Shows: "the automation page with both states on it — standing " +
				"instructions the scheduler runs, and one that is switched off",
		},
		{
			// The vocabulary, not the count. One rule per trigger, so the page
			// shows what a rule can watch for rather than three spellings of one
			// thing — and so a trigger that stopped working is visible as a row
			// that never fires rather than as an absence nobody notices.
			Milestone: "M43", Feature: "Every trigger in the vocabulary has a rule",
			Query: `SELECT count(DISTINCT trigger) FROM automation_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min: 3, Max: 3,
			Shows: "all three triggers on one page: a link expiring, a click " +
				"budget running out, and a destination somebody was refused",
		},
		{
			// Not a display claim — a safety claim, and the same one D81 made
			// about the demo's webhooks, one turn further out.
			//
			// The demo is a public instance anybody can drive. `archive_link` is
			// the one action that changes what another visitor sees: somebody who
			// set an expiry on a link they were showing a colleague would come
			// back to find a rule they did not write had archived it. The action
			// exists and the form offers it; the demo seeds only the two that
			// report.
			//
			// Asserted rather than trusted, because "the seeder does not do that"
			// is exactly the kind of claim that survives the change that makes it
			// false.
			Milestone: "M43", Feature: "No seeded rule archives anybody's link",
			Query: `SELECT count(*) FROM automation_rules
			         WHERE workspace_id IN (` + demoWorkspaces + `)
			           AND actions @> '["archive_link"]'::jsonb`,
			MaxIsZero: true,
			Shows: "that the demo's automation reports and never changes a " +
				"visitor's links behind their back",
		},
		{
			// The second safety claim, and it is the reason a `webhook` action is
			// in the demo at all.
			//
			// A firing queues a delivery, and every seeded registration points at
			// a `.example` hostname RFC 2606 guarantees never resolves — so the
			// demo can show an automation with an outbound consequence without
			// becoming a machine that connects to a stranger's server on a
			// schedule. It stays true because CreateAutomationRule arms a rule at
			// the instant it is written, so the watermark is already past
			// everything the seeder wrote and the first evaluation matches
			// nothing.
			Milestone: "M43", Feature: "The seeder fires no rule",
			Query: `SELECT count(*) FROM audit_logs
			         WHERE action = 'automation.fired'
			           AND organization_id = $1`,
			MaxIsZero: true,
			Shows: "that seeding the demo runs nobody's standing instruction: " +
				"every rule is armed at creation, so the backlog it was created " +
				"beside is behind its watermark and invisible to it",
		},

		{
			// M44 asserted this count was *zero* until M44 was built, which is the
			// boundary-row move M34, M36, M39, M40, M41, M42 and M43 each made
			// before it.
			//
			// Four rows for three credentials, and the arithmetic is the feature:
			// the seeder mints three and rotates one, and a rotation is a fourth
			// row rather than an edit to the third.
			//
			// Bounded above as well as below, and the ceiling is the load-bearing
			// half. Every row here is a live credential on a public instance —
			// unusable, because the token is discarded and only an HMAC is stored,
			// but a seeder that quietly grew this number would be minting keys
			// nobody asked for on every demo-update.
			Milestone: "M44", Feature: "Four key rows: three minted, one of them a successor",
			Query: `SELECT count(*) FROM api_keys WHERE organization_id = $1`,
			Min:   4, Max: 4,
			Shows: "the key page as a list rather than as an empty panel, with a " +
				"rotated pair, an organization-wide key and an ordinary one on it",
		},
		{
			// The rotation itself, which is the whole of what M44 added and the one
			// thing on the page nobody can produce by clicking: rotation refuses any
			// actor that is not a key, so a visitor holding a session cannot make
			// this state exist.
			//
			// Exactly one, because the claim is that a rotation is visible, not that
			// the demo is a graveyard of superseded credentials.
			Milestone: "M44", Feature: "A key that has rotated itself",
			Query: `SELECT count(*) FROM api_keys
			         WHERE organization_id = $1 AND successor_id IS NOT NULL`,
			Min: 1, Max: 1,
			Shows: "a predecessor counting down beside the successor that replaced " +
				"it — the state the rotation milestone exists to show, and one a " +
				"session cannot create",
		},
		{
			// The scope choice, as two values rather than as a column of one word.
			// `workspace_id IS NULL` is the choice itself, so counting its distinct
			// values counts the states the *Reach* column can be in.
			Milestone: "M44", Feature: "Keys of both reaches",
			Query: `SELECT count(DISTINCT (workspace_id IS NULL)) FROM api_keys
			         WHERE organization_id = $1`,
			Min: 2, Max: 2,
			Shows: "that a key's reach is a choice: one bound to the workspace it " +
				"was made in, one valid across the organization",
		},
		{
			// Not a display claim — a safety claim, and the same shape D81 and D86
			// made about webhooks and automation.
			//
			// The demo is public and these are credentials. The seeder must leave
			// none of them in a state where the window is still open on a key whose
			// successor has also lapsed, and more importantly none of them may be
			// **already dead on arrival**: a predecessor seeded past its own grace
			// would show as revoked the moment the instance came up, which is the
			// one state that makes the rotation invisible again.
			//
			// Asserted rather than trusted, because "the seeder uses the maximum
			// grace" is exactly the kind of claim that survives the change that
			// makes it false.
			Milestone: "M44", Feature: "The rotated key's window is still open",
			Query: `SELECT count(*) FROM api_keys
			         WHERE organization_id = $1
			           AND grace_expires_at IS NOT NULL
			           AND grace_expires_at <= now()`,
			MaxIsZero: true,
			Shows: "that the demo's rotated key is mid-rotation rather than already " +
				"dead, so the page shows a window closing instead of a revocation " +
				"that happened before anybody arrived",
		},
		{
			// The instance-level principal, and the delegation that is the whole
			// point of it (M45, D98). Two holders: the owner, conferred when the
			// instance was claimed, and the admin they appointed.
			//
			// Bounded above as well as below, because this is instance-wide reach
			// on a public instance. A seeder that quietly grew this number would be
			// handing the demo's dispute queue to more people on every
			// demo-update — the same reason the API key row is capped.
			Milestone: "M45", Feature: "Instance-level review, held by two people",
			Query: `SELECT count(*) FROM instance_grants ig
			          JOIN permissions p ON p.id = ig.permission_id
			         WHERE p.slug = 'destinations.review'`,
			Min: 2, Max: 2,
			Shows: "the reviewer roster under the dispute queue as a list somebody " +
				"appointed, rather than one row saying \"you\"",
		},
		{
			// Not a display claim — the claim F15 is about, asserted against the
			// demo rather than only against a test fixture.
			//
			// No organization role may carry instance-wide reach. If a later
			// migration grants one of these four to owner or admin, every account
			// that registers on a public instance holds it, which is the finding
			// this milestone closed and the one shape most likely to come back by
			// accident.
			//
			// The slug list is built from auth.InstancePrincipalScopes rather
			// than written here, because a hand-written copy is short by one the
			// moment a scope is added — which happened at D100, when
			// domains.write.instance joined the four this row used to name. A
			// guard that enumerates by hand is the thing it is guarding against.
			Milestone: "M45", Feature: "No organization role carries instance-wide reach",
			Query: `SELECT count(*) FROM role_permissions rp
			          JOIN permissions p ON p.id = rp.permission_id
			         WHERE p.slug IN (` + quotedScopes(auth.InstancePrincipalScopes) + `)`,
			MaxIsZero: true,
			Shows: "that instance-wide reach is held by named people and by no role, " +
				"so registering an account never confers it",
		},
		{
			// The instance-wide audit surface (F36). Rows belonging to no
			// organization: the demo's two dispute decisions and the reviewer it
			// appointed.
			//
			// Deliberately not scoped by $1 — being outside every organization is
			// the property being asserted, so a query that filtered by one would
			// pass by testing nothing.
			Milestone: "M45", Feature: "An audit trail for acts that belong to no tenant",
			Query: `SELECT count(DISTINCT action) FROM audit_logs WHERE organization_id IS NULL`,
			Min:   2,
			Shows: "that an instance-wide act is recorded where it happened rather " +
				"than filed under whichever organization the person was standing in",
		},

		{
			// M48's click-through, and the one thing it needs the demo to have:
			// an inbox holding notifications of **more than one kind**, so
			// clicking two of them goes to two different places.
			//
			// m48.md asks whether a coverage row is owed at all —
			// *"demoCoverage() gains a row only if a kind ends up with no seeded
			// example; whether it does is settled during the work rather than
			// guessed here"* — and the answer is yes, but not the row that
			// question implies. Three of the seven declared kinds have no seeded
			// example: `audit.growth`, `domain.failing` and `domain.unverified`.
			// None of them is seedable without the demo asserting something
			// untrue about itself — that its audit log has outgrown its disk, or
			// that a hostname it serves has stopped verifying — so the honest row
			// is about what the feature needs rather than about the vocabulary
			// being complete. decisions.md carries the full reasoning.
			//
			// Counted as distinct kinds rather than as a list of them. A query
			// naming the kinds would be a second enumeration of the vocabulary,
			// which internal/httpx's notificationTargets is deliberately the only
			// one of — and one written in SQL, where no test would notice it
			// going stale.
			Milestone: "M48", Feature: "Notifications of more than one kind in the owner's inbox",
			Query: `SELECT count(DISTINCT kind) FROM notifications
			         WHERE user_id = $2
			           AND (workspace_id IS NULL
			                OR workspace_id IN (` + demoWorkspaces + `))`,
			Min: 2,
			Shows: "that opening a notification goes somewhere, and somewhere " +
				"different for a different kind. One kind in the inbox shows a " +
				"click-through that could be a hardcoded link",
		},
	}
}

// TestDemoSeederShowsEveryFeatureItClaimsTo runs the seeder the way
// `make demo-update` runs it, then checks every row of the list above.
//
// It also runs it twice, because "idempotent under --reset" is a property the
// demo target depends on at every milestone boundary: the second run must
// produce the same demo as the first, or somebody's screenshot stops matching
// the instance.
func TestDemoSeederShowsEveryFeatureItClaimsTo(t *testing.T) {
	ctx := context.Background()
	pool := newDemoDB(t)
	cfg := demoTestConfig()

	owner := claimDemoInstance(t, pool, cfg)

	first := runDemoSeed(t, ctx, pool, cfg, owner.Email)
	t.Logf("seeder runtime, first run: %s", first.Round(time.Millisecond))

	orgID, ownerID := demoScope(t, pool, owner.Email)
	counts := checkDemoCoverage(t, pool, orgID, ownerID)

	// Second run, same flags. --reset is what `task demo` passes, and the whole
	// point of it is that running it again is safe.
	second := runDemoSeed(t, ctx, pool, cfg, owner.Email)
	t.Logf("seeder runtime, second run: %s", second.Round(time.Millisecond))

	// The organization is the same; the owner is the same account. The second
	// workspace and the seeded people are new rows with new ids, which is why the
	// comparison is on what the demo shows rather than on identifiers.
	orgID, ownerID = demoScope(t, pool, owner.Email)
	again := checkDemoCoverage(t, pool, orgID, ownerID)

	for i, feature := range demoCoverage() {
		if counts[i] != again[i] {
			t.Errorf("`lctl demo --reset` is not idempotent: %s (%s) counted %d "+
				"on the first run and %d on the second. Running demo-update twice "+
				"must produce the same demo. Two causes, and the count says which: "+
				"a count that only ever grows means something the seeder writes has "+
				"no matching line in demoResetPhase2; a count that moves either way "+
				"means the seeder writes something the clock decides, which is what "+
				"cmd/lctl/demo_clicks_test.go exists to catch before it reaches here.",
				feature.Feature, feature.Milestone, counts[i], again[i])
		}
	}

	// Third run, from the state `make demo-update` actually meets (M36).
	//
	// The two runs above both start with the owner's last-used workspace equal
	// to the one the catalogue is in, because the first run restores it and the
	// second is a copy of the first. A long-lived demo instance is not in that
	// state: somebody signs in and uses the workspace switcher M25 built, or a
	// run fails between the seeder moving the owner into the second workspace
	// and moving them back. Either leaves the account's preference pointing
	// somewhere else.
	//
	// That is not a hypothetical: it is what broke `make demo-update` at M36.
	// demoReset scoped its link, tag and destination deletes to whatever
	// workspace the actor resolved into, while everything else in the same
	// transaction was scoped to the organization. The reset committed, removed
	// everything organization-scoped and nothing workspace-scoped — then the very
	// first catalogue link collided with the copy of itself the reset had walked
	// past, because alias uniqueness is per domain rather than per workspace.
	//
	// M36 closed the path by restoring the owner's workspace; M45 closed the
	// asymmetry, so every statement in demoReset is now organization-scoped
	// (F68). This run is what holds both: it reaches the state from the outside,
	// the way a person clicking the switcher does, which no amount of restoring
	// inside the seeder can prevent.
	switchOwnerAwayFromTheCatalogue(t, pool, ownerID, orgID)

	third := runDemoSeed(t, ctx, pool, cfg, owner.Email)
	t.Logf("seeder runtime, third run (owner switched away): %s", third.Round(time.Millisecond))

	orgID, ownerID = demoScope(t, pool, owner.Email)
	moved := checkDemoCoverage(t, pool, orgID, ownerID)

	for i, feature := range demoCoverage() {
		if counts[i] != moved[i] {
			t.Errorf("`lctl demo --reset` is not idempotent once the owner has "+
				"switched workspace: %s (%s) counted %d on the first run and %d "+
				"after the switch. Where the demo is written, and which rows the "+
				"reset removes, must not depend on where somebody left the "+
				"workspace switcher. This comparison spans all three runs, so it "+
				"is also the one furthest apart in wall-clock time — read the fork "+
				"in the error above before concluding the switch caused it.",
				feature.Feature, feature.Milestone, counts[i], moved[i])
		}
	}
}

// TestDemoResetClearsTheCatalogueFromAnyWorkspaceInTheOrganization is F68's
// asymmetry as a claim, and it has to call demoReset directly to be one.
//
// The three statements that removed links, tags and destinations were scoped to
// `actor.WorkspaceID`, while the rollups, the click events and everything in
// demoResetPhase2 in the same transaction were scoped to the organization. An
// actor resolving into any other workspace of the organization therefore
// committed a half-reset — links left standing, their analytics wiped — and
// reported success.
//
// The seeder-level test above cannot reach this. M36 closed the reachable path
// by restoring the owner's workspace before the reset runs, so by the time
// demoReset is called the actor is always back in the catalogue's workspace and
// a workspace-scoped delete is indistinguishable from an organization-scoped
// one. Narrowing the statement back to one workspace leaves that test green,
// which is how the asymmetry survived M36 in the first place.
//
// So this drives the unit rather than the seeder, with an actor pointed at the
// second workspace: the state M36 prevents the seeder from producing, and which
// nothing prevents a future caller from passing in.
func TestDemoResetClearsTheCatalogueFromAnyWorkspaceInTheOrganization(t *testing.T) {
	ctx := context.Background()
	pool := newDemoDB(t)
	cfg := demoTestConfig()
	owner := claimDemoInstance(t, pool, cfg)
	runDemoSeed(t, ctx, pool, cfg, owner.Email)
	orgID, ownerID := demoScope(t, pool, owner.Email)

	before := demoCatalogueLinks(t, pool, orgID)
	if before == 0 {
		t.Fatal("the seeded demo has no catalogue links; the assertion below would prove nothing")
	}

	// The second workspace — not the one the catalogue is in, which is exactly
	// what an actor who used the workspace switcher resolves into.
	var elsewhere uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT w.id FROM workspaces w
		 WHERE w.organization_id = $1 AND w.deleted_at IS NULL
		 ORDER BY w.created_at DESC, w.id DESC LIMIT 1`, orgID).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	var catalogueWorkspace uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT w.id FROM workspaces w
		 WHERE w.organization_id = $1 AND w.deleted_at IS NULL
		 ORDER BY w.created_at, w.id LIMIT 1`, orgID).Scan(&catalogueWorkspace); err != nil {
		t.Fatal(err)
	}
	if elsewhere == catalogueWorkspace {
		t.Fatal("the demo has only one workspace; this test needs the second one")
	}

	actor := &auth.Identity{UserID: ownerID, OrgID: orgID, WorkspaceID: elsewhere}
	if err := demoReset(ctx, pool, actor, demoCatalogue()); err != nil {
		t.Fatalf("reset from another workspace: %v", err)
	}

	if after := demoCatalogueLinks(t, pool, orgID); after != 0 {
		t.Errorf("%d catalogue links survived a reset run from another workspace in "+
			"the same organization, out of %d. Every statement in demoReset has to "+
			"agree about its scope: the ones that removed the analytics already "+
			"used the organization, so a workspace-scoped link delete leaves a "+
			"half-reset that commits and reports success.", after, before)
	}
}

// demoCatalogueLinks counts the catalogue's links anywhere in the organization,
// which is the scope the reset is supposed to cover.
func demoCatalogueLinks(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID) int {
	t.Helper()
	aliases := make([]string, 0)
	for _, d := range demoCatalogue() {
		if d.alias != "" {
			aliases = append(aliases, d.alias)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM links
		 WHERE workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
		   AND alias = ANY($2::text[])`, orgID, aliases).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// switchOwnerAwayFromTheCatalogue points the owner's last-used workspace at the
// newest workspace in the organization — the one the seeder creates second.
//
// It writes the column the workspace switcher writes, which is the whole of what
// somebody clicking through the demo leaves behind: auth.Service.SwitchWorkspace
// moves the session and records the choice on the account, and only the second
// half of that outlives the browser. A session is not needed to reproduce the
// state, and demanding one here would be testing the switcher rather than the
// seeder.
func switchOwnerAwayFromTheCatalogue(t *testing.T, pool *pgxpool.Pool, ownerID, orgID uuid.UUID) {
	t.Helper()
	const q = `
		UPDATE users SET last_workspace_id = (
		    SELECT w.id FROM workspaces w
		     WHERE w.organization_id = $2 AND w.deleted_at IS NULL
		     ORDER BY w.created_at DESC, w.id DESC
		     LIMIT 1)
		 WHERE id = $1
		RETURNING last_workspace_id`
	var moved *uuid.UUID
	if err := pool.QueryRow(context.Background(), q, ownerID, orgID).Scan(&moved); err != nil {
		t.Fatalf("move the owner to another workspace: %v", err)
	}
	if moved == nil {
		t.Fatal("the demo organization has no second workspace to switch to; " +
			"the M25 coverage row above should have caught that first")
	}
}

// checkDemoCoverage evaluates every row and returns the counts, so the caller can
// compare two runs.
func checkDemoCoverage(t *testing.T, pool *pgxpool.Pool, orgID, ownerID uuid.UUID) []int64 {
	t.Helper()
	features := demoCoverage()
	counts := make([]int64, len(features))

	for i, f := range features {
		// Postgres refuses a parameter the statement does not mention, and the
		// boundary rows below name a whole table rather than a scope, so each
		// query is given exactly the placeholders it uses.
		var args []any
		if strings.Contains(f.Query, "$1") {
			args = append(args, orgID)
		}
		if strings.Contains(f.Query, "$2") {
			args = append(args, ownerID)
		}
		var n int64
		if err := pool.QueryRow(context.Background(), f.Query, args...).Scan(&n); err != nil {
			t.Fatalf("%s %s: %v\nquery: %s", f.Milestone, f.Feature, err, f.Query)
		}
		counts[i] = n

		switch {
		case f.MaxIsZero && n != 0:
			t.Errorf("%s — %s: %d rows, want none.\nThe demo shows %s.",
				f.Milestone, f.Feature, n, f.Shows)
		case f.MaxIsZero:
		case n < f.Min:
			t.Errorf("%s — %s: %d rows, want at least %d.\n"+
				"Without it the demo loses %s.\n"+
				"Extend cmd/lctl/demo_phase2.go so the demo instance shows the "+
				"feature instead of an empty page.",
				f.Milestone, f.Feature, n, f.Min, f.Shows)
		case f.Max > 0 && n > f.Max:
			t.Errorf("%s — %s: %d rows, want at most %d.\nThe demo shows %s.",
				f.Milestone, f.Feature, n, f.Max, f.Shows)
		}
	}
	return counts
}

// runDemoSeed runs the seeder with the flags `task demo` uses, and times it.
//
// The figure is logged rather than asserted. A threshold here would be a
// measurement of the machine the tests happen to be running on, and the property
// that matters — that `make demo-update` stays pleasant at a milestone
// boundary — is one a person judges from the number.
func runDemoSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg config.Config, email string) time.Duration {
	t.Helper()
	start := time.Now()
	if err := demoSeed(ctx, pool, cfg, demoOptions{
		user: email, days: 30, reset: true, volume: 4, prng: 1,
	}); err != nil {
		t.Fatalf("seed the demo: %v", err)
	}
	return time.Since(start)
}

// demoScope resolves the organization and owner every coverage query is written
// against.
func demoScope(t *testing.T, pool *pgxpool.Pool, email string) (orgID, userID uuid.UUID) {
	t.Helper()
	const q = `
		SELECT m.organization_id, u.id
		  FROM users u
		  JOIN memberships m ON m.user_id = u.id
		 WHERE u.email_lower = $1
		 ORDER BY m.created_at
		 LIMIT 1`
	if err := pool.QueryRow(context.Background(), q, strings.ToLower(email)).
		Scan(&orgID, &userID); err != nil {
		t.Fatalf("resolve the demo organization: %v", err)
	}
	return orgID, userID
}

// claimDemoInstance registers the first account, which is what somebody visiting
// a fresh instance does before anything can be seeded into it.
func claimDemoInstance(t *testing.T, pool *pgxpool.Pool, cfg config.Config) *auth.Identity {
	t.Helper()
	svc := auth.NewService(pool, auth.ServiceConfig{Params: auth.Params{
		MemoryKiB:   cfg.Auth.Argon2MemoryKiB,
		Iterations:  cfg.Auth.Argon2Iterations,
		Parallelism: cfg.Auth.Argon2Parallelism,
	}})
	id, err := svc.Register(context.Background(), auth.RegisterInput{
		Email: "demo-owner@example.com", Name: "Demo Owner",
		Password: "demo-owner-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	return id
}

// demoTestConfig is the configuration an instance runs with, as far as the
// seeder reads it.
//
// The argon2 costs are the real defaults rather than cheap test values: this test
// is also where the seeder's runtime is measured, and hashing five passwords is
// part of what it costs.
func demoTestConfig() config.Config {
	var cfg config.Config
	cfg.BaseURL = "http://localhost:8080"
	cfg.Auth.Argon2MemoryKiB = auth.DefaultParams.MemoryKiB
	cfg.Auth.Argon2Iterations = auth.DefaultParams.Iterations
	cfg.Auth.Argon2Parallelism = auth.DefaultParams.Parallelism
	cfg.Auth.InviteTTL = 168 * time.Hour
	// The seeder mints API keys (M44), and the key service refuses a pepper below
	// the configuration floor — deliberately, so a service built directly in a
	// test cannot be weaker than a deployed one. Any 32 bytes will do here: the
	// tokens are discarded and nothing verifies one afterwards.
	cfg.APIKeyPepper = config.Secret("a-demo-seeder-pepper-of-32-plus-bytes")
	return cfg
}

// newDemoDB creates a migrated database of its own and drops it afterwards.
//
// Its own rather than the integration suite's template: the suite drops and
// recreates that template in its TestMain, and two package test binaries running
// concurrently would race for it. One database, migrated once, costs a couple of
// seconds against a seeder that takes longer than that anyway.
func newDemoDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, demoDSNFor("postgres"))
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v\n\n"+
			"These tests need Postgres. Start it with `make up`, and run them with "+
			"`make test-integration` so TEST_DATABASE_URL is set.", err)
	}
	defer admin.Close()

	name := "t_demo_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:16]
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), demoDSNFor("postgres"))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, demoDSNFor(name), quiet); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := pgxpool.New(ctx, demoDSNFor(name))
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := store.EnsurePartitions(ctx, pool, store.PartitionLookahead); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return pool
}

// demoDSNFor rewrites the database name in TEST_DATABASE_URL.
const demoFallbackDSN = "postgres://linkctrl:devpassword@localhost:55432/linkctrl?sslmode=disable"

func demoDSNFor(name string) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = demoFallbackDSN
	}
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := ""
	if j := strings.Index(dsn[i:], "?"); j >= 0 {
		rest = dsn[i+j:]
	}
	return dsn[:i+1] + name + rest
}
