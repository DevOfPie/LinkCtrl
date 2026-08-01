package link

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/feed"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Tier is how much confidence a refusal carries, and therefore what it costs to
// overrule.
//
// Two threat models wear one name here, and the tier is what tells them apart.
// Phase 1's refusals protect *this instance* from being used as an SSRF proxy.
// The other two tiers protect *visitors* from a destination hostile to them.
// They must not share an override switch, because the party the first protects
// is not the party who would be appealing: an owner who could approve
// 169.254.169.254 on request would have turned the review queue into the SSRF
// the validator exists to prevent.
type Tier string

const (
	// TierUnappealable is Phase 1's SSRF refusals. Nothing overrules it — no
	// configuration, no list entry, no review. There is deliberately no field,
	// flag or row anywhere in this program that turns it off, and
	// TestUnappealableTierHasNoOverrideSwitch fails if one appears.
	TierUnappealable Tier = "unappealable"

	// TierHighConfidence is the curated embedded list in blocked_hosts.txt.
	// Overruled by editing that file and rebuilding, which is the point: the
	// dangerous override is a reviewable, version-controlled change rather than
	// a click at 2am.
	TierHighConfidence Tier = "high_confidence"

	// TierLowConfidence is the heuristics and the runtime Postgres blocklist.
	// Overruled by the instance owner from M31's review queue, without a
	// rebuild, because a heuristic false-positive rate is unknown until real
	// use and a tier that guesses needs a cheap way to be wrong.
	TierLowConfidence Tier = "low_confidence"
)

// Rules, one per way a destination can be refused. Named constants because they
// are stored in the audit log, returned in a 422 and read by operators, so a
// typo is a silently different vocabulary rather than a compile error.
const (
	// Unappealable.
	RulePrivateAddress  = "private_address"
	RuleSchemeForbidden = "scheme_not_allowed"

	// High confidence.
	RuleEmbeddedHost = "embedded_host"

	// Low confidence, from the runtime list.
	RuleOperatorBlocklist = "operator_blocklist"
	RuleShortenerChain    = "shortener_chain"

	// Low confidence, computed.
	RulePunycodeHomograph = "punycode_homograph"
	RuleURLCredentials    = "url_credentials"

	// RuleFeedReputation is the opt-in third-party feed (M32). Low confidence
	// like everything else that guesses, and low confidence for a second reason
	// the others do not have: the claim is somebody else's, made about a URL
	// they were sent, and this instance cannot check their working.
	RuleFeedReputation = "feed_reputation"
)

// Sources a row of the runtime list can carry, and the vocabulary the `source`
// column is written and reconciled against.
//
// Every reconciliation this program runs is scoped to exactly one of these,
// which is what keeps them from erasing each other: the boot-time rewrite of the
// environment list deletes SourceEnv rows and nothing else, so neither a host an
// operator added by hand nor a seeded shortener can be retired by a restart.
const (
	// SourceEnv is LINKCTRL_DESTINATION_BLOCKLIST, rewritten at every boot.
	SourceEnv = "env"

	// SourceReview is what a person added — the column default, and what M31's
	// review queue will write.
	SourceReview = "review"

	// SourceShortener is the known URL-shortener hosts, seeded by migration
	// 01500 rather than compiled into the binary (D39). A list is compiled when
	// overruling it should be hard; a shortener host is neither a structural
	// claim nor an authoritative one, and a match on it only raises a
	// low-confidence flag the owner may overrule.
	SourceShortener = "shortener"
)

// Code is the reason code a refusal carries into its 422 and its audit record.
//
// "<tier>.<rule>", so one string answers both of the questions somebody reading
// a refusal has: how sure was it, and what did it match. A client that only
// cares whether an appeal is possible reads the part before the dot.
func (t Tier) Code(rule string) string { return string(t) + "." + rule }

