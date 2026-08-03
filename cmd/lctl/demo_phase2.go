package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// The Phase 2 half of the demo dataset (M33.5).
//
// `lctl demo` was written when a workspace, twenty links and a month of clicks
// were the whole product. Everything Phase 2 shipped — invitations, members, a
// second workspace, notifications, an audit trail, destination blocking, the
// dispute queue, bot blocking — rendered as an empty page on the demo instance,
// because nothing seeded a row for any of it.
//
// Three properties of this file are load-bearing rather than stylistic.
//
// **It seeds through the service layer.** Every membership, invitation,
// workspace, refusal and dispute below is produced by the same service call the
// dashboard and the REST API make. A dispute created by INSERT has never been
// through the form that files one, and a demo that diverges from what the
// product actually produces is worse than a thin demo. The three places that
// deliberately go around a service each say so at the statement, and there is no
// fourth.
//
// **It enables no reputation feed, needs no mailer, and does not touch
// LINKCTRL_SIGNUP_MODE.** Those are asserted by TestDemoSeederNeverEnablesAFeed,
// TestDemoSeederNeedsNoMailer and TestDemoSeederLeavesSignupModeAlone rather
// than promised here. The first is the one that would matter most if it were
// wrong: a feed sends every destination to a third party (M32), and a demo
// instance quietly doing that would violate the promise the feature was built to
// qualify.
//
// **It is idempotent under --reset.** Everything written here is removed again
// by demoResetPhase2, so two runs of `make demo-update` produce the same demo.
// That is what makes the target safe to run at every milestone boundary.
//
// The features this is expected to show are enumerated by the coverage test in
// demo_coverage_test.go, which fails when one of them has no seeded rows. A
// milestone that adds something visible extends the seeder and that list. That
// obligation is the point of the test, not a side effect of it.

// The demo's cast, beyond whoever claimed the instance.
//
// Addresses under example.com, which RFC 2606 reserves: a demo that seeds a real
// address is a demo that emails a stranger the first time somebody configures a
// relay on it.
const (
	// demoAdminEmail joins by redeeming an invitation and ends up an
	// organization-wide admin. They file the disputes, so the audit trail and the
	// review queue show a name that is not the owner's.
	demoAdminEmail = "morgan@example.com"
	demoAdminName  = "Morgan Reyes"

	// demoViewerEmail joins as an organization-wide viewer and is then granted
	// editor in the second workspace. That pair is D31's union rule made visible:
	// a grant adds and never narrows.
	demoViewerEmail = "sam@example.com"
	demoViewerName  = "Sam Okafor"

	// demoPendingEmail is invited and never redeems, so the invitations page has
	// an outstanding row beside the redeemed ones.
	demoPendingEmail = "riley@example.com"

	// demoPassword is what the seeded accounts sign in with. Written in the
	// clear because publishing it is the point — the demo instance exists to be
	// signed into — and it clears auth.MinPasswordLength.
	demoPassword = "demo-account-password"
)

// demoWorkspace2 is the second workspace, which is the entire difference between
// a switcher that renders nothing and one somebody can use.
const (
	demoWorkspace2Name = "Campaigns"
	demoWorkspace2Slug = "campaigns"
)

// The hosts the blocking and dispute story is told with.
//
// demoBlockedHost is the seeder's own: it is added to the runtime blocklist and
// removed again by the dispute that allows it. The other two are URL-shortener
// hosts seeded by migration 01500, and nothing here ever allows a dispute about
// them — an allow deletes the matched row, that migration never re-asserts its
// rows by design, and a demo run must not quietly retire a shortener from the
// instance's blocklist.
const (
	demoBlockedHost = "promo.tracker-demo.example"
	demoBlockedURL  = "https://promo.tracker-demo.example/offer?utm_source=demo"

	demoUpheldURL = "https://bit.ly/linkctrl-demo-upheld"
	demoOpenURL   = "https://tinyurl.com/linkctrl-demo-open"
)

// demoDisputedHosts are the hosts whose disputes the reset removes. Kept beside
// the URLs above so adding one cannot be done without teaching the reset about
// it.
func demoDisputedHosts() []string {
	return []string{demoBlockedHost, "bit.ly", "tinyurl.com"}
}

