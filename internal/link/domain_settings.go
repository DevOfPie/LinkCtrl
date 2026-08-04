package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// DomainSettings is what an operator can configure about the hostname short
// links are served on.
type DomainSettings struct {
	Hostname string `json:"hostname"`
	// RootRedirectURL is where https://<link host>/ sends a visitor. Empty means
	// the root answers 404, which is the default and reveals nothing.
	RootRedirectURL string `json:"root_redirect_url,omitempty"`
	// SplitHosts reports whether the setting is in effect at all. On a
	// single-host deployment the root belongs to the dashboard.
	SplitHosts bool `json:"split_hosts"`

	// BlockBots is the domain's own answer, inherited by every link that has not
	// said otherwise. BlockBotsEnforced additionally overrules the ones that
	// have. Unlike the root redirect, neither depends on split hosts: short
	// links are served on this instance either way, and so are the crawlers
	// asking for them.
	BlockBots         bool `json:"block_bots"`
	BlockBotsEnforced bool `json:"block_bots_enforced"`
}

// DomainSettings reads the link domain's settings.
//
// Readable by anyone who can read links: it is one URL an operator chose, and
// every visitor to the bare domain sees where it points anyway.
func (s *Service) DomainSettings(ctx context.Context, actor *auth.Identity) (*DomainSettings, error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading domain settings requires %s", domain.ErrForbidden, PermRead)
	}
	id, err := s.defaultDomainID(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetDefaultDomainSettings(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read domain settings: %w", err)
	}
	out := &DomainSettings{
		Hostname:          row.Hostname,
		SplitHosts:        s.splitHosts,
		BlockBots:         row.BlockBots,
		BlockBotsEnforced: row.BlockBotsEnforced,
	}
	if row.RootRedirectUrl != nil {
		out.RootRedirectURL = *row.RootRedirectUrl
	}
	return out, nil
}

// LinkDomainBots is the bot policy of the domain one link is served on, and the
// hostname to name when explaining it.
type LinkDomainBots struct {
	Hostname          string
	BlockBots         bool
	BlockBotsEnforced bool
}

// LinkDomainBots reads the bot policy that applies to one link.
//
// **The link's own domain, which is not always the instance default (F89).** The
// link detail page read `DomainSettings` for every link, which is the default
// domain's row whatever hostname the link is served on: on a link served from a
// verified custom hostname (M40) the page disabled a control the API would have
// accepted and named the wrong hostname in the sentence explaining why. The API's
// own guard has always read the right row — `Update` asks
// `GetDomainBotSettings(existing.DomainID)` before refusing an `off` the domain
// enforces — so this is the page being brought to where the API already was, and
// m32.5.md's "asserted by test at both surfaces" becomes true again.
//
// Guarded by links.read and scoped to the actor's workspace, because a hostname
// is a thing worth not leaking: reading it through a link id must not answer for
// a link the caller cannot see.
func (s *Service) LinkDomainBots(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) (*LinkDomainBots, error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading domain settings requires %s", domain.ErrForbidden, PermRead)
	}
	l, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}
	row, err := s.q.GetDomainBotSettings(ctx, l.DomainID)
	if err != nil {
		return nil, fmt.Errorf("read domain bot settings: %w", err)
	}
	return &LinkDomainBots{
		Hostname:          row.Hostname,
		BlockBots:         row.BlockBots,
		BlockBotsEnforced: row.BlockBotsEnforced,
	}, nil
}