// tierOf splits a reason code back into its parts. Reports false for the
// validation codes that are not refusals by a tier at all — a URL that is empty,
// over-long or unparseable is a typo, not an attempt at anything, and recording
// it as a blocked attempt would bury the ones that matter.
func tierOf(code string) (Tier, string, bool) {
	tier, rule, ok := strings.Cut(code, ".")
	if !ok {
		return "", "", false
	}
	switch Tier(tier) {
	case TierUnappealable, TierHighConfidence, TierLowConfidence:
		return Tier(tier), rule, true
	}
	return "", "", false
}

// Block is a refusal by one of the two appealable tiers.
//
// There is no counterpart type meaning "allowed", and that is structural rather
// than an accident of naming: nothing in this package returns permission. A
// destination is accepted by surviving every check, so no list entry and no
// future review path can hand one an approval that the unappealable tier would
// then have to honour.
type Block struct {
	Tier Tier
	Rule string
	// Detail is what the person who typed the URL is told. It never echoes the
	// destination back: the reason code is the machine-readable half, and every
	// place a hostile URL gets rendered is a place it has to be defanged first.
	Detail string
}

// Error renders a block as the field error an API or a form receives.
func (b Block) Error(field string) domain.FieldError {
	return domain.FieldError{Field: field, Code: b.Tier.Code(b.Rule), Message: b.Detail}
}

// --- the high-confidence tier ------------------------------------------------

//go:embed blocked_hosts.txt
var blockedHostsRaw string

// embeddedHosts is the high-confidence tier, exact matches only.
//
// Written once, in init, and never again. TestEmbeddedTierIsWrittenOnlyAtInit
// parses this package and fails if any assignment to it appears outside init —
// which is what makes "heuristics never write into the embedded tier" a property
// of the code rather than a convention somebody has to remember.
//
// It is the only list this package compiles in, and D39 is why: overruling it is
// meant to cost a rebuild. Every other curated list — the shorteners, and
// whatever M32's feeds bring — is rows in blocked_destinations, because
// overruling those is meant to cost a click.
var embeddedHosts map[string]struct{}

func init() {
	embeddedHosts = parseHostList(blockedHostsRaw, "blocked_hosts.txt")
}

// parseHostList reads the embedded list.
//
// It panics on a malformed entry rather than skipping it. A blocklist that
// silently drops the line somebody added is worse than one that refuses to
// start: the operator believes a host is refused when it is not, and nothing
// ever tells them otherwise.
func parseHostList(raw, name string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := checkListEntry(line); err != nil {
			panic(fmt.Sprintf("link: %s entry %q: %v", name, line, err))
		}
		out[line] = struct{}{}
	}
	return out
}

// checkListEntry refuses an entry these lists cannot honour.
//
// The address rule is the one worth stating: an IP literal is refused here
// because addresses are the unappealable tier's business and nothing else's.
// Allowing one would mean a reader of this file could reasonably believe that
// deleting the line makes the address acceptable, which it does not — and the
// day somebody acts on that belief is the day the tier boundary stops being
// legible.
func checkListEntry(entry string) error {
	switch {
	case strings.ContainsAny(entry, "/:@?# "):
		return errors.New("must be a bare host, with no scheme, port, path or credentials")
	case strings.Contains(entry, "*"):
		return errors.New("wildcards are not supported; this tier matches exact hosts only")
	case strings.HasPrefix(entry, "."), strings.HasSuffix(entry, "."):
		return errors.New("must not start or end with a dot; suffix matching is the low-confidence tier's")
	case looksNumeric(entry):
		return errors.New("addresses belong to the unappealable tier and cannot be listed here")
	}
	return nil
}

// highConfidence reports the embedded tier's verdict on a host.
//
// Exact equality, deliberately: no suffix walk, no wildcard, no normalization
// beyond the case folding ValidateDestination already did. Confining the
// expensive tier to exact matches is what keeps a false positive there from
// costing a rebuild often enough that operators route around the feature.
func highConfidence(host string) *Block {
	if _, ok := embeddedHosts[host]; !ok {
		return nil
	}
	return &Block{
		Tier: TierHighConfidence, Rule: RuleEmbeddedHost,
		Detail: "that destination is on this instance's blocked list and cannot be used",
	}
}

