// Package dispute is the appeal path for a blocked destination, and the queue an
// instance owner works through.
//
// It is designed as an attack surface rather than decorated as one afterwards,
// because that is literally what it is: a stranger picks a URL, and this feature
// puts it in front of the person who administers the box. Four properties carry
// that, and each is enforced by something other than good intentions.
//
// **Only a low-confidence refusal can be disputed.** The tier is re-derived
// server-side from the URL, by the same evaluator the link surfaces use, so it is
// never a field a caller supplies. An unappealable refusal — a private address, a
// forbidden scheme — has no dispute path at all, and neither does the embedded
// tier: the party those refusals protect is not the party appealing, and an owner
// who could approve 169.254.169.254 on request would have turned this queue into
// the SSRF the validator exists to refuse.
//
// **Nothing here fetches anything.** No preview, no screenshot, no favicon, no
// "does it still resolve" check. TestTheQueueFetchesNothing parses this package
// and the handlers that serve it and fails on any outbound-HTTP symbol, because a
// preview fetch is exactly that same SSRF arriving as a convenience feature.
// Since M32 there is one thing that leaves the box on this path and it is not a
// fetch of the destination: filing re-judges the URL, and on an instance that
// has named a reputation feed, judging sends the destination to that feed. The
// difference is the one the SSRF argument turns on — the address contacted is
// the operator's configured endpoint, never the attacker-chosen destination —
// and it is disclosed at /feeds and in the docs rather than left implicit.
//
// **The destination is stored inert.** Defanged once, on the way in, the rule
// audit_logs.metadata has followed since M30 — a value that cannot be rendered
// live is one no consumer written later can render live by forgetting.
//
// **A decision writes no permission anywhere.** Allowing removes one row from
// blocked_destinations; there is no row anybody can write that makes a
// destination acceptable, and 01500 has no allow column on purpose. M32 added
// the one decision that deletes nothing: an allowed dispute about a feed verdict
// is itself the override, read only by internal/link's feed step, which is the
// last check and the one every built-in tier has already returned before. So an
// allow still cannot reach the unappealable tier, the embedded list or the
// runtime blocklist — see liftableRules.
package dispute

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// PermReview guards reading the queue: listing it, counting it, inspecting what
// is in it.
//
// **Delegable to an API key**, and it was not until M45 (D98). Reading matches
// neither limb of D18 — the queue discloses who filed a dispute and a defanged
// host, never an address or a network prefix, and holding it widens nobody's
// reach. What used to make it non-delegable was that one permission guarded both
// halves, and the deciding half does widen reach; splitting them is what lets the
// reading half be what it always was.
//
// Held instance-wide, by a person the principal appointed, or by the principal
// itself. It is no longer granted to the owner *role*, and that is F15: on an
// instance with more than one organization — one registration away under
// LINKCTRL_SIGNUP_MODE=open — every owner read every dispute on the box.
const PermReview = "destinations.review"

// PermDecide guards acting on a dispute: allowing it or upholding it.
//
// Non-delegable to an API key, on D18's second limb: holding it lets a key widen
// its own reach. An allow removes a host from the instance-wide low-confidence
// list, after which every destination under that host becomes creatable —
// including by the key that removed it. That is a key turning "may not link
// there" into "may link there" by an action it takes itself, which is the shape
// D18 names.
//
// This is also how D98's second constraint is built. *"API access is read-only
// for disputes; a change requires a person"* is implemented as this scope sitting
// in auth.NonDelegableScopes, **not** as a check on what kind of credential is
// calling: the inherited Permissions rule says anything branching on credential
// type outside that map and D43 is a defect. So the endpoints below authorize on
// a permission like every other endpoint, and nothing in this package asks
// whether the caller holds a session. It keeps *"every UI feature has API
// support"* true as well — the decide endpoints exist, are documented and are
// replayed by the contract test, and they refuse a key, exactly as apikeys.*
// already does. A surface that exists and refuses is a different thing from a
// surface that does not exist.
//
// auth.NonDelegableScopes is the only thing that enforces it, so reversing this
// is deleting one map entry. See decisions.md.
const PermDecide = "destinations.decide"

// PermFile guards filing one.
//
// Deliberately the permission that would have let the person create the link in
// the first place, rather than a new one. A dispute is the second half of an
// attempt somebody was already allowed to make; requiring a separate grant would
// mean the refusal message ("the instance owner can review it") is a lie for
// everyone who can create links, which is the population it is shown to.
const PermFile = link.PermCreate