// demoWorkspace2Catalogue is the second workspace's links.
//
// Small on purpose. Its job is to make workspace scoping observable — the list,
// the tags and the analytics all change when the switcher moves — and a second
// twenty-link catalogue would only make the first one harder to read.
func demoWorkspace2Catalogue() []demoLink {
	return []demoLink{
		{alias: "spring-webinar", url: "https://example.com/campaigns/spring-webinar",
			title: "Spring webinar registration",
			desc:  "Lives in the Campaigns workspace: switch back and it is gone from the list.",
			tags:  []string{"campaign", "events"}, weight: 22, from: 21, age: 21},
		{alias: "field-guide", url: "https://example.com/campaigns/field-guide.pdf",
			title: "Field guide (gated download)",
			tags:  []string{"campaign", "content"}, weight: 14, from: 21, age: 21},
		{alias: "partner-portal", url: "https://partners.example.com",
			title: "Partner portal", tags: []string{"partners"},
			weight: 9, from: 18, spikeDay: 6, spikeMult: 4, age: 18},
		{alias: "roadshow", url: "https://example.com/campaigns/roadshow",
			title: "Roadshow city list", tags: []string{"events"},
			weight: 6, from: 14, age: 14},
		{alias: "cw-survey", url: "https://forms.example.com/campaign-survey",
			title: "Campaign feedback survey",
			desc:  "Tags are per workspace, so this one's tags are not the other workspace's.",
			tags:  []string{"research"}, weight: 4, from: 10, age: 10},
	}
}

// demoLinkConfig is the link service the seeder runs with.
//
// A function rather than a literal at the call site so a test can read it. The
// field that matters is the one that is absent: Feed is never set, so
// link.Service.askFeed short-circuits and no destination this seeder judges
// leaves the instance. See TestDemoSeederNeverEnablesAFeed.
func demoLinkConfig(cfg config.Config, rec audit.Recorder,
	hasher *auth.Hasher, gates link.GateReader,
) link.Config {
	return link.Config{
		Policy:  link.DefaultDestinationPolicy(),
		BaseURL: cfg.LinkOrigin(),
		// Wired so a refused destination writes the `destination.blocked` record
		// a real refusal writes, with the attempted URL defanged in the column.
		// Without it the demo would show a dispute about a refusal the audit log
		// has no trace of.
		Audit: rec,
		// The gates (M35). Without these the seeder could not set a link
		// password or mint a signed URL, and the demo would show four controls
		// that do nothing.
		Hasher: hasher,
		Gates:  gates,
		Log:    discardLogger(),
	}
}

// demoInviteConfig is the invitation service the seeder runs with.
//
// Two fields carry the milestone's prohibitions.
//
// Mail is nil, which is not a degraded mode: on a default instance the mailer is
// off (D1) and the copyable link is the whole delivery mechanism (D27). The
// seeder must run on such an instance, so it never asks for one.
//
// NewAccounts is false unconditionally — not read from the signup mode. Under
// `closed`, D7 says the environment ceiling is absolute and no invitation admits
// an account that does not exist; a seeder that flipped the mode, or that quietly
// passed true, would be making D7 false to save itself a query. The accounts the
// demo needs are written first, by the seeder, and redemption only ever adds a
// membership to one that already exists.
func demoInviteConfig(
	cfg config.Config, hasher *auth.Hasher, rec audit.Recorder, n notify.Notifier,
) invite.Config {
	return invite.Config{
		AppURL:      cfg.AppOrigin(),
		TTL:         cfg.Auth.InviteTTL,
		NewAccounts: false,
		Hasher:      hasher,
		Audit:       rec,
		Notify:      n,
		Mail:        nil,
		Log:         discardLogger(),
	}
}

// demoSeeder holds the services the Phase 2 content is written through.
//
// Constructed the way cmd/linkctrl constructs them, minus the mailer and minus
// the feed, so that what the demo shows is what the product produces.
type demoSeeder struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	cfg  config.Config
	now  time.Time
	opt  demoOptions

	auth    *auth.Service
	audit   *audit.Service
	notify  *notify.Service
	team    *team.Service
	invite  *invite.Service
	link    *link.Service
	dispute *dispute.Service
	// gates spends part of the click-limited link's budget through the same
	// statement the redirect path uses (M35).
	gates *gate.Service

	// owner is whoever claimed the instance, acting in their first workspace.
	owner *auth.Identity
}