// --- the low-confidence heuristics -------------------------------------------

// heuristic is one low-confidence rule.
//
// Note what this type cannot carry: a tier. The evaluator stamps every match
// TierLowConfidence and there is no field here through which a heuristic could
// say otherwise, so a heuristic cannot promote itself into the tier that costs a
// rebuild to overrule. That is the structural half of the promise; the other
// half is that nothing outside init writes to embeddedHosts.
type heuristic struct {
	rule   string
	detail string
	// match sees the parsed URL and the already-folded host, and answers only
	// yes or no.
	match func(u *url.URL, host string) bool
}

// heuristics is the registry. Order decides which rule a refusal reports when
// more than one matches; the more specific claim leads.
//
// Everything here is computed from the URL in front of it. The shortener-chain
// rule used to be here too, backed by a compiled host list, and is now a source
// in blocked_destinations instead (D39): a rule whose whole content is a list of
// names is data, and keeping it here meant a new shortener cost a release.
//
// Freshly-registered domains is absent, and that is decision D13 rather than an
// oversight: it needs a domain-age data source, which means egress, which
// collides with the promise that no destination leaves the box uninvited. M32's
// opt-in feed path is exactly what can supply it.
var heuristics = []heuristic{
	{
		rule: RuleURLCredentials,
		detail: "destination must not carry credentials before the host; " +
			"a URL written that way hides where it actually goes",
		match: func(u *url.URL, _ string) bool { return u.User != nil },
	},
	{
		rule: RulePunycodeHomograph,
		detail: "destination host is spelled with characters that imitate a " +
			"different name and cannot be used",
		match: func(_ *url.URL, host string) bool { return isHomograph(host) },
	},
}

// lowConfidenceHeuristics runs the registry.
func lowConfidenceHeuristics(u *url.URL, host string) *Block {
	for _, h := range heuristics {
		if h.match(u, host) {
			// The tier is stamped here and nowhere else.
			return &Block{Tier: TierLowConfidence, Rule: h.rule, Detail: h.detail}
		}
	}
	return nil
}

// --- the low-confidence Postgres list ----------------------------------------

// HostCandidates is a host and every parent of it, longest first.
//
// The label-boundary rule the environment blocklist has always had, expressed as
// the set of things to ask the database for: blocking "evil.example" refuses
// "login.evil.example" and does not refuse "notevil.example", because the
// candidates for the second are {notevil.example, example} and neither is the
// listed entry. Asking for all of them at once makes the match an index probe
// rather than a scan with a LIKE.
//
// Exported for M31's review queue. A decision to allow a destination has to
// remove the row that actually refused it, which may be a parent of the host
// that was typed — so the queue asks the same question this package does, with
// the same rule, rather than inventing a second matching rule that could drift.
func HostCandidates(host string) []string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return nil
	}
	out := []string{host}
	for i := strings.IndexByte(host, '.'); i >= 0; i = strings.IndexByte(host, '.') {
		host = host[i+1:]
		if host == "" {
			break
		}
		out = append(out, host)
	}
	return out
}

// listedInDatabase asks the runtime blocklist.
//
// An error is returned rather than swallowed. The alternative — treating a
// database failure as "not blocked" — would mean the one tier an owner can
// change stops working exactly when the instance is unhealthy, and a link
// created in that window is a link nobody reviewed.
func (s *Service) listedInDatabase(ctx context.Context, host string) (*Block, error) {
	candidates := HostCandidates(host)
	if len(candidates) == 0 {
		return nil, nil
	}
	row, err := s.q.MatchBlockedDestination(ctx, candidates)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("match blocked destination: %w", err)
	}
	// The row's source decides the rule. Its reason is deliberately not returned
	// to the caller: that is the operator's note to the owner reading the list,
	// not an explanation owed to whoever typed the URL, and echoing it back would
	// turn the refusal into a way to read the blocklist one probe at a time.
	return blockForSource(row.Source), nil
}