// Statuses. A dispute is filed open and leaves in exactly one of two directions.
const (
	// StatusOpen is waiting for a decision.
	StatusOpen = "open"
	// StatusAllowed means the owner removed the entry that refused it.
	StatusAllowed = "allowed"
	// StatusUpheld means the owner looked and left the refusal standing.
	StatusUpheld = "upheld"
)

// Notification kinds. Both are about the dispute rather than about the refusal:
// whoever typed the URL already learned of the block synchronously, in the 422.
// What needs delivering later is that something is waiting, and what came of it.
const (
	// KindFiled tells the people who can review that a dispute has arrived.
	KindFiled = "dispute.filed"
	// KindDecided tells the person who filed one what was decided.
	KindDecided = "dispute.decided"
)

// Audit actions. The decisions are recorded and the filing is not, and that
// asymmetry is deliberate: a filing's whole record is the dispute row, which
// carries who and when and outlives the account. A decision has an effect
// *outside* that row — an entry gone from the instance-wide blocklist — and the
// audit log is the only place that effect is otherwise visible.
const (
	ActionDisputeAllowed = "dispute.allowed"
	ActionDisputeUpheld  = "dispute.upheld"
)

// Dispute is one appeal, as every reader sees it.
//
// Every string here that came from whoever filed it is inert. Destination is
// defanged in the column; Host is defanged on the way out, because the plain
// value is the key a decision acts on and is worth exactly one representation in
// the database. There is no free-text field at all — see the migration.
type Dispute struct {
	ID uuid.UUID `json:"id"`
	// Host is the destination's host as it was typed, defanged. Never rendered as
	// a link, never handed to anything that fetches.
	Host string `json:"host_defanged"`
	// BlockedHost is the blocklist entry an allow would delete, defanged. Empty
	// when no entry produced the refusal.
	//
	// It is a separate field from Host because it is routinely a different value:
	// the list matches on label boundaries, so a dispute about
	// login.evil.example is a dispute about the row that says evil.example. Every
	// surface that offers Allow renders this rather than Host, which is the whole
	// of F33 — a queue that shows one host while the button acts on another is
	// asking somebody to approve a decision they have not been told.
	BlockedHost string `json:"blocked_host_defanged,omitempty"`
	// Destination is the attempted URL, defanged.
	Destination string `json:"destination_defanged"`
	// ReasonCode is M30's "<tier>.<rule>". Always a low_confidence code.
	ReasonCode string `json:"reason_code"`
	Status     string `json:"status"`
	// FiledBy is the address of whoever filed it, snapshotted at write time so it
	// survives the account.
	FiledBy   string    `json:"filed_by"`
	CreatedAt time.Time `json:"created_at"`
	// DecidedBy and DecidedAt are set once a decision is recorded.
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// Liftable says whether an allow could do anything. False for a refusal that
	// no list row produced — a homograph, credentials in the URL — where there is
	// nothing to delete and the only honest decision is to uphold or to change
	// the code. The page draws its buttons from this rather than making the owner
	// discover it by clicking.
	Liftable bool `json:"liftable"`
}

// Filter is a page request.
type Filter struct {
	Cursor   string
	Limit    int32
	OpenOnly bool
}

// Judge is internal/link's tier evaluation, as this package needs it.
//
// An interface declared by the consumer, so a test can answer with a table
// instead of standing up the link service and a blocklist behind it — and so
// this package's dependency on link is one method wide.
type Judge interface {
	Judge(ctx context.Context, raw string) (link.Verdict, error)
}

// Notifier is internal/notify's writing half, as this package needs it.
//
// Reviewers, not "owners of an organization": the queue is instance-wide, so the
// people to tell about a new dispute are everybody who could act on it. Until
// M45 that was the same method spelled `EveryOwner`, which meant every owner of
// every organization because no other set existed — see EveryReviewer, and F137.
type Notifier interface {
	Notify(ctx context.Context, userID uuid.UUID, e notify.Event) error
	EveryReviewer(ctx context.Context) ([]notify.Recipient, error)
	// RecipientByID and Mail are the outcome email (D1's addendum). Mail is a
	// no-op on an instance with no mailer, so this package never asks whether
	// one is configured — in-app delivery is the baseline and the mail is the
	// addition, in that order, at every call site.
	RecipientByID(ctx context.Context, userID uuid.UUID) (notify.Recipient, error)
	Mail(ctx context.Context, to notify.Recipient, template string, data map[string]string) error
}

// Service files, lists and decides disputes.
type Service struct {
	q      *dbgen.Queries
	judge  Judge
	audit  audit.Recorder
	notify Notifier
	log    *slog.Logger
}