func newDemoSeeder(
	pool *pgxpool.Pool, cfg config.Config, authSvc *auth.Service, linkSvc *link.Service,
	gateSvc *gate.Service, owner *auth.Identity, opt demoOptions, now time.Time,
) (*demoSeeder, error) {
	auditSvc := audit.NewService(pool)
	// No WithMail. The inbox is the baseline and mail is the addition, in that
	// order, at every call site in the product; the demo takes only the baseline.
	notifySvc := notify.NewService(pool)

	inviteSvc, err := invite.NewService(pool,
		demoInviteConfig(cfg, authSvc.Hasher(), auditSvc, notifySvc))
	if err != nil {
		return nil, fmt.Errorf("demo invitations: %w", err)
	}
	disputeSvc, err := dispute.NewService(pool, dispute.Config{
		Judge:  linkSvc,
		Audit:  auditSvc,
		Notify: notifySvc,
		Log:    discardLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("demo disputes: %w", err)
	}

	return &demoSeeder{
		pool: pool, q: dbgen.New(pool), cfg: cfg, now: now, opt: opt,
		auth:    authSvc,
		audit:   auditSvc,
		notify:  notifySvc,
		team:    team.NewService(pool, team.Config{Audit: auditSvc, Log: discardLogger()}),
		invite:  inviteSvc,
		link:    linkSvc,
		dispute: disputeSvc,
		gates:   gateSvc,
		owner:   owner,
	}, nil
}

// run seeds everything Phase 2 added, in the order the features depend on each
// other: accounts before invitations, invitations before memberships, the second
// workspace before the grant into it, refusals before the disputes about them.
func (s *demoSeeder) run(ctx context.Context, primary []demoLink, ids map[int]uuid.UUID) error {
	people, err := s.seedAccounts(ctx)
	if err != nil {
		return err
	}
	if err := s.seedInvitations(ctx, &people); err != nil {
		return err
	}
	if err := s.seedSecondWorkspace(ctx, people); err != nil {
		return err
	}
	if err := s.seedBotBlocking(ctx, primary, ids); err != nil {
		return err
	}
	if err := s.seedRoutingRules(ctx, primary, ids); err != nil {
		return err
	}
	if err := s.seedGatedLinks(ctx); err != nil {
		return err
	}
	if err := s.seedBlockingAndDisputes(ctx, people); err != nil {
		return err
	}
	return s.readSomeNotifications(ctx)
}

// demoPeople is the seeded cast, resolved to identities.
type demoPeople struct {
	admin  *auth.Identity
	viewer *auth.Identity
}

// seedAccounts writes the extra accounts.
//
// auth.Service.Register is the account writer, and calling it here is what the
// milestone means by "written by the seeder directly, not by opening
// registration": the signup mode governs the *request* paths in internal/signup,
// and nothing on this path reads it, changes it, or needs it relaxed. An
// operator with a CLI could always create an account; that is not the hole D7
// closes.
//
// Each account arrives with the personal organization and workspace registration
// gives everybody, because that is what registration does and a demo that
// skipped it would be showing a shape the product never produces.
func (s *demoSeeder) seedAccounts(ctx context.Context) (demoPeople, error) {
	var out demoPeople
	for _, acct := range []struct {
		email, name string
		into        **auth.Identity
	}{
		{demoAdminEmail, demoAdminName, &out.admin},
		{demoViewerEmail, demoViewerName, &out.viewer},
	} {
		id, err := s.auth.Register(ctx, auth.RegisterInput{
			Email: acct.email, Name: acct.name, Password: demoPassword,
		})
		if err != nil {
			return out, fmt.Errorf("create demo account %s: %w", acct.email, err)
		}
		*acct.into = id
	}
	fmt.Fprintf(os.Stderr, "accounts: %s, %s\n", demoAdminEmail, demoViewerEmail)
	return out, nil
}

// seedInvitations issues three and redeems two, so the invitations page shows
// both halves of the lifecycle.
//
// The redemptions are real: invite.Service.Redeem verifies the password, honours
// the D27 address binding, writes the organization-wide membership and records
// invitation.redeemed against the person who joined. The token is recovered from
// the copyable URL because that is the only place it exists — the row stores a
// hash — which is also the property that makes the outstanding invitation usable
// on a mailer-free instance.
func (s *demoSeeder) seedInvitations(ctx context.Context, people *demoPeople) error {
	for _, inv := range []struct {
		email, name, role string
	}{
		{demoAdminEmail, demoAdminName, "admin"},
		{demoViewerEmail, demoViewerName, "viewer"},
	} {
		created, err := s.invite.Create(ctx, s.owner, invite.CreateInput{
			Email: inv.email, Role: inv.role,
		})
		if err != nil {
			return fmt.Errorf("invite %s: %w", inv.email, err)
		}
		if created.Emailed {
			// Unreachable with a nil Enqueuer, and asserted rather than assumed:
			// a demo that queued mail would have needed a relay to run.
			return fmt.Errorf("invitation to %s was queued for delivery; "+
				"the demo seeder must need no mailer", inv.email)
		}
		if _, err := s.invite.Redeem(ctx, invite.RedeemInput{
			Token:    demoInviteToken(created.URL),
			Email:    inv.email,
			Name:     inv.name,
			Password: demoPassword,
		}); err != nil {
			return fmt.Errorf("redeem invitation for %s: %w", inv.email, err)
		}
	}

	pending, err := s.invite.Create(ctx, s.owner, invite.CreateInput{
		Email: demoPendingEmail, Role: "editor",
	})
	if err != nil {
		return fmt.Errorf("invite %s: %w", demoPendingEmail, err)
	}
	fmt.Fprintf(os.Stderr, "invitations: 2 redeemed, 1 outstanding for %s\n", demoPendingEmail)
	fmt.Fprintf(os.Stderr, "  outstanding link: %s\n", pending.URL)

	// The memberships the redemptions produced are what the members page reads;
	// re-resolving here is what lets the rest of the seeding act as these people.
	// In place, on the caller's value: an identity resolved before the membership
	// existed still points at the personal workspace registration gave them, and
	// everything seeded with it would land in the wrong organization.
	return s.refresh(ctx, people)
}

// demoInviteToken recovers the raw token from a copyable invitation URL.
func demoInviteToken(url string) string {
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return url
	}
	return url[i+1:]
}