// blockForSource is the refusal a matched row reports.
//
// The tier is TierLowConfidence whichever branch is taken, and there is no
// source that could say otherwise — the same confinement the heuristic type has,
// for the same reason. What the source picks is only the rule, so that somebody
// reading a 422 or an audit record learns what the row actually claims about
// their destination: that a person listed it, or that it is a shortener.
//
// Anything unrecognized reports the operator-list rule rather than a code named
// after the source. The column has no CHECK constraint and later milestones add
// to it, so an unknown source is an expected future state and not a bug; what it
// still is, reliably, is a host somebody put on the list. Minting a reason code
// from the column's contents would put strings in a 422 that no documentation
// explains and that a client cannot have been written against.
func blockForSource(source string) *Block {
	if source == SourceShortener {
		return &Block{
			Tier: TierLowConfidence, Rule: RuleShortenerChain,
			Detail: "destination is itself a short link, which hides where visitors " +
				"actually end up",
		}
	}
	return &Block{
		Tier: TierLowConfidence, Rule: RuleOperatorBlocklist,
		Detail: "that destination is on this instance's blocked list; " +
			"the instance owner can review it",
	}
}

// --- the opt-in third-party feed ---------------------------------------------

// FeedChecker is internal/feed's client, as this package needs it.
//
// An interface declared by the consumer, so the dependency is two methods wide
// and a test answers with a table instead of an HTTP server. It is also what
// makes "with the feature off, zero destination URLs leave the instance" a
// structural claim: off is a nil interface, not a false flag, so there is no
// branch to get wrong and nothing to construct.
type FeedChecker interface {
	Check(ctx context.Context, destination string) (feed.Result, error)
	Name() string
	// Describe is what the instance tells its users it is doing. On this
	// interface rather than read from configuration so that the disclosure and
	// the sending come from one object: a page assembled from the environment
	// could say "on" about a client that was never built.
	Describe() feed.Disclosure
}

// FeedObserver counts checks. internal/observability implements it; nil counts
// nothing, which is what the CLI and most tests run with.
type FeedObserver interface {
	ObserveFeedCheck(result string)
}

// askFeed is the whole of the feed's authority over a destination.
//
// Three properties, and each is here rather than in a comment somewhere else
// because this is the only function that can break them.
//
// **It runs last, and only on a destination every built-in tier accepted.** So
// the built-in tiers behave identically with the feed on, off or erroring —
// they have already returned by the time this is reached, and nothing here can
// revisit their verdict. That is the bullet about feeds never being the
// mechanism the built-in tiers depend on, expressed as control flow.
//
// **It fails open, always.** Every error path returns nil, which means "no
// opinion" and lets the destination through. A third party's outage must not
// decide that this instance may not create links; the built-in protection an
// operator was promised is the protection they still have.
//
// **The owner's decision is read before anything leaves the box.** A host with
// an allowed dispute is not sent, so overruling a feed verdict stops both the
// refusal and the egress. It is scoped to this function — the only place in the
// program that reads that state — so it cannot reach the unappealable tier, the
// embedded list or the runtime blocklist, all three of which have already had
// their say above.
func (s *Service) askFeed(ctx context.Context, normalized, host string) *Block {
	if s.feed == nil {
		return nil
	}

	// Exact host, not the label-boundary walk the blocklist uses. An owner
	// allowing evil.example said that host was fine; reading it as permission
	// for login.evil.example would be widening a decision nobody made.
	allowed, err := s.q.HostHasAllowedDispute(ctx, host)
	switch {
	case err != nil:
		// Unable to tell whether the owner already allowed this. Counted as an
		// error and the destination is not sent: the failure direction that
		// keeps a promise is the one where nothing leaves, and refusing to ask
		// fails open on blocking exactly as an unanswered feed does.
		s.observeFeed(string(feed.ResultError))
		s.log.Warn("reputation feed skipped: could not read the owner's decisions",
			slog.Any("error", err))
		return nil
	case allowed:
		s.observeFeed("skipped")
		return nil
	}

	result, err := s.feed.Check(ctx, normalized)
	s.observeFeed(string(result))
	if err != nil {
		// Warn rather than error: nothing is broken here, a dependency the
		// operator opted into did not answer, and the request succeeds. The
		// counter above is what an alert reads; this is what explains it.
		s.log.Warn("reputation feed did not answer; failing open to the built-in tiers",
			slog.String("feed", s.feed.Name()),
			slog.Any("error", err))
		return nil
	}
	if result != feed.ResultMalicious {
		return nil
	}
	return feedBlock(s.feed.Name())
}