// Config is what a Service needs.
type Config struct {
	// Judge decides which tier refused a destination. Required: without it there
	// is no way to tell an appealable refusal from an unappealable one, and
	// guessing in either direction is worse than refusing to start.
	Judge Judge
	// Audit records the two decisions. Nil records nothing.
	Audit audit.Recorder
	// Notify carries both messages. Nil tells nobody, which is what a test that
	// does not care about delivery gets.
	Notify Notifier
	Log    *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Judge == nil {
		return nil, errors.New("dispute: no destination judge")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		q: dbgen.New(pool), judge: cfg.Judge,
		audit: cfg.Audit, notify: cfg.Notify, log: log,
	}, nil
}

const (
	defaultPageLimit = 25
	maxPageLimit     = 100
)

// liftableRules are the low-confidence rules a decision can actually lift.
//
// Two of them are rows in blocked_destinations, and deleting the matched row is
// the entire reach an allow had until M32. The punycode homograph and the
// credentials rule are computed from the URL every time it is judged, so there
// is no row to remove and no row anyone may add: 01500 has no allow column,
// deliberately, because a list that can permit a destination is a list that can
// overrule the unappealable tier one entry at a time.
//
// A dispute about one of those is still worth filing and worth reading — it is
// how an operator learns their heuristics are producing false positives, which
// is the number M30 built the audit record to expose. It just cannot be closed
// by allowing it, and saying so up front beats an owner clicking a button that
// reports a conflict.
var liftableRules = map[string]bool{
	link.TierLowConfidence.Code(link.RuleOperatorBlocklist): true,
	link.TierLowConfidence.Code(link.RuleShortenerChain):    true,
	link.TierLowConfidence.Code(link.RuleFeedReputation):    true,
}

// liftedByDecision are the liftable rules with no row behind them.
//
// One entry, and it is the whole of how a third party's verdict is made
// owner-overridable without giving anybody a way to write permission into the
// blocklist. A feed verdict is not stored — it is asked for on every destination
// write — so there is nothing for an allow to delete. The `allowed` status on
// this dispute *is* the override: internal/link reads it before it sends
// anything, so overruling the verdict stops both the refusal and the egress.
//
// It is confined to the feed step and cannot widen anything else, for a reason
// that is structural rather than careful: the feed is consulted last, only on a
// destination the unappealable tier, the embedded list, the runtime blocklist
// and the heuristics have all already accepted. There is no verdict left above
// it for a suppression to reach.
var liftedByDecision = map[string]bool{
	link.TierLowConfidence.Code(link.RuleFeedReputation): true,
}

// Codes a refusal to file carries, on the `url` field.
//
// Field errors rather than a sentinel, because every other refusal about a
// destination in this program is one and a client already branches on the shape.
// Nothing is hidden by naming the cause: whoever is filing received the tier and
// the rule in the 422 that refused their link a moment ago, so the vocabulary is
// one they have already been handed.
const (
	// CodeNotDisputable is the whole of "unappealable and embedded-tier refusals
	// have no dispute path at all".
	CodeNotDisputable = "not_disputable"
	// CodeNotBlocked means nothing refuses that destination — usually because
	// somebody already lifted the entry.
	CodeNotBlocked = "not_blocked"
)