// refresh re-resolves the seeded people after their memberships changed.
func (s *demoSeeder) refresh(ctx context.Context, people *demoPeople) error {
	for _, p := range []struct {
		email string
		into  **auth.Identity
	}{
		{demoAdminEmail, &people.admin},
		{demoViewerEmail, &people.viewer},
	} {
		// Everyone joined an organization they were not in a moment ago, and the
		// demo wants them acting there rather than in the personal workspace
		// registration gave them. SwitchWorkspace is the product's answer and is
		// session-only by design (M25) — a credential without a browser must not
		// move somebody's browser — so this runs the write that call makes, the
		// last-used one, and then resolves the identity through
		// auth.IdentityForEmail like every other CLI subcommand does. It is one of
		// the three deliberate steps around a service in this file.
		if _, err := s.q.SetLastWorkspaceForUser(ctx, dbgen.SetLastWorkspaceForUserParams{
			UserID: (*p.into).UserID, WorkspaceID: s.owner.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("place %s in the demo workspace: %w", p.email, err)
		}
		id, err := s.auth.IdentityForEmail(ctx, p.email)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", p.email, err)
		}
		*p.into = id
	}
	return nil
}

// seedSecondWorkspace creates it, grants somebody workspace-scoped access to it,
// and fills it with links and traffic of its own.
//
// The switcher M25 built renders nothing on a one-workspace instance, which is
// exactly the state today's demo is in. Two workspaces is the smallest number at
// which the control, and the scoping underneath it, can be looked at.
func (s *demoSeeder) seedSecondWorkspace(ctx context.Context, people demoPeople) error {
	ws, err := s.team.CreateWorkspace(ctx, s.owner, demoWorkspace2Name)
	if err != nil {
		return fmt.Errorf("create %s workspace: %w", demoWorkspace2Name, err)
	}

	// An organization-wide viewer who is an editor in one workspace. D31's rule
	// is that a grant adds and never narrows, and it is unreadable as prose and
	// obvious as two rows on the members page.
	if _, err := s.team.Grant(ctx, s.owner, team.GrantInput{
		UserID: people.viewer.UserID, WorkspaceID: ws.ID, Role: "editor",
	}); err != nil {
		return fmt.Errorf("grant %s access to %s: %w", demoViewerEmail, ws.Name, err)
	}

	// The owner acts in the new workspace to fill it. Same deliberate step around
	// SwitchWorkspace as refresh takes, and undone at the end so the demo opens
	// where it always did.
	here, err := s.actAs(ctx, s.owner.Email, ws.ID)
	if err != nil {
		return err
	}
	cat := demoWorkspace2Catalogue()
	ids, err := demoCreateLinks(ctx, s.pool, s.link, here, cat, s.now)
	if err != nil {
		return fmt.Errorf("create %s links: %w", ws.Name, err)
	}
	clicks, err := demoClicks(ctx, s.pool, ws.ID.String(), cat, ids, s.opt, s.now)
	if err != nil {
		return fmt.Errorf("click history for %s: %w", ws.Name, err)
	}
	if _, err := s.actAs(ctx, s.owner.Email, s.owner.WorkspaceID); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "workspace %q: %d links, %d clicks\n", ws.Name, len(ids), clicks)
	return nil
}

// actAs resolves an identity acting in a named workspace. See refresh for why
// the last-used write is made directly.
func (s *demoSeeder) actAs(ctx context.Context, email string, wsID uuid.UUID) (*auth.Identity, error) {
	id, err := s.auth.IdentityForEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", email, err)
	}
	if _, err := s.q.SetLastWorkspaceForUser(ctx, dbgen.SetLastWorkspaceForUserParams{
		UserID: id.UserID, WorkspaceID: wsID,
	}); err != nil {
		return nil, fmt.Errorf("place %s in workspace %s: %w", email, wsID, err)
	}
	return s.auth.IdentityForEmail(ctx, email)
}