// feedBlock is the refusal a malicious verdict produces.
//
// Separate from askFeed so the tier can be asserted without a database, and
// because it is the one place a third party's answer becomes this program's. The
// tier is stamped here and there is no path by which a feed could supply one:
// FeedChecker returns a Result, which is three words, none of them a tier.
func feedBlock(name string) *Block {
	return &Block{
		Tier: TierLowConfidence, Rule: RuleFeedReputation,
		// Names the feed. The person being refused is entitled to know whose
		// claim it is, and the instance already discloses that the feed exists —
		// hiding it here would only make the refusal unappealable in practice.
		Detail: "that destination is reported as malicious by " + name +
			", this instance's configured reputation feed; the instance owner can review it",
	}
}

func (s *Service) observeFeed(result string) {
	if s.feedMetrics == nil {
		return
	}
	s.feedMetrics.ObserveFeedCheck(result)
}

// --- the surfaces ------------------------------------------------------------

// destinationSurface names one place a destination can be written.
//
// A constant per surface rather than a free string, because the audit record and
// the bypass test both key on it: TestEveryDestinationSurfaceGoesThroughTheCheck
// fails when a call site appears that is not one of these, which is how a later
// milestone adding a destination-writing surface finds out it has to declare M30
// as a dependency instead of discovering the gap in production.
type destinationSurface string

const (
	surfaceLinkCreate   destinationSurface = "link.create"
	surfaceLinkUpdate   destinationSurface = "link.update"
	surfaceRootRedirect destinationSurface = "domain.root_redirect"
)

// field is the form input a refusal on this surface belongs against, so the
// error highlights the box the person actually typed in.
func (s destinationSurface) field() string {
	if s == surfaceRootRedirect {
		return "root_redirect_url"
	}
	return "url"
}

// Verdict is what the tiers make of a destination. Nothing is recorded, nothing
// is stored, and no field says what to do about it.
//
// It exists because two callers need the same judgement for different reasons. A
// destination-writing surface needs it in order to refuse and to record the
// refusal. M31's dispute path needs it to answer a question no surface asks —
// *may this refusal be appealed at all* — and must not write a second
// `destination.blocked` record for a refusal that already happened, because
// double-counting is exactly what would ruin the numbers the log exists to let
// an operator tune.
type Verdict struct {
	// Normalized is the destination as it would be stored. Non-empty only when
	// nothing refused it.
	Normalized string
	// Host is the destination's folded host, or "" when the URL never got far
	// enough to have one.
	Host string
	// Block is the tiered refusal, or nil. Present for every refusal that names
	// a tier, the unappealable ones included — the validator raises those as
	// reason codes rather than as a Block, and Judge recovers the tier so that
	// "which tier refused this" is one question with one answer.
	Block *Block
	// Errs is the refusal as whoever typed the URL receives it. Its Field is
	// unset; the surface that reports it decides which input to highlight.
	// Empty exactly when Normalized is set.
	Errs domain.ValidationErrors
}