// File records a dispute about a destination that was refused.
//
// The destination is re-judged here rather than trusted from the caller, and
// that is the mechanism behind "creatable only from a low-confidence refusal":
// no request field names a tier, so no request can claim one. It re-judges
// against the list *as it is now*, so a host removed since the refusal produces
// "that destination is not refused any more" instead of a dispute about nothing.
//
// No audit record. The refusal being argued about already wrote one, and a
// second `destination.blocked` per dispute would inflate exactly the count an
// operator reads to decide whether a heuristic is worth keeping.
func (s *Service) File(ctx context.Context, actor *auth.Identity, rawURL string) (*Dispute, error) {
	if !actor.Can(PermFile) {
		return nil, fmt.Errorf("%w: disputing a refusal requires %s", domain.ErrForbidden, PermFile)
	}

	verdict, err := s.judge.Judge(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if err := disputable(verdict); err != nil {
		return nil, err
	}

	params := dbgen.InsertDestinationDisputeParams{
		// v7, so the queue's keyset tiebreak on (created_at, id) is stable.
		ID:   uuid.Must(uuid.NewV7()),
		Host: verdict.Host,
		// The row that refused it, taken from the judgement that just ran rather
		// than re-derived when somebody decides. Empty for a rule computed from
		// the URL, which has no row at all.
		//
		// Recorded now because the answer can change later: the walk that finds a
		// matching entry answers with the longest match, so an entry added between
		// the filing and the decision would silently retarget a decision the owner
		// believes they are making about the host the queue showed them.
		BlockedHost: verdict.ListedHost,
		UrlDefanged: link.Defang(rawURL),
		ReasonCode:  verdict.Block.Tier.Code(verdict.Block.Rule),
		// Both forms of the filer, for the reason audit_logs keeps both: the id
		// joins while the account exists, and the label is what a reviewer reads
		// after it is gone.
		CreatedBy:      actorID(actor),
		CreatedByLabel: actorLabel(actor),
	}
	if actor != nil {
		if actor.OrgID != uuid.Nil {
			org := actor.OrgID
			params.OrganizationID = &org
		}
		if actor.WorkspaceID != uuid.Nil {
			ws := actor.WorkspaceID
			params.WorkspaceID = &ws
		}
	}

	row, err := s.q.InsertDestinationDispute(ctx, params)
	if err != nil {
		if isUniqueViolation(err) {
			// Either index can raise this, and the message covers both without
			// saying which — the honest reading of the second one is that the
			// decision is queued rather than the destination, because an open
			// dispute about the entry that refuses evil.example is the same
			// decision as one about login.evil.example. Naming the queued dispute
			// would tell a filer about a row they cannot read.
			return nil, fmt.Errorf(
				"%w: that refusal is already waiting for review", domain.ErrConflict)
		}
		return nil, fmt.Errorf("file dispute: %w", err)
	}

	s.tellReviewers(ctx, row)
	// One lookup, on a path that has already written and notified. The filer's
	// own response carries Liftable too, and answering it from the rule alone
	// would put the same wrong claim in the API that F42 put on the page.
	return toDispute(row, s.entrySourceFor(ctx, row.BlockedHost)), nil
}

// disputable reports whether a verdict may be appealed, returning the refusal to
// hand back when it may not.
//
// The whole of "creatable only from a low-confidence refusal" is here, in one
// comparison against a tier this package never assigns, on a verdict it did not
// compute. There is no request field that reaches it, so no request can claim a
// tier — and it is a function rather than four branches inside File so that
// every tier, including the accepting one, is exercised by a test that needs no
// database.
func disputable(v link.Verdict) error {
	switch {
	case v.Block == nil && len(v.Errs) > 0:
		// Refused, but not by a tier — an empty, over-long or unparseable URL.
		// A typo is not an appeal. Reported as the validator wrote it, against
		// the field the caller supplied.
		errs := make(domain.ValidationErrors, len(v.Errs))
		copy(errs, v.Errs)
		for i := range errs {
			errs[i].Field = "url"
		}
		return errs
	case v.Block == nil:
		return domain.ValidationErrors{{
			Field: "url", Code: CodeNotBlocked,
			Message: "that destination is not refused, so there is nothing to dispute",
		}}
	case v.Block.Tier != link.TierLowConfidence:
		return domain.ValidationErrors{{
			Field: "url", Code: CodeNotDisputable,
			Message: "that refusal is not appealable: " + v.Block.Detail,
		}}
	}
	return nil
}

// List answers one page of the queue, newest first.
//
// Instance-wide, like the list it argues with. Scoping it to the reader's own
// organization would hide rows the same reader is nonetheless deciding for,
// because an allow removes an entry every organization on the instance is
// refused by. The permission is what bounds who sees it — and since D98 that
// permission is held instance-wide by named people rather than by every owner of
// every organization, which is the difference between the queue being
// instance-wide and its readership being accidental.
func (s *Service) List(ctx context.Context, actor *auth.Identity, f Filter) (*domain.Page[Dispute], error) {
	if !actor.Can(PermReview) {
		return nil, fmt.Errorf("%w: reviewing disputes requires %s", domain.ErrForbidden, PermReview)
	}

	limit := f.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	params := dbgen.ListDestinationDisputesParams{
		OpenOnly: f.OpenOnly,
		// One extra row answers "is there another page" without a second query.
		PageLimit: limit + 1,
	}
	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, domain.ValidationErrors{{
				Field: "cursor", Code: "invalid", Message: "pagination cursor is not valid",
			}}
		}
		params.CursorCreated = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := s.q.ListDestinationDisputes(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list disputes: %w", err)
	}

	page := &domain.Page[Dispute]{Items: make([]Dispute, 0, limit)}
	if len(rows) > int(limit) {
		page.HasMore = true
		rows = rows[:limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, *toDispute(listRowToDispute(r), deref(r.EntrySource)))
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// CountOpen is what the queue's heading says there is to do.
func (s *Service) CountOpen(ctx context.Context, actor *auth.Identity) (int64, error) {
	if !actor.Can(PermReview) {
		return 0, fmt.Errorf("%w: reviewing disputes requires %s", domain.ErrForbidden, PermReview)
	}
	n, err := s.q.CountOpenDestinationDisputes(ctx)
	if err != nil {
		return 0, fmt.Errorf("count open disputes: %w", err)
	}
	return n, nil
}

// Allow removes the entry that refused the destination, and closes the dispute.
//
// The deletion is scoped to the row the low-confidence list matched when the
// dispute was filed, recorded on the dispute then and rendered on the control
// that triggers this. It can reach nothing else: the embedded tier is a compiled
// file and the unappealable tier has no row anywhere, so "acts only on the
// runtime low-confidence list" is a property of there being nothing else to act
// on.
//
// Two refusals it declines rather than pretending to lift:
//
//   - a rule no row produced. A homograph is computed from the URL every time,
//     so deleting nothing and reporting success would tell the owner the
//     destination is now usable when it is not.
//   - a row sourced from LINKCTRL_DESTINATION_BLOCKLIST. Boot reconciles that
//     variable back into the table, so allowing it would last until the next
//     restart and then silently revert — the one failure mode a moderation
//     decision must not have. The operator is told to edit the variable.
func (s *Service) Allow(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*Dispute, error) {
	return s.decide(ctx, actor, id, StatusAllowed)
}

// Uphold leaves the refusal standing and closes the dispute.
//
// It changes no list and is still an audit event. "The owner looked and said no"
// is a different fact from "nobody has looked yet", and a queue that only records
// the decisions which changed something cannot tell them apart.
func (s *Service) Uphold(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*Dispute, error) {
	return s.decide(ctx, actor, id, StatusUpheld)
}

func (s *Service) decide(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, status string,
) (*Dispute, error) {
	if !actor.Can(PermDecide) {
		return nil, fmt.Errorf("%w: deciding a dispute requires %s", domain.ErrForbidden, PermDecide)
	}

	existing, err := s.q.GetDestinationDispute(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, domain.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("load dispute: %w", err)
	}
	if existing.Status != StatusOpen {
		return nil, fmt.Errorf("%w: that dispute was already decided", domain.ErrConflict)
	}

	// Everything that can refuse the decision runs before anything is written, so
	// a refusal leaves the list and the dispute exactly as they were.
	var lifted string
	if status == StatusAllowed {
		if lifted, err = s.entryToLift(ctx, existing); err != nil {
			return nil, err
		}
	}

	if lifted != "" {
		if _, err := s.q.DeleteBlockedDestination(ctx, lifted); err != nil {
			return nil, fmt.Errorf("lift blocked destination %q: %w", lifted, err)
		}
	}

	row, err := s.q.DecideDestinationDispute(ctx, dbgen.DecideDestinationDisputeParams{
		ID: id, Status: status,
		DecidedBy: actorID(actor), DecidedByLabel: actorLabel(actor),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Somebody decided it between the read above and this write. Reported
			// as the conflict it is rather than as a 404, because the row exists.
			return nil, fmt.Errorf("%w: that dispute was already decided", domain.ErrConflict)
		}
		return nil, fmt.Errorf("decide dispute: %w", err)
	}

	s.record(ctx, actor, row, lifted)
	s.tellFiler(ctx, row)
	// The source as it was **before** the decision: an allow has just deleted
	// the entry, and reporting "not liftable" because the lift succeeded would
	// be the opposite of informative.
	return toDispute(row, deref(existing.EntrySource)), nil
}

// entryToLift finds the blocklist row an allow would remove, or refuses.
//
// Returns the host to delete. An empty string with a nil error means the rule is
// one of liftedByDecision — the allow changes something real, just not a row.
// For every other rule an empty string is a refusal, because a decision that
// changes nothing must not be recorded as one that did.
//
// It reads the entry off the dispute and does not look for one. Until M45 it
// re-ran link.HostCandidates against the list as it stood at decision time and
// deleted the longest match, which meant the row that disappeared was whatever
// the list happened to say when the button was clicked rather than the row the
// filer was refused by and the owner was shown. Naming it at filing time is F33's
// repair, and it is what makes DeleteBlockedDestination's scoping comment true as
// written.
func (s *Service) entryToLift(ctx context.Context, d dbgen.GetDestinationDisputeRow) (string, error) {
	if !liftableRules[d.ReasonCode] {
		return "", fmt.Errorf(
			"%w: %s is computed from the URL rather than held in the blocklist, so there "+
				"is no entry to remove; uphold it, or change the rule", domain.ErrConflict, d.ReasonCode)
	}
	if liftedByDecision[d.ReasonCode] {
		// Nothing to delete, and nothing to check for either: the verdict is a
		// third party's, re-asked on every write, so there is no row that could
		// have gone stale between filing and deciding. Recording the decision is
		// the whole effect.
		return "", nil
	}
	if d.BlockedHost == "" {
		// A list-backed rule with no entry recorded is a dispute filed before
		// 03300 existed. Refused rather than guessed at: re-deriving the entry is
		// the behaviour this column was added to retire, and doing it once "just
		// for old rows" would keep the defect alive on exactly the instances that
		// have one. Upholding closes it, and re-filing writes the entry.
		return "", fmt.Errorf(
			"%w: this dispute was filed before the blocklist entry was recorded on it, "+
				"so there is no entry it can be trusted to remove; uphold it and file it "+
				"again", domain.ErrConflict)
	}

	row, err := s.q.GetBlockedDestination(ctx, d.BlockedHost)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf(
			"%w: nothing on the blocklist refuses that host any more; uphold it to close "+
				"the dispute", domain.ErrConflict)
	case err != nil:
		return "", fmt.Errorf("read blocked destination: %w", err)
	}
	if row.Source == link.SourceEnv {
		return "", fmt.Errorf(
			"%w: %q comes from LINKCTRL_DESTINATION_BLOCKLIST, so removing it here would "+
				"last until the next restart; take it out of the environment instead",
			domain.ErrConflict, row.Host)
	}
	return row.Host, nil
}

// entrySourceFor reads the source of the blocklist entry behind a refusal.
//
// Empty when there is no entry — a refusal computed from the URL, or a
// list-backed one whose entry has since gone. Errors are treated as "no entry"
// rather than failing the caller: this decides whether a button is drawn, and
// refusing to file a dispute because the button's state could not be computed
// would trade a cosmetic defect for a functional one.
func (s *Service) entrySourceFor(ctx context.Context, host string) string {
	if host == "" {
		return ""
	}
	row, err := s.q.GetBlockedDestination(ctx, host)
	if err != nil {
		return ""
	}
	return row.Source
}

// listRowToDispute drops the joined column so the renderer keeps taking the
// table's own shape. The two differ by exactly one field, and threading a second
// row type through toDispute would buy nothing.
func listRowToDispute(r dbgen.ListDestinationDisputesRow) dbgen.DestinationDispute {
	return dbgen.DestinationDispute{
		ID: r.ID, Host: r.Host, UrlDefanged: r.UrlDefanged, ReasonCode: r.ReasonCode,
		Status: r.Status, OrganizationID: r.OrganizationID, WorkspaceID: r.WorkspaceID,
		CreatedBy: r.CreatedBy, CreatedByLabel: r.CreatedByLabel, CreatedAt: r.CreatedAt,
		DecidedBy: r.DecidedBy, DecidedByLabel: r.DecidedByLabel, DecidedAt: r.DecidedAt,
		BlockedHost: r.BlockedHost,
	}
}

// deref is the empty string for a LEFT JOIN that matched nothing.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// canLiftEntry reports whether the entry behind a refusal is one a decision may
// delete.
//
// Spelled once and consulted from both the page and the refusal, so the button
// and the answer cannot disagree — which is what F42 was: `entryToLift` refused
// an `env`-sourced entry and `Liftable` never asked, so an operator-configured
// instance's most likely dispute drew an Allow button that answered 409.
//
// A rule whose verdict is re-asked on every write deletes nothing and is
// liftable regardless of any entry, which is why it is checked first.
// An empty source means no row was joined: either the refusal is computed from
// the URL, or it is list-backed and was filed before the entry was recorded.
// Both are refusals a decision cannot lift.
func canLiftEntry(reasonCode, entrySource string) bool {
	if liftedByDecision[reasonCode] {
		return true
	}
	return entrySource != "" && entrySource != link.SourceEnv
}

// record writes the audit event for a decision.
//
// After the decision, outside any transaction, and logged rather than returned
// on failure: the list has already changed, and failing the request now would
// tell the caller nothing happened when something did. The metadata names the
// entry that was lifted, because that is the part of the decision the dispute row
// does not carry.
//
// **Instance-wide** (F36). Allowing a dispute deletes a row from a blocklist
// every organization on the instance is refused by, and upholding one decides
// for the same population; recording that under whichever organization the
// decider happened to be standing in named a tenant with no particular claim to
// it and hid it from every tenant it changed. Since D98 the decider is the
// instance principal or somebody it appointed, so the tenant it would have been
// filed under is now, routinely, a personal organization that has nothing to do
// with the decision.
func (s *Service) record(
	ctx context.Context, actor *auth.Identity, d dbgen.DestinationDispute, lifted string,
) {
	if s.audit == nil {
		return
	}
	action := ActionDisputeUpheld
	if d.Status == StatusAllowed {
		action = ActionDisputeAllowed
	}
	id := d.ID
	meta := map[string]any{
		"reason_code": d.ReasonCode,
		// Defanged in the audit metadata too. That table is returned verbatim by
		// the read API, so a live URL here would be a live URL in every consumer.
		"host_defanged": link.Defang(d.Host),
		"url_defanged":  d.UrlDefanged,
	}
	if lifted != "" {
		meta["blocklist_entry_removed"] = link.Defang(lifted)
	}
	// An allow that deleted nothing still did something, and the log has to say
	// which of the two it was. Without this an operator reading back a
	// `dispute.allowed` row with no `blocklist_entry_removed` would reasonably
	// conclude the decision failed silently.
	if d.Status == StatusAllowed && liftedByDecision[d.ReasonCode] {
		meta["feed_verdict_overridden"] = true
	}
	err := s.audit.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "destination_dispute", TargetID: &id,
		Metadata: meta, InstanceWide: true,
	})
	if err != nil {
		s.log.Warn("dispute decided but the audit record was not written",
			slog.String("action", action), slog.String("dispute", id.String()),
			slog.Any("error", err))
	}
}