// seedBotBlocking switches it on for exactly one link.
//
// One, and not the domain: the domain setting decides for every link on the
// instance at once, and turning it on would make every other link's `inherit`
// mean "blocked" — which is the state in which the precedence table M32.5 built
// is invisible. One link on, everything else inheriting a domain that blocks
// nothing, is the arrangement in which both halves can be read off the page.
func (s *demoSeeder) seedBotBlocking(ctx context.Context, cat []demoLink, ids map[int]uuid.UUID) error {
	const alias = "intro-video"
	id, ok := demoLinkID(cat, ids, alias)
	if !ok {
		return fmt.Errorf("demo catalogue has no %q link to switch bot blocking on", alias)
	}
	policy := domain.BotBlock
	if _, err := s.link.Update(ctx, s.owner, id, link.UpdateInput{BotBlocking: &policy}); err != nil {
		return fmt.Errorf("switch bot blocking on for %s: %w", alias, err)
	}
	fmt.Fprintf(os.Stderr, "bot blocking: on for /%s, inherited by every other link\n", alias)
	return nil
}

// demoRuleLink is the link the routing rules hang off.
//
// One link with several rules rather than several links with one each, because
// what M34 actually built is *ordered* evaluation — priority, first match,
// short-circuit — and a single rule anywhere demonstrates none of it. The
// summer sale is the right link for it: a seasonal campaign routed by where
// somebody is and what they are holding is the reason this feature exists.
const demoRuleLink = "summer-sale"

// seedRoutingRules gives one link a rule list somebody can read top to bottom.
//
// Four rules, chosen so that the list teaches the model rather than showing one
// example four times:
//
//   - **A disabled rule at priority 10.** First in the list, and it changes
//     nothing. That is the whole of what `enabled` means and it is invisible if
//     every seeded rule is on.
//   - **Two enabled rules that both match a British mobile visitor**, at 20 and
//     30. Only the first fires. First-match short-circuiting is the one part of
//     this feature people get wrong when they reason about it, and two
//     overlapping rules is the smallest arrangement that shows it.
//   - **A rule with no geography at all**, matching on a campaign parameter, so
//     the list does not read as though routing means countries.
//
// Seeded through link.Service.CreateRule, like everything else in this file: the
// destinations go through the full M30 tier check on the way in, exactly as a
// rule typed into the dashboard does.
func (s *demoSeeder) seedRoutingRules(ctx context.Context, cat []demoLink, ids map[int]uuid.UUID) error {
	id, ok := demoLinkID(cat, ids, demoRuleLink)
	if !ok {
		return fmt.Errorf("demo catalogue has no %q link to hang routing rules off", demoRuleLink)
	}

	rules := []link.CreateRuleInput{
		{
			// Off. Kept in the list because a rule somebody switched off is a
			// state the page has to be able to show.
			URL: "https://shop.example.com/summer/clearance", Priority: 10, Enabled: false,
			Conditions: domain.RuleConditions{
				Time: &domain.RuleTime{Days: []string{"sat", "sun"}},
			},
		},
		{
			// Wins for a British visitor on a phone, because it is checked first.
			URL: "https://shop.example.com/uk/summer-mobile", Priority: 20, Enabled: true,
			Conditions: domain.RuleConditions{
				Country: []string{"GB"}, Device: []string{"mobile"},
			},
		},
		{
			// Would also match that visitor, and never gets the chance.
			URL: "https://shop.example.com/uk/summer", Priority: 30, Enabled: true,
			Conditions: domain.RuleConditions{Country: []string{"GB"}},
		},
		{
			// Nothing geographic: whoever arrives from the newsletter, wherever
			// they are, lands on the page the newsletter promised.
			URL: "https://shop.example.com/summer/newsletter", Priority: 40, Enabled: true,
			Conditions: domain.RuleConditions{
				UTM: map[string][]string{"source": {"newsletter"}},
			},
		},
	}

	for _, in := range rules {
		if _, err := s.link.CreateRule(ctx, s.owner, id, in); err != nil {
			return fmt.Errorf("create routing rule for /%s: %w", demoRuleLink, err)
		}
	}
	fmt.Fprintf(os.Stderr, "routing rules: %d on /%s, one of them disabled\n",
		len(rules), demoRuleLink)
	return nil
}