// Judge runs every tier against a destination and reports what they make of it.
//
// It is the single call site of ValidateDestination in the whole program, and
// that is enforced by test rather than by discipline. The plan review found this
// bypass in two of three candidate orderings: a later milestone adds a surface
// that writes a destination, calls the validator directly because that is what
// the existing code appears to do, and inherits the SSRF refusals while silently
// skipping every tier above them. Having one door removes the choice.
//
// Its own callers are policed too, by the same test, because a caller reaching
// past checkDestination to here would get the verdict without the audit record.
// That is legitimate for a dispute, which is arguing about a refusal already on
// record, and is a silent gap for anything that writes a destination.
func (s *Service) Judge(ctx context.Context, raw string) (Verdict, error) {
	normalized, err := ValidateDestination(raw, s.policy)
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			return Verdict{}, err
		}
		v := Verdict{Errs: ve}
		for _, fe := range ve {
			if tier, rule, ok := tierOf(fe.Code); ok {
				v.Block = &Block{Tier: tier, Rule: rule, Detail: fe.Message}
				break
			}
		}
		return v, nil
	}

	// Parsing again rather than threading the parsed URL out of the validator:
	// what the tiers judge must be the normalized string that would actually be
	// stored, not an intermediate the validator happened to hold. A tier reading
	// a different value from the one persisted is how a check passes for the
	// wrong reason.
	u, perr := url.Parse(normalized)
	if perr != nil {
		return Verdict{}, fmt.Errorf("reparse normalized destination: %w", perr)
	}
	host := strings.ToLower(u.Hostname())

	block := highConfidence(host)
	if block == nil {
		block, err = s.listedInDatabase(ctx, host)
		if err != nil {
			return Verdict{}, err
		}
	}
	if block == nil {
		block = lowConfidenceHeuristics(u, host)
	}
	// Last, and only on a destination everything above accepted. See askFeed:
	// the ordering is what makes the built-in tiers independent of the feed
	// rather than a claim somebody has to keep true by hand.
	if block == nil {
		block = s.askFeed(ctx, normalized, host)
	}
	if block == nil {
		return Verdict{Normalized: normalized, Host: host}, nil
	}
	return Verdict{
		Host: host, Block: block, Errs: domain.ValidationErrors{block.Error("")},
	}, nil
}

// checkDestination is the only way a destination becomes acceptable.
//
// Judge decides; this reports the decision against the surface's own form field
// and writes the audit record. Every surface that writes a destination goes
// through here, enforced by TestEveryDestinationSurfaceGoesThroughTheCheck.
//
// Returns the normalized destination, or a domain.ValidationErrors carrying a
// reason code that names the tier and the rule.
func (s *Service) checkDestination(
	ctx context.Context, actor *auth.Identity, raw string, surface destinationSurface,
) (string, error) {
	v, err := s.Judge(ctx, raw)
	if err != nil {
		return "", err
	}
	if len(v.Errs) == 0 {
		return v.Normalized, nil
	}

	// Copied rather than stamped in place. The Verdict is the caller's value and
	// a surface must not reach into it to relabel a field, or the next surface to
	// read the same Verdict would find one belonging to a form it never rendered.
	ve := make(domain.ValidationErrors, len(v.Errs))
	copy(ve, v.Errs)
	field := surface.field()
	for i := range ve {
		ve[i].Field = field
	}
	if v.Block != nil {
		s.recordBlocked(ctx, actor, raw, surface, v.Block.Tier, v.Block.Rule)
	}
	return "", ve
}

// recordBlocked writes the audit event for a refusal.
//
// After the refusal is decided and outside any transaction: the caller is about
// to be told no either way, and failing the refusal because its record could not
// be written would be a strictly worse outcome than a missing line. Logged at
// warn rather than swallowed, so the gap is visible to whoever goes looking.
//
// The attempted URL is evidence and is stored defanged. Not defanged at display
// time — defanged here, once, on the way in, because the audit read API returns
// metadata verbatim to whatever is asking and a URL that is inert in the column
// is inert in every consumer that has not been written yet.
func (s *Service) recordBlocked(
	ctx context.Context, actor *auth.Identity, raw string,
	surface destinationSurface, tier Tier, rule string,
) {
	if s.audit == nil {
		return
	}
	err := s.audit.Record(ctx, actor, audit.Event{
		Action:     audit.ActionDestinationBlocked,
		TargetType: "destination",
		Metadata: map[string]any{
			"tier":    string(tier),
			"rule":    rule,
			"code":    tier.Code(rule),
			"surface": string(surface),
			// The key says what the value is. A reader who sees "url" expects
			// something they can click; nobody clicks a "url_defanged".
			"url_defanged": Defang(raw),
		},
	})
	if err != nil {
		s.log.Warn("destination blocked but the audit record was not written",
			slog.String("code", tier.Code(rule)),
			slog.String("surface", string(surface)),
			slog.Any("error", err))
	}
}