// tellReviewers puts a newly filed dispute in front of everybody who can act on
// it. Failure is logged, never returned: the dispute is filed either way, and
// refusing the filing because a notification could not be written would lose the
// thing that matters to keep the thing that reports it.
//
// **The recipient set is what bounds this, and nothing else does** (F137).
// Filing costs `link.PermCreate`, the browser route carries no limiter, and a
// refusal computed from the URL rather than matched against a blocklist row —
// `url_credentials` fires on any host with an `a@` prefix — has no entry for
// 01600's one-open-dispute index to key on, so distinct strings are distinct
// open disputes without bound. F33 closed the subdomain multiplier by keying the
// index on the matched entry; it could close nothing for a rule that matches no
// entry, and the remaining lever was always *how many people each filing
// reaches*. While that was every organization owner on the instance it was a
// number that grew with registrations, which is a stranger multiplying their own
// nuisance by the size of somebody else's user base. It is now the reviewers,
// which is a set the instance principal chose and can change — so an unbounded
// number of filings is an unbounded queue for the people whose job the queue is,
// and not an inbox flood for everyone who ever signed up.
func (s *Service) tellReviewers(ctx context.Context, d dbgen.DestinationDispute) {
	if s.notify == nil {
		return
	}
	reviewers, err := s.notify.EveryReviewer(ctx)
	if err != nil {
		s.log.Warn("dispute filed but reviewers could not be listed", slog.Any("error", err))
		return
	}
	for _, to := range reviewers {
		if err := s.notify.Notify(ctx, to.UserID, notify.Event{
			Kind:  KindFiled,
			Title: "A blocked destination is waiting for review",
			// The body carries the defanged host and no live URL. It is rendered
			// in the bell, on the notifications page and in the API, none of which
			// this package controls — which is exactly why it is inert here.
			Body: fmt.Sprintf(
				"%s was refused as %s and the person who tried it has asked you to look. "+
					"Review it at /disputes.", link.Defang(d.Host), d.ReasonCode),
			Data: map[string]any{
				"dispute_id":    d.ID.String(),
				"host_defanged": link.Defang(d.Host),
				"reason_code":   d.ReasonCode,
			},
		}); err != nil {
			s.log.Warn("dispute filed but a reviewer was not notified",
				slog.String("dispute", d.ID.String()), slog.Any("error", err))
		}
	}
}