// The four gated links (M35), and their aliases are their explanation.
//
// Four separate links rather than one link wearing four gates, because the
// gates are independent and a single link carrying all of them would show only
// their intersection: a visitor would meet the signature refusal and never learn
// that the other three exist. Somebody opening the demo should be able to click
// each one and find out what it does.
const (
	demoPasswordAlias = "board-deck"
	demoOneTimeAlias  = "access-once"
	demoMaxClickAlias = "first-fifty"
	demoSignedAlias   = "signed-preview"

	// demoLinkPassword is what the password link asks for. In the clear for the
	// same reason demoPassword is: publishing it is the point, and a demo gate
	// nobody can get through teaches only that the gate exists.
	// Written in the clear for the same reason demoPassword is: publishing it is
	// the point, because a demo gate nobody can get through teaches only that
	// the gate exists.
	demoLinkPassword = "demo-link-password" //nolint:gosec // G101: a published demo credential, deliberately

	// demoMaxClicks is the ceiling on the click-limited link. Fifty, and the
	// budget below is spent partway, so the page shows a limit with a number
	// against it rather than a limit nothing has ever touched.
	demoMaxClicks = 50
	// demoClicksSpent is how much of it the demo has already used.
	demoClicksSpent = 12
)

// seedGatedLinks creates one link per gate and spends part of one budget.
//
// Through the link service like everything else here, so each destination goes
// through the full M30 tier check and each password is hashed with the
// instance's own argon2 parameters — a demo whose password was written straight
// into the column would be showing a row shape the product never produces.
func (s *demoSeeder) seedGatedLinks(ctx context.Context) error {
	maxClicks := int64(demoMaxClicks)
	gated := []struct {
		in   link.CreateInput
		what string
	}{
		{link.CreateInput{
			Alias: demoPasswordAlias, URL: "https://example.com/investors/board-deck.pdf",
			Title: "Board deck (password)",
			Description: "Asks for a password before it redirects. Nothing is stored in the " +
				"visitor's browser, so coming back means typing it again.",
			Password: demoLinkPassword,
		}, "password " + demoLinkPassword},
		{link.CreateInput{
			Alias: demoOneTimeAlias, URL: "https://example.com/onboarding/welcome",
			Title:       "One-time access link",
			Description: "Works once. The second visit is a 410, counted in Postgres rather than in the cache.",
			OneTime:     true,
		}, "one-time"},
		{link.CreateInput{
			Alias: demoMaxClickAlias, URL: "https://example.com/offers/early-bird",
			Title:       "First fifty only",
			Description: "Stops at fifty clicks. The count beside it is exact, unlike the click total.",
			MaxClicks:   &maxClicks,
		}, fmt.Sprintf("max %d clicks", demoMaxClicks)},
		{link.CreateInput{
			Alias: demoSignedAlias, URL: "https://example.com/press/embargoed",
			Title: "Signed link only",
			Description: "The plain short URL is refused with 403. Only a signed, unexpired " +
				"URL works — make one from this link's page.",
			RequireSignature: true,
		}, "signature required"},
	}

	ids := make(map[string]uuid.UUID, len(gated))
	for _, g := range gated {
		created, err := s.link.Create(ctx, s.owner, g.in)
		if err != nil {
			return fmt.Errorf("create gated link /%s: %w", g.in.Alias, err)
		}
		ids[g.in.Alias] = created.ID
		fmt.Fprintf(os.Stderr, "gated link: /%s — %s\n", g.in.Alias, g.what)
	}

	// Part of the click-limited link's budget, spent through the same statement
	// the redirect path uses. A limit with a zero against it looks like a limit
	// that does not work; a partly-spent one is the state the control is
	// actually for.
	//
	// One of the three deliberate steps around a service is not what this is: it
	// goes through internal/gate, which is where the redirect path consumes a
	// click too. It is a loop rather than one call because the counter is
	// deliberately incremental — there is no "set it to twelve", and adding one
	// would be a way to write a number the gate never produced.
	for range demoClicksSpent {
		if _, err := s.gates.Consume(ctx, ids[demoMaxClickAlias], s.owner.WorkspaceID,
			int64(demoMaxClicks)); err != nil {
			return fmt.Errorf("spend part of /%s's budget: %w", demoMaxClickAlias, err)
		}
	}

	// A signed URL for the signature-gated link, printed for whoever is looking
	// at the demo. Without it that link is a 403 with no way past it, which
	// demonstrates the refusal and nothing else — and minting it here is also
	// what brings the workspace's signing secret into existence.
	signed, err := s.link.Sign(ctx, s.owner, ids[demoSignedAlias], 30*24*time.Hour)
	if err != nil {
		return fmt.Errorf("sign /%s: %w", demoSignedAlias, err)
	}
	fmt.Fprintf(os.Stderr, "  signed link: %s\n", signed.URL)
	return nil
}

// demoGatedAliases are the gated links the reset removes. Beside the constants
// above, so adding one cannot be done without teaching the reset about it.
func demoGatedAliases() []string {
	return []string{demoPasswordAlias, demoOneTimeAlias, demoMaxClickAlias, demoSignedAlias}
}