// SetRootRedirect points the link domain's root somewhere, or clears it.
//
// Three refusals, each of which would otherwise be discovered late.
//
// It needs domains.write rather than links.update: this is not one link, it is
// where every visitor who trims a short link back to its domain ends up.
//
// It is refused outright on a single-host deployment. There "/" is the
// dashboard, and honouring this would take the dashboard away from the person
// who just set it — a failure that reads as the product breaking rather than as
// a setting doing what it says.
//
// The destination goes through exactly the same validation as a link's, which
// matters more here than anywhere: a root redirect that skipped the private,
// loopback and metadata refusals would be a cleaner SSRF than the one the
// validator exists to prevent, because reaching it needs no link and no alias.
func (s *Service) SetRootRedirect(ctx context.Context, actor *auth.Identity, rawURL string) (*DomainSettings, error) {
	if !actor.Can(PermDomainsWrite) {
		return nil, fmt.Errorf("%w: changing domain settings requires %s",
			domain.ErrForbidden, PermDomainsWrite)
	}
	if !s.splitHosts {
		return nil, domain.ValidationErrors{{
			Field: "root_redirect_url", Code: "not_applicable",
			Message: "the link domain root is the dashboard on a single-host deployment; " +
				"set LINK_BASE_URL to a separate host first",
		}}
	}

	var stored *string
	if trimmed := strings.TrimSpace(rawURL); trimmed != "" {
		// The same check as a link's, through the same door — which matters more
		// here than anywhere. A root redirect that skipped the tiers would be a
		// cleaner attack than the one they exist to prevent, because reaching it
		// needs no link and no alias, only the bare domain. checkDestination
		// reports against root_redirect_url rather than url, so the form
		// highlights the box the operator typed in.
		normalized, err := s.checkDestination(ctx, actor, trimmed, surfaceRootRedirect)
		if err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				return nil, ve
			}
			return nil, err
		}
		stored = &normalized
	}

	// Read before the write, because the audit record is worth little without
	// what it replaced: "the root now points at example.com" does not tell an
	// operator whether that was a change or a no-op, and the previous value is
	// unrecoverable a moment later.
	id, err := s.defaultDomainID(ctx)
	if err != nil {
		return nil, err
	}
	previous := ""
	if before, berr := s.q.GetDefaultDomainSettings(ctx, id); berr == nil && before.RootRedirectUrl != nil {
		previous = *before.RootRedirectUrl
	}

	row, err := s.q.SetDefaultDomainRootRedirect(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("set root redirect: %w", err)
	}

	// The audit event M20 promised. This is one setting that redirects every
	// stray visitor to the whole domain, which is the class of change worth
	// being able to ask about months later.
	//
	// After the write and outside it: the change is what the operator asked
	// for, and failing it because the record could not be written would trade a
	// missing audit line for a setting that did not take effect. Logged at warn
	// rather than swallowed, so the gap is visible to whoever goes looking.
	if s.audit != nil {
		to := ""
		if row.RootRedirectUrl != nil {
			to = *row.RootRedirectUrl
		}
		if err := s.audit.Record(ctx, actor, audit.Event{
			Action:     audit.ActionDomainRootRedirectChanged,
			TargetType: "domain",
			TargetID:   &row.ID,
			Metadata: map[string]any{
				"hostname": row.Hostname,
				"from":     previous,
				"to":       to,
			},
			// F36. `SetDefaultDomainRootRedirect` targets `WHERE is_default`, so
			// this is unambiguously the instance's own hostname and not any
			// tenant's — the row it writes to has a NULL organization_id, and the
			// audit record now says the same thing the domain row does.
			//
			// The per-domain events at recordDomainEvent are deliberately *not*
			// marked: a registered hostname belongs to an organization, and its
			// record belongs there too.
			InstanceWide: true,
		}); err != nil {
			s.log.Warn("root redirect changed but the audit record was not written",
				slog.String("hostname", row.Hostname), slog.Any("error", err))
		}
	}

	// The redirect tree caches this; without invalidation the change waits out
	// the TTL on the one URL an operator is most likely to reload immediately
	// to check their work.
	if s.rootCache != nil {
		s.rootCache.InvalidateRoot()
	}

	out := &DomainSettings{
		Hostname:          row.Hostname,
		SplitHosts:        true,
		BlockBots:         row.BlockBots,
		BlockBotsEnforced: row.BlockBotsEnforced,
	}
	if row.RootRedirectUrl != nil {
		out.RootRedirectURL = *row.RootRedirectUrl
	}
	return out, nil
}