// --- the environment list ----------------------------------------------------

// SeedBlocklist reconciles LINKCTRL_DESTINATION_BLOCKLIST into the runtime list.
//
// Run at boot. The environment variable seeds the Postgres list and keeps
// feeding it, so an operator who has been using it keeps the behaviour they
// had — the entries simply arrive as rows, gaining a reason code, an audit trail
// and a way for the owner to see them.
//
// Environment entries are reconciled, not merely inserted: a host the operator
// has since removed from the variable is deleted on the next boot, or the
// variable would be a one-way ratchet whose entries could only ever be undone
// with SQL. The delete is scoped to SourceEnv and reaches no other source — not
// what the review queue added, and not the seeded shorteners — because a restart
// quietly reversing a decision somebody made is the one failure this
// reconciliation must not have. That scoping is the whole job of the source
// column, and it is why the seed has one of its own rather than borrowing 'env'.
func (s *Service) SeedBlocklist(ctx context.Context, hosts []string) error {
	keep := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(h, ".")
		if h == "" {
			continue
		}
		if err := s.q.UpsertEnvBlockedDestination(ctx, dbgen.UpsertEnvBlockedDestinationParams{
			Host:   h,
			Reason: "LINKCTRL_DESTINATION_BLOCKLIST",
		}); err != nil {
			return fmt.Errorf("seed blocklist entry %q: %w", h, err)
		}
		keep = append(keep, h)
	}
	if _, err := s.q.DeleteStaleEnvBlockedDestinations(ctx, keep); err != nil {
		return fmt.Errorf("retire stale blocklist entries: %w", err)
	}
	return nil
}

// --- defanging ---------------------------------------------------------------

// defangSafe are the bytes Defang leaves alone.
//
// Deliberately narrow. Everything HTML-active is absent — <, >, ", ', & and the
// backtick — as are the square brackets, so the only brackets in the output are
// the defang markers themselves and the result is never ambiguous about which
// were there to begin with. % is absent too, so every %XX in the output is one
// this function wrote.
const defangSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
	"-_.~:/?#@!$()*+,;="

// defangMaxBytes bounds what is stored. A refusal happens before any length
// check on some paths, so this is the bound, not DestinationPolicy.MaxLength.
const defangMaxBytes = 2048

// Defang renders a hostile URL inert for storage and for display.
//
// Two transformations, and both are needed. Percent-escaping everything outside
// defangSafe makes the string inert as markup, so a destination carrying
// "<script>" cannot become one wherever it is rendered — including in a consumer
// written after this, which is the one that will forget. Bracketing the scheme
// delimiter and the dots makes it inert as a link, so nothing auto-links it, no
// mail client makes it clickable, and nobody follows it by reflex while reading
// the record of somebody else being refused.
//
// Reversible by hand and lossless in the sense that matters: an operator reading
// the audit log can still see exactly which host was attempted. Non-ASCII is
// escaped rather than shown, which costs readability on an internationalized
// host and buys certainty — a right-to-left override or a homograph rendered
// faithfully into a console is the display attack this function exists to stop.
func Defang(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) > defangMaxBytes {
		s = s[:defangMaxBytes]
	}

	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		if c := s[i]; strings.IndexByte(defangSafe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	// Every colon and every dot, not only the first of each.
	//
	// The first-occurrence version of this was wrong in a way worth recording: a
	// destination whose path contains another URL — which is most open-redirect
	// payloads — kept a live "https://" inside it, so the record of a refusal
	// contained a followable link to the thing that was refused. Neutralizing
	// every occurrence means the output can be checked by a property rather than
	// by reading it: no "://" survives anywhere, so nothing in it auto-links.
	out := strings.ReplaceAll(b.String(), ":", "[:]")
	return strings.ReplaceAll(out, ".", "[.]")
}