// demoLinkID finds a created link by its catalogue alias.
func demoLinkID(cat []demoLink, ids map[int]uuid.UUID, alias string) (uuid.UUID, bool) {
	for i, d := range cat {
		if d.alias == alias {
			id, ok := ids[i]
			return id, ok
		}
	}
	return uuid.Nil, false
}

// seedBlockingAndDisputes produces the refusals and the queue that argues with
// them: a destination the instance blocks, and one dispute in each of the three
// states.
//
// Every refusal below happens the way a real one does — a link create through
// the link service, refused by a tier, recording `destination.blocked` with the
// attempted URL defanged in the metadata. Nothing writes a dispute row; each is
// filed by dispute.Service.File, which re-judges the destination and would
// refuse to file about one that is not actually blocked.
//
// The admin files them rather than the owner. That is not decoration: the queue
// shows who filed, the decision notifies them, and a demo where every row says
// the same name teaches nothing about either.
func (s *demoSeeder) seedBlockingAndDisputes(ctx context.Context, people demoPeople) error {
	if err := s.listDemoHost(ctx); err != nil {
		return err
	}

	type appeal struct {
		url string
		// decide is "", "allow" or "uphold".
		decide string
	}
	// The allowed one is the seeder's own host, and it is the only one an allow
	// touches. The other two argue with rows migration 01500 seeded and which it
	// never re-asserts, so allowing either would retire a shortener from this
	// instance's blocklist permanently — and the next --reset could not put it
	// back without re-asserting a row an owner may have deleted on purpose.
	for _, a := range []appeal{
		{demoBlockedURL, "allow"},
		{demoUpheldURL, "uphold"},
		{demoOpenURL, ""},
	} {
		if err := s.refuseAndFile(ctx, people.admin, a.url, a.decide); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "disputes: one open, one allowed, one upheld")
	return nil
}

// listDemoHost adds the demo's own host to the runtime low-confidence blocklist.
//
// Written directly, and it is the second of the three deliberate steps around a
// service. Nothing in the product inserts into this table: boot reconciles
// LINKCTRL_DESTINATION_BLOCKLIST into it (source 'env'), migration 01500 seeded
// the shortener hosts once, and M31's review queue only ever *removes* a row.
// Neither writer can be borrowed — an 'env' row is deleted by the next boot, and
// dispute.Allow refuses to lift one for exactly that reason, so a demo built on
// one would show an allow that cannot be clicked.
//
// The row therefore goes in as the source the review queue reads, 'review',
// which is the column default and the state an operator's own list entry is in.
func (s *demoSeeder) listDemoHost(ctx context.Context) error {
	const insert = `
		INSERT INTO blocked_destinations (host, reason)
		VALUES ($1, 'Reported by the demo operator as a tracking redirector')
		ON CONFLICT (host) DO NOTHING`
	if _, err := s.pool.Exec(ctx, insert, demoBlockedHost); err != nil {
		return fmt.Errorf("list %s: %w", demoBlockedHost, err)
	}
	return nil
}

// refuseAndFile attempts a link the instance will refuse, files the dispute, and
// optionally decides it.
func (s *demoSeeder) refuseAndFile(ctx context.Context, filer *auth.Identity, url, decide string) error {
	// The attempt has to be refused. A destination that was accepted would leave
	// a link in the demo pointing at a tracker and no blocked-attempt record at
	// all, which is the failure worth being loud about.
	_, err := s.link.Create(ctx, filer, link.CreateInput{
		URL: url, Title: "Refused destination (demo)",
	})
	var ve domain.ValidationErrors
	switch {
	case err == nil:
		return fmt.Errorf("demo destination %s was accepted; it must be refused "+
			"for the blocked-attempt record and the dispute to mean anything", url)
	case !errors.As(err, &ve):
		return fmt.Errorf("attempt %s: %w", url, err)
	}

	d, err := s.dispute.File(ctx, filer, url)
	if err != nil {
		return fmt.Errorf("file dispute about %s: %w", url, err)
	}

	switch decide {
	case "allow":
		if _, err := s.dispute.Allow(ctx, s.owner, d.ID); err != nil {
			return fmt.Errorf("allow dispute about %s: %w", url, err)
		}
	case "uphold":
		if _, err := s.dispute.Uphold(ctx, s.owner, d.ID); err != nil {
			return fmt.Errorf("uphold dispute about %s: %w", url, err)
		}
	}
	return nil
}