// tellFiler reports the outcome to whoever filed the dispute.
//
// In the dashboard always, and by email as well when the instance has a mailer
// (D1). Which is the baseline and which is the addition is decided by the
// ordering here rather than by a flag: the inbox row is written first, and the
// mail is only attempted after it succeeded. A person who files a dispute may
// not open the dashboard again for a week — the outcome is the one thing in this
// feature they are actually waiting for, and it is the only notification in it
// addressed to somebody who did not choose to be an administrator.
func (s *Service) tellFiler(ctx context.Context, d dbgen.DestinationDispute) {
	if s.notify == nil || d.CreatedBy == nil {
		return
	}
	title := "Your disputed destination was allowed"
	body := fmt.Sprintf("%s is no longer refused; you can create that link now.", link.Defang(d.Host))
	outcome := "You can create that link now."
	if d.Status != StatusAllowed {
		title = "Your disputed destination stays blocked"
		body = fmt.Sprintf(
			"%s was reviewed and the refusal stands (%s).", link.Defang(d.Host), d.ReasonCode)
		outcome = "The refusal stands, so that destination still cannot be used here."
	}
	if err := s.notify.Notify(ctx, *d.CreatedBy, notify.Event{
		Kind: KindDecided, Title: title, Body: body,
		Data: map[string]any{
			"dispute_id":    d.ID.String(),
			"host_defanged": link.Defang(d.Host),
			"status":        d.Status,
		},
	}); err != nil {
		s.log.Warn("dispute decided but the filer was not notified",
			slog.String("dispute", d.ID.String()), slog.Any("error", err))
		// No mail either. The inbox row is the delivery; if it could not be
		// written the person has heard nothing, and emailing them about a
		// decision the dashboard will not show is worse than the silence.
		return
	}
	s.mailFiler(ctx, d, outcome)
}