// SetBotBlocking turns bot blocking on or off for every link on the instance,
// and decides whether a link may overrule it.
//
// Guarded by domains.write rather than links.update, and the reason is the same
// one the root redirect has: one hostname serves every workspace on this
// instance, so this is not a setting about some links. Enforcing it decides for
// all of them, including links whose owners deliberately turned blocking off.
//
// **It reaches every domain row, not only the default (F89).** The redirect path
// reads the policy from the link's own domain — `ResolveAliasForRedirect` joins
// on `l.domain_id` — so while this wrote only the `is_default` row, a link served
// on a verified custom hostname (M40) was never blocked whatever the operator
// set, and D71 makes a workspace's own hostname the default for its new links, so
// the hole opened without anybody choosing it. `SetBotBlockingForEveryDomain` is
// the writer and `CreateDomain` inherits the current answer, which together are
// what the word *instance-wide* has always claimed.
//
// Widening the setter to take a domain id instead was the other way to close it
// and it is the wrong one: the guard here is `domains.write`, which F70 records
// as reaching every organization's owner and admin on a multi-organization
// instance, so a per-hostname policy would let a workspace switch off an
// enforcement the operator set — a wider hole than the one being closed. Per-
// domain settings are D69's parked question and they need the instance-level
// principal D38 does not have.
//
// Unlike the root redirect it is NOT refused on a single-host deployment. That
// refusal exists because "/" belongs to the dashboard there and honouring the
// setting would take the dashboard away; nothing of the sort applies here.
// Short links are served on a single-host instance exactly as on a split one,
// and so is the crawler traffic this refuses.
//
// The cost of switching this on is worth stating where the operator's own
// documentation will repeat it: analytics.Classify decides who is a bot, it
// matches substrings including "preview", "monitor" and "checker", it treats an
// absent user agent as automated, and its false-positive rate has never been
// measured because until this milestone nothing depended on it. A person it
// misclassifies gets a 403 and has no way past it — the bypass is Phase 3 — and
// nobody tells the link's owner it happened.
func (s *Service) SetBotBlocking(ctx context.Context, actor *auth.Identity, block, enforced bool) (*DomainSettings, error) {
	if !actor.Can(PermDomainsWrite) {
		return nil, fmt.Errorf("%w: changing domain settings requires %s",
			domain.ErrForbidden, PermDomainsWrite)
	}
	// Enforcement without blocking is not a state. The database refuses it too
	// (01800's CHECK), but a constraint violation reaches the caller as a 500
	// describing a constraint name, and the person who ticked one box wants to
	// be told which box to tick.
	if enforced && !block {
		return nil, domain.ValidationErrors{{
			Field: "block_bots_enforced", Code: "requires_blocking",
			Message: "enforcing bot blocking requires turning it on for the domain first",
		}}
	}

	// Read before the write, for the audit record: "bot blocking is on" does not
	// say whether that was a change, and the previous value is gone a moment
	// later.
	id, err := s.defaultDomainID(ctx)
	if err != nil {
		return nil, err
	}
	before, err := s.q.GetDefaultDomainSettings(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read domain settings: %w", err)
	}

	rows, err := s.q.SetBotBlockingForEveryDomain(ctx, dbgen.SetBotBlockingForEveryDomainParams{
		BlockBots: block, BlockBotsEnforced: enforced,
	})
	if err != nil {
		return nil, fmt.Errorf("set domain bot blocking: %w", err)
	}
	// The instance default is what every settings surface reads and names, so it
	// is what this returns and what the audit record is about. Its absence is not
	// a state this instance has — 00700 seeds the row and `defaultDomainID` above
	// has already read it — so it is reported rather than papered over with a
	// zero value that would tell the caller their change did not happen.
	var row dbgen.SetBotBlockingForEveryDomainRow
	found := false
	for _, r := range rows {
		if r.IsDefault {
			row, found = r, true
			break
		}
	}
	if !found {
		return nil, errors.New("set domain bot blocking: the instance default domain was not updated")
	}

	changed := before.BlockBots != row.BlockBots ||
		before.BlockBotsEnforced != row.BlockBotsEnforced
	if s.audit != nil && changed {
		// After the write and outside it, like the root redirect: the change is
		// what the operator asked for, and failing it because the record could
		// not be written trades a missing log line for a setting that did not
		// take effect.
		if err := s.audit.Record(ctx, actor, audit.Event{
			Action:     audit.ActionDomainBotBlockingChanged,
			TargetType: "domain",
			TargetID:   &row.ID,
			// The folded three-state value rather than the two booleans: "on"
			// and "enforced" are what the change means, and a reader should not
			// have to recombine a pair of flags to see that every link on the
			// instance just lost the ability to opt out.
			Metadata: map[string]any{
				"hostname": row.Hostname,
				"from":     string(domain.DomainBots(before.BlockBots, before.BlockBotsEnforced)),
				"to":       string(domain.DomainBots(row.BlockBots, row.BlockBotsEnforced)),
			},
			// F36. This write reaches every domain row on the box, so the change
			// governs every link in every organization — and it was recorded in
			// exactly one of them, the actor's, invisible to all the rest. The
			// record now belongs to the instance and is read under
			// audit.read.instance.
			InstanceWide: true,
		}); err != nil {
			s.log.Warn("domain bot blocking changed but the audit record was not written",
				slog.String("hostname", row.Hostname), slog.Any("error", err))
		}
	}

	// Every link on every domain just got a different answer, and every cached
	// snapshot carries the old one. This is the expensive invalidation and it is
	// the direct price of the redirect path needing no second lookup; see
	// redirect.Resolver.InvalidateDomain.
	//
	// One per domain, because the cache is keyed by domain and there is no
	// wildcard sweep — which is also the cost of the write having been widened
	// (F89): an instance with n registered hostnames pays n sweeps on this form
	// submission instead of one, each bounded by domainSweepBudget. It is bounded
	// work on an operator action that happens about as often as somebody changes
	// their mind about crawlers, and the alternative is links that keep applying
	// the previous policy until their TTL expires.
	//
	// Unconditional rather than skipped when nothing changed. A no-op write that
	// left a stale cache in place would be indistinguishable from a change that
	// did not take effect.
	if s.cache != nil {
		for _, r := range rows {
			s.cache.InvalidateDomain(ctx, r.ID)
		}
	}

	out := &DomainSettings{
		Hostname:          row.Hostname,
		SplitHosts:        s.splitHosts,
		BlockBots:         row.BlockBots,
		BlockBotsEnforced: row.BlockBotsEnforced,
	}
	if row.RootRedirectUrl != nil {
		out.RootRedirectURL = *row.RootRedirectUrl
	}
	return out, nil
}