// readSomeNotifications marks the older half of the owner's inbox read.
//
// An inbox where everything is unread and one where nothing is are both states
// the bell renders badly — the first is a badge that never moves, the second is a
// control nobody notices. Reading the older half leaves an unread count and a
// read/unread distinction to look at, and it is done through MarkRead, which is
// what the bell itself calls.
func (s *demoSeeder) readSomeNotifications(ctx context.Context) error {
	page, err := s.notify.List(ctx, s.owner, notify.Filter{Limit: 100})
	if err != nil {
		return fmt.Errorf("read the owner's inbox: %w", err)
	}
	// Newest first, so the tail is the older half.
	items := page.Items
	read := 0
	for i := len(items) / 2; i < len(items); i++ {
		if err := s.notify.MarkRead(ctx, s.owner, items[i].ID); err != nil {
			return fmt.Errorf("mark notification read: %w", err)
		}
		read++
	}
	fmt.Fprintf(os.Stderr, "notifications: %d for %s, %d unread\n",
		len(items), s.owner.Email, len(items)-read)
	if len(items)-read == 0 {
		return errors.New("the demo owner has no unread notifications; the bell would be empty")
	}
	return nil
}

// demoResetPhase2 removes everything the Phase 2 seeding wrote.
//
// Runs inside demoReset's transaction, and every statement is scoped to the demo
// organization or to the named demo addresses, so it cannot reach a row somebody
// added by hand in another organization. This is what "idempotent under --reset"
// costs: a feature seeded above without a line here makes the second run of
// `make demo-update` differ from the first.
func demoResetPhase2(ctx context.Context, tx pgxExecutor, orgID, userID uuid.UUID) error {
	emails := []string{demoAdminEmail, demoViewerEmail, demoPendingEmail}

	// Routing rules (M34) have no step of their own, and that is a cascade
	// rather than an omission. Their rows hang off `destinations`, which
	// demoReset deletes for every catalogue alias before it deletes the links —
	// and `routing_rules.destination_id` is ON DELETE CASCADE, so the rules go
	// with them. A step here would delete rows that no longer exist.
	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"disputes", `DELETE FROM destination_disputes WHERE host = ANY($1::text[])`,
			[]any{demoDisputedHosts()}},
		// Scoped to 'review' so a shortener row seeded by migration 01500 can
		// never be deleted here, whatever ends up in the host list above.
		{"blocklist entry",
			`DELETE FROM blocked_destinations WHERE host = $1 AND source = 'review'`,
			[]any{demoBlockedHost}},

		// The gated links (M35), and their durable click budgets with them —
		// link_click_budget is ON DELETE CASCADE from links, so the budget rows
		// need no step of their own. Destinations are deleted first for the same
		// reason the main reset does it: links.primary_destination_id points at
		// one, and the delete order is what keeps that foreign key satisfied.
		{"gated link destinations", `
			DELETE FROM destinations WHERE link_id IN (
				SELECT l.id FROM links l
				 WHERE l.workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
				   AND l.alias = ANY($2::text[]))`,
			[]any{orgID, demoGatedAliases()}},
		{"gated links", `
			DELETE FROM links
			 WHERE workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
			   AND alias = ANY($2::text[])`,
			[]any{orgID, demoGatedAliases()}},

		{"invitations", `DELETE FROM invitations WHERE organization_id = $1`, []any{orgID}},
		{"notifications", `DELETE FROM notifications WHERE user_id = $1`, []any{userID}},
		{"audit records", `DELETE FROM audit_logs WHERE organization_id = $1`, []any{orgID}},
		// Cascades the workspace's links, tags and destinations.
		{"second workspace",
			`DELETE FROM workspaces WHERE organization_id = $1 AND slug = $2`,
			[]any{orgID, demoWorkspace2Slug}},
		// The personal organizations registration gave the seeded accounts. No
		// foreign key removes them with the user, and leaving them behind would
		// make the next run's organization list grow every time.
		//
		// Two guards, and the first exists because its absence deleted the demo
		// itself. The organization the demo lives in is the owner's *personal*
		// one — auth.Register provisions every account that way — and the seeded
		// people are members of it, so "personal organizations the seeded people
		// belong to" reached it on the second run and took the workspace, the
		// links and the owner's membership with it. Excluding the demo
		// organization by id is the narrow fix; requiring that every remaining
		// member is one of the seeded accounts is the one that would have caught
		// it, and both are cheap.
		{"seeded personal organizations", `
			DELETE FROM organizations o
			 WHERE o.is_personal
			   AND o.id <> $1
			   AND EXISTS (
			       SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
			        WHERE m.organization_id = o.id AND u.email_lower = ANY($2::text[]))
			   AND NOT EXISTS (
			       SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
			        WHERE m.organization_id = o.id
			          AND (u.email_lower IS NULL OR NOT (u.email_lower = ANY($2::text[]))))`,
			[]any{orgID, emails}},
		// Last: cascades memberships, sessions and notifications.
		{"seeded accounts", `DELETE FROM users WHERE email_lower = ANY($1::text[])`, []any{emails}},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			return fmt.Errorf("reset %s: %w", st.what, err)
		}
	}
	return nil
}