// mailFiler queues the email form of the outcome, if there is a mailer.
//
// Every value it interpolates is inert before it goes in — the host defanged,
// the status and the reason code drawn from this program's own vocabulary — and
// internal/ui neutralizes them again on the way into the template. Twice, because
// this is the one message in the product whose subject matter is a string an
// attacker chose, sent to somebody who did not ask to receive it.
func (s *Service) mailFiler(ctx context.Context, d dbgen.DestinationDispute, outcome string) {
	to, err := s.notify.RecipientByID(ctx, *d.CreatedBy)
	if err != nil {
		s.log.Warn("dispute decided but the filer could not be resolved for mail",
			slog.String("dispute", d.ID.String()), slog.Any("error", err))
		return
	}
	if err := s.notify.Mail(ctx, to, notify.MailDisputeDecided, map[string]string{
		"Status":     d.Status,
		"Host":       link.Defang(d.Host),
		"ReasonCode": d.ReasonCode,
		"Outcome":    outcome,
	}); err != nil {
		s.log.Warn("dispute decided but the outcome mail was not queued",
			slog.String("dispute", d.ID.String()), slog.Any("error", err))
	}
}

// toDispute renders one row for a reader.
//
// entrySource is the blocklist entry behind this refusal, or empty when there is
// none — a refusal computed from the URL, or a list-backed one filed before the
// entry was recorded. It decides Liftable together with the rule (F42).
func toDispute(r dbgen.DestinationDispute, entrySource string) *Dispute {
	d := &Dispute{
		ID: r.ID,
		// Defanged on the way out. The columns hold the plain hosts because those
		// are the keys a decision acts on, and every reader gets the inert form —
		// including the JSON API, whose consumers are the ones nobody reviews.
		Host:        link.Defang(r.Host),
		Destination: r.UrlDefanged,
		ReasonCode:  r.ReasonCode,
		Status:      r.Status,
		FiledBy:     r.CreatedByLabel,
		CreatedAt:   r.CreatedAt,
		DecidedAt:   r.DecidedAt,
		// Both halves. The rule says whether an allow *could* delete something;
		// the source says whether this refusal's entry is one a decision may
		// delete. An env-sourced entry is rewritten from
		// LINKCTRL_DESTINATION_BLOCKLIST at every boot, so `entryToLift` refuses
		// it — and the page drew its Allow button from the rule alone, which
		// meant the most likely dispute on an operator-configured instance got a
		// button that 409s and no explanation (F42).
		Liftable: liftableRules[r.ReasonCode] && canLiftEntry(r.ReasonCode, entrySource),
	}
	// Empty stays empty rather than becoming a defanged nothing, so "no entry
	// behind this refusal" reads the same on every surface.
	if r.BlockedHost != "" {
		d.BlockedHost = link.Defang(r.BlockedHost)
	}
	if r.DecidedByLabel != "" {
		d.DecidedBy = r.DecidedByLabel
	}
	return d
}

func actorLabel(actor *auth.Identity) string {
	switch {
	case actor == nil:
		return "system"
	case actor.Email != "":
		return actor.Email
	case actor.Name != "":
		return actor.Name
	}
	return "unknown"
}

func actorID(actor *auth.Identity) *uuid.UUID {
	if actor == nil || actor.UserID == uuid.Nil {
		return nil
	}
	id := actor.UserID
	return &id
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// cursor is a position in the (created_at DESC, id DESC) ordering.
// Version-prefixed and fixed-arity, like every other cursor in this tree: one
// minted by an older build decodes to the wrong number of fields and is refused
// rather than read as a position it does not describe.
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
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
	return cursor{CreatedAt: at, ID: id}, nil
}