// LoadRootRedirect reads the current value for the redirect path. Unexported
// callers only: the hot path uses it through a cache.
func (s *Service) LoadRootRedirect(ctx context.Context) (string, error) {
	id, err := s.defaultDomainID(ctx)
	if err != nil {
		return "", err
	}
	row, err := s.q.GetDefaultDomainSettings(ctx, id)
	if err != nil {
		return "", err
	}
	if row.RootRedirectUrl == nil {
		return "", nil
	}
	return *row.RootRedirectUrl, nil
}

// defaultDomainID names the instance default, which the settings queries used to
// find with `WHERE is_default` inside every statement.
//
// M40 gave those statements a domain filter, because a verified custom hostname
// has settings of its own and a predicate that could only ever return the
// default would answer about the wrong row. The flag is still how the default is
// found — it just happens once, here, rather than in four queries.
func (s *Service) defaultDomainID(ctx context.Context) (uuid.UUID, error) {
	row, err := s.q.ResolveDefaultDomain(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve default domain: %w", err)
	}
	return row.ID, nil
}

// SetDomainRootRedirect points one registered hostname's root somewhere.
//
// The per-domain half of a setting that has existed since 00800 for the instance
// default. A custom hostname is a bare domain somebody will type into a browser,
// and whether that answers 404 or goes to the workspace's own site is the
// workspace's choice rather than the operator's.
//
// Three things are deliberately not shared with the instance-default version.
// It is **not** refused on a single-host deployment: the custom hostname is not
// the dashboard's host whatever LINK_BASE_URL says, so its root belongs to
// nobody else. It is guarded by the **ownership** check rather than by bare
// `domains.write`, because this is one workspace's hostname. And it is refused
// on an **unverified** hostname, because nothing is served there — offering to
// configure where its root points would be offering a setting with no effect.
//
// The destination goes through the same validation a link's does, which matters
// here for the reason it matters on the instance root: a redirect reachable with
// no alias and no link is the cleanest SSRF this product could offer.
func (s *Service) SetDomainRootRedirect(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, rawURL string,
) (*Domain, error) {
	row, err := s.domainForWrite(ctx, actor, id, "configuring")
	if err != nil {
		return nil, err
	}
	if row.VerifiedAt == nil {
		return nil, domain.ValidationErrors{{
			Field: "root_redirect_url", Code: "unverified",
			Message: "nothing is served on " + row.Hostname + " until it is verified, so its " +
				"root has nowhere to send anybody yet",
		}}
	}

	var stored *string
	if trimmed := strings.TrimSpace(rawURL); trimmed != "" {
		normalized, derr := s.checkDestination(ctx, actor, trimmed, surfaceRootRedirect)
		if derr != nil {
			var ve domain.ValidationErrors
			if errors.As(derr, &ve) {
				return nil, ve
			}
			return nil, derr
		}
		stored = &normalized
	}

	previous := ""
	if row.RootRedirectUrl != nil {
		previous = *row.RootRedirectUrl
	}
	updated, err := s.q.SetDomainRootRedirect(ctx, dbgen.SetDomainRootRedirectParams{
		ID: id, RootRedirectUrl: stored,
	})
	if err != nil {
		return nil, fmt.Errorf("set domain root redirect: %w", err)
	}

	to := ""
	if updated.RootRedirectUrl != nil {
		to = *updated.RootRedirectUrl
	}
	// The same audit action the instance default's root redirect writes, against
	// a different target id. One action rather than two, because it is one kind
	// of change — where a bare hostname sends a visitor — and a reader filtering
	// the log for it should not have to know which of two names it was called.
	s.recordDomainEvent(ctx, actor, audit.ActionDomainRootRedirectChanged, updated.ID,
		map[string]any{"hostname": updated.Hostname, "from": previous, "to": to})

	// The root URL travels in the verified-hostname set, so this is the same
	// broadcast a verification sends rather than the instance root's own cache
	// invalidation — there is no second cache to clear.
	s.invalidateHosts(ctx)

	links, err := s.q.CountLinksOnDomain(ctx, updated.ID)
	if err != nil {
		return nil, fmt.Errorf("count links on domain: %w", err)
	}
	out := domainFromRow(updated, links, true)
	out.Verification = s.verificationOf(updated)
	return out, nil
}
