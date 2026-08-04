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

// DNS verification, and what happens when it stops passing (M40).
//
// **The gap between registered and verified is an alias-namespace hijack.** M39
// let a workspace claim a hostname and served nothing on it; this file is the
// proof that turns a claim into serving, and the bounded patience that takes
// serving away again when the proof stops holding.
//
// The whole gate is one column. `verified_at` is set here and nowhere else, it
// is cleared here and by a rename, and `ListVerifiedDomains` — the only query
// the host router's cache reads — filters on it. There is no second predicate to
// disagree with.
//
// **Decision D70** governs the failure path and its three constraints are load
// bearing rather than decoration: the window is short enough to state plainly in
// the runbook, it is operator configuration rather than a constant, and its
// expiry is a real stop rather than another warning.

// TXTLookup reads DNS TXT records.
//
// An interface with one method, and the implementation is deliberately in
// another package (internal/dnsx). This package's guard test —
// TestTheFeedIsTheOnlyWayADestinationLeaves — fails the build on any outbound
// symbol here at all, because a lookup added to "check the host resolves" would
// send a user's destination to a nameserver while /feeds went on saying nothing
// leaves. Custom-domain verification needs DNS and does not need it *here*: this
// file decides when to ask, and something else does the asking.
//
// The seam earns its place twice over. A test can say exactly what DNS answered,
// and the demo seeder supplies a lookup that satisfies the challenge for its own
// reserved `.example` hostnames — so the demo shows a verified domain by passing
// the check rather than by writing the column behind the checker's back.
type TXTLookup interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// HostInvalidator tells every replica that the verified set has changed.
//
// Nil is valid and means this process is the only one that matters — the CLI,
// most tests — and then a verification is visible here and nowhere else, which
// is the pre-pub/sub behaviour rather than a fault.
type HostInvalidator interface {
	InvalidateHosts(ctx context.Context)
}

// DomainNotifier warns a workspace that its hostname is in trouble.
//
// Declared here and implemented by internal/notify, the same shape the audit
// recorder has: this package decides *when* somebody must be told and knows
// nothing about inboxes or mail. Nil sends nothing, and then the grace window
// still runs — the warning is what makes the stop fair, not what makes it work.
type DomainNotifier interface {
	WarnDomainFailing(ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
		hostname, reason string, stopsAt time.Time) error
	WarnDomainUnverified(ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
		hostname, reason string) error
}

// DomainVerification is the challenge as the dashboard and the API print it.
//
// The record name and value are given in full, because the person reading this
// is about to paste them into a DNS provider's form and reconstructing
// `_linkctrl-challenge.` + hostname by hand is exactly where a typo lands.
type DomainVerification struct {
	RecordType string `json:"record_type"`
	RecordName string `json:"record_name"`
	RecordData string `json:"record_data"`
	// CheckedAt is when the last check ran, whatever it concluded. Absent means
	// none has.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// FailingSince anchors the grace window. Absent means the last check passed.
	FailingSince *time.Time `json:"failing_since,omitempty"`
	// StopsAt is when serving stops if nothing changes. Absent unless the domain
	// is both serving and failing, because it is a threat only in that state.
	StopsAt *time.Time `json:"stops_at,omitempty"`
	// Error is what the last failed check said, in the sentence the page shows.
	Error string `json:"error,omitempty"`
}

// verificationOf builds the challenge view from a row.
func (s *Service) verificationOf(row dbgen.Domain) *DomainVerification {
	if row.VerificationToken == nil || row.IsDefault {
		return nil
	}
	v := &DomainVerification{
		RecordType: "TXT",
		RecordName: domain.ChallengeRecordName(row.Hostname),
		RecordData: *row.VerificationToken,
		CheckedAt:  row.VerificationCheckedAt,
	}
	if row.VerificationError != nil {
		v.Error = *row.VerificationError
	}
	if row.VerificationFailingSince != nil {
		v.FailingSince = row.VerificationFailingSince
		if row.VerifiedAt != nil {
			stops := row.VerificationFailingSince.Add(s.verifyGrace())
			v.StopsAt = &stops
		}
	}
	return v
}

// verifyGrace is how long a serving hostname keeps serving after its first
// failed check (D70). Zero configuration means the default, never "no window":
// an unset knob must not turn the first DNS hiccup into an outage.
func (s *Service) verifyGrace() time.Duration {
	if s.verifyGraceWindow <= 0 {
		return DefaultVerifyGrace
	}
	return s.verifyGraceWindow
}

// DefaultVerifyGrace is the grace window an operator who sets nothing gets.
//
// **Twenty-four hours, and the number is a judgement rather than a measurement.**
// It is bounded below by what a human can act on: the workspace is told at the
// first failure, and a window shorter than a working day would notify somebody
// at 02:00 and stop serving their links before they read it, which is a warning
// that only exists to have technically been given. It is bounded above by what
// it costs — for the length of the window this instance keeps serving a hostname
// whose DNS its owner may no longer control — and a week of that is not a grace
// period, it is a policy of ignoring the answer.
//
// One day is also the largest window that can be stated in the runbook without
// arithmetic, which D70 requires of it.
const DefaultVerifyGrace = 24 * time.Hour

// DefaultVerifyInterval is how often the leader re-checks.
//
// **Hourly.** The point of the cadence is to make a single failure weak evidence
// and a sustained one strong: at this rate a domain must fail twenty-four
// consecutive checks, spread over a day, before serving stops, and a resolver
// blip — which is measured in seconds and minutes — cannot produce that. Faster
// would buy nothing, because the window is a day either way, and would multiply
// the queries a large instance sends to other people's nameservers.
const DefaultVerifyInterval = time.Hour

// VerifyDomain runs the challenge check now, for one domain.
//
// On demand as well as on a cadence, because the person who has just published a
// TXT record should not have to wait out an hour to find out whether they got it
// right — and because the failure message is the only feedback there is when
// they did not.
//
// Guarded by the same ownership check every other write to a domain is: this
// starts serving an alias namespace on a public hostname, which is not a read.
func (s *Service) VerifyDomain(ctx context.Context, actor *auth.Identity, id uuid.UUID) (*Domain, error) {
	row, err := s.domainForWrite(ctx, actor, id, "verifying")
	if err != nil {
		return nil, err
	}
	if s.dns == nil {
		return nil, fmt.Errorf("%w: this process cannot resolve DNS, so a hostname "+
			"cannot be verified here", domain.ErrUnavailable)
	}

	checked, reason := s.checkChallenge(ctx, row)
	if !checked {
		// Recorded even though this is the on-demand path, so the row's "last
		// check" is the last check whoever ran it — a page that showed only what
		// the hourly job found would go on saying "no record" for an hour after
		// somebody fixed it and pressed the button.
		//
		// It also starts the grace window on a *serving* hostname, which is
		// correct: a failed check is a failed check, and D70's clock does not
		// care who asked for it.
		if _, ferr := s.q.MarkDomainVerificationFailed(ctx,
			dbgen.MarkDomainVerificationFailedParams{ID: row.ID, VerificationError: reason}); ferr != nil {
			return nil, fmt.Errorf("record failed verification: %w", ferr)
		}
		// A validation error rather than a 500: nothing went wrong here, the
		// record is not there yet. The message carries the name and the value so
		// the page that shows the refusal shows what to publish.
		return nil, domain.ValidationErrors{{
			Field: "hostname", Code: "unverified",
			Message: fmt.Sprintf("%s is not verified: %s. Publish a TXT record at %s "+
				"with the value %s, then check again.",
				row.Hostname, reason, domain.ChallengeRecordName(row.Hostname),
				derefString(row.VerificationToken)),
		}}
	}

	verified, err := s.q.MarkDomainVerified(ctx, verifiedWrite(row))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domainChangedUnderCheck(row.Hostname)
		}
		return nil, fmt.Errorf("mark domain verified: %w", err)
	}
	// Only when it *became* verified. Re-running the check on a domain that has
	// been serving for a month is not an administrative change, and recording one
	// every time somebody presses the button would bury the event that matters.
	if row.VerifiedAt == nil {
		s.recordDomainEvent(ctx, actor, audit.ActionDomainVerified, verified.ID, map[string]any{
			// The name that was **checked**, taken from the row the challenge was
			// resolved against rather than from the row the write returned. They
			// are the same name now, because the write refuses to land on any
			// other — and the record is stamped from the checked one anyway, so
			// that what the log claims was proved is what a lookup actually
			// proved, whatever a later reader of this code does to the statement.
			"hostname": row.Hostname,
		})
	}
	s.invalidateHosts(ctx)

	links, err := s.q.CountLinksOnDomain(ctx, verified.ID)
	if err != nil {
		return nil, fmt.Errorf("count links on domain: %w", err)
	}
	out := domainFromRow(verified, links, true)
	out.Verification = s.verificationOf(verified)
	return out, nil
}

// verifiedWrite builds the conditional write from the row that was checked.
//
// **The row is the argument because the row is the proof.** Both call sites read
// a domain, ask a nameserver about the hostname on it, and write back — and
// between those two steps a rename can commit, clearing `verified_at` and giving
// the row a name nobody has proved anything about. Passing the id alone let that
// write land; passing the hostname and the token the check was made against makes
// it affect zero rows instead. Built here rather than at each call site so the
// two cannot drift into disagreeing about what was proved.
func verifiedWrite(row dbgen.Domain) dbgen.MarkDomainVerifiedParams {
	return dbgen.MarkDomainVerifiedParams{
		ID: row.ID, Hostname: row.Hostname, VerificationToken: row.VerificationToken,
	}
}

// domainChangedUnderCheck is what the on-demand path answers when the write found
// no row to land on.
//
// A validation error rather than a 500, and rather than silence: nothing went
// wrong, the domain changed while its DNS was being read, and the honest answer
// is that this check proved nothing about whatever the row says now. Running it
// again is the whole remedy.
func domainChangedUnderCheck(hostname string) error {
	return domain.ValidationErrors{{
		Field: "hostname", Code: "conflict",
		Message: fmt.Sprintf("%s changed while its DNS record was being checked, so nothing "+
			"was verified: a check proves control of the hostname it read, and this row no "+
			"longer carries that name. Check it again.", hostname),
	}}
}

// checkChallenge asks DNS and reports whether the record is there, plus the
// sentence to show when it is not.
//
// The failure sentence is built here rather than at the call sites so that the
// job and the on-demand check cannot describe the same failure differently — an
// operator comparing the page against the log has to be reading one answer.
func (s *Service) checkChallenge(ctx context.Context, row dbgen.Domain) (bool, string) {
	name := domain.ChallengeRecordName(row.Hostname)
	if row.VerificationToken == nil || *row.VerificationToken == "" {
		return false, "this hostname has no challenge token; remove and register it again"
	}
	records, err := s.dns.LookupTXT(ctx, name)
	if err != nil {
		if notFound(err) {
			return false, fmt.Sprintf("no TXT record was found at %s", name)
		}
		// A lookup that failed for any other reason is not evidence that the
		// record is absent, and it is reported as what it is. It still counts as
		// a failed check — D70's window exists precisely because a single failed
		// check is weak evidence — but the sentence does not accuse the owner of
		// not having published anything.
		return false, fmt.Sprintf("the DNS lookup for %s did not complete: %v", name, err)
	}
	if domain.ChallengeSatisfied(*row.VerificationToken, records) {
		return true, ""
	}
	return false, fmt.Sprintf("a TXT record exists at %s but none of its values is this "+
		"domain's token", name)
}

// DomainCheckSummary is what one re-verification pass did, for the job's log.
type DomainCheckSummary struct {
	Checked    int
	Verified   int
	Failing    int
	Unverified int
}

// ReverifyDomains is the cadence half of D70, run by the leader.
//
// Three outcomes per domain and the third is the one the milestone is about:
//
//   - The check passes. The domain is verified — which is also how a hostname
//     registered an hour ago starts serving without anybody pressing a button —
//     and any failing streak is cleared.
//   - The check fails and the window has not elapsed. The failure is recorded,
//     the workspace is notified the *first* time, and **serving continues**. A
//     poll against somebody else's nameserver is weak evidence; building an
//     outage trigger out of one failed query is how an availability feature
//     becomes an availability incident.
//   - The check fails and the window has elapsed. Serving **stops**:
//     `verified_at` is cleared, the hostname goes back to ops-only 404, and the
//     change is broadcast so no replica keeps serving a domain this one has just
//     unverified. This is a stop and not an escalation — a grace period whose
//     expiry issues another warning is the silent persistence this milestone
//     forbids, reached by a gentler route.
//
// Which hostnames a pass reaches, and in what order, is verificationWorkList's —
// and it is a security property rather than a scheduling preference, because the
// third outcome above is the only one this instance ever performs on its own.
//
// Errors are collected rather than returned on the first one: a nameserver that
// is refusing queries for one customer must not stop the other customers' domains
// being checked.
func (s *Service) ReverifyDomains(ctx context.Context, now time.Time, batch int32) (DomainCheckSummary, error) {
	var sum DomainCheckSummary
	if s.dns == nil {
		return sum, nil
	}
	if batch <= 0 {
		batch = 200
	}
	rows, err := s.verificationWorkList(ctx, batch)
	if err != nil {
		return sum, err
	}

	changed := false
	var errs []error
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		full, err := s.q.GetDomainByID(ctx, r.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			errs = append(errs, fmt.Errorf("read domain %s: %w", r.Hostname, err))
			continue
		}
		sum.Checked++
		ok, reason := s.checkChallenge(ctx, full)
		if ok {
			verified, err := s.q.MarkDomainVerified(ctx, verifiedWrite(full))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// The row was renamed or removed while this lookup was in
					// flight, so the check that just passed proved control of a
					// name this row no longer has. Not an error to collect — a
					// rename is something its owner is entitled to do at any
					// moment — but logged, because the same shape is what an
					// attempt to verify a name nobody holds looks like.
					s.log.Warn("a domain changed while its verification was in flight; "+
						"nothing was verified",
						slog.String("hostname", full.Hostname),
						slog.String("domain_id", full.ID.String()))
					continue
				}
				errs = append(errs, fmt.Errorf("verify %s: %w", full.Hostname, err))
				continue
			}
			if full.VerifiedAt == nil {
				sum.Verified++
				changed = true
				// Stamped with the name the check was made against, for the
				// reason VerifyDomain stamps it that way.
				s.recordDomainEvent(ctx, systemActor(full), audit.ActionDomainVerified,
					verified.ID, map[string]any{"hostname": full.Hostname})
			} else if full.VerificationFailingSince != nil {
				// It recovered inside the window. Not an audit event — nothing
				// administrative happened — but the host cache carries a root
				// redirect and an ssl_status that may have moved with it.
				changed = true
			}
			continue
		}

		failed, err := s.q.MarkDomainVerificationFailed(ctx,
			dbgen.MarkDomainVerificationFailedParams{ID: full.ID, VerificationError: reason})
		if err != nil {
			errs = append(errs, fmt.Errorf("record failure for %s: %w", full.Hostname, err))
			continue
		}
		if full.VerifiedAt == nil {
			// Never served, so there is nothing to take away and nobody to warn:
			// a hostname registered and not yet pointed at us fails every check
			// until its owner publishes the record, which is not an incident.
			continue
		}
		sum.Failing++

		since := failed.VerificationFailingSince
		if since == nil {
			// Cannot happen — the statement COALESCEs it — but a nil here would
			// otherwise be read as "no window", which is the unsafe direction.
			now := now
			since = &now
		}
		if full.VerificationFailingSince == nil {
			// The first failure of this run. Told before the stop rather than
			// only at it, which is D70's third constraint.
			s.warnDomainFailing(ctx, full, reason, since.Add(s.verifyGrace()))
			continue
		}
		if now.Sub(*since) < s.verifyGrace() {
			continue
		}

		stopped, err := s.q.UnverifyDomain(ctx, dbgen.UnverifyDomainParams{
			ID: full.ID, VerificationError: reason,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("unverify %s: %w", full.Hostname, err))
			continue
		}
		sum.Unverified++
		changed = true
		s.recordDomainEvent(ctx, systemActor(full), audit.ActionDomainUnverified,
			stopped.ID, map[string]any{
				"hostname":      stopped.Hostname,
				"failing_since": since.UTC().Format(time.RFC3339),
				"reason":        reason,
			})
		s.warnDomainUnverified(ctx, full, reason)
	}

	if changed {
		// One broadcast for the whole pass. A pass that unverified three domains
		// publishing three messages would make every replica reload three times
		// to reach the same set.
		s.invalidateHosts(ctx)
	}
	return sum, errors.Join(errs...)
}

// domainCheckTarget is one row on the pass's work list: which row, and the name
// it carried when the list was drawn.
//
// The name is here for the log line and nothing else. What the check is made
// against is re-read from the row a moment later, and what the write is
// predicated on comes from that re-read — a list is a plan, not evidence.
type domainCheckTarget struct {
	ID       uuid.UUID
	Hostname string
}

// verificationWorkList decides which hostnames this pass checks, and in what
// order.
//
// **Serving hostnames first, and their class is drawn separately (M40,
// reopened).** One queue ordered by last check looks fair and is not: a rename
// writes `verification_checked_at` back to NULL, so a workspace renaming its
// registrations in a loop held the head of that queue indefinitely, and a serving
// hostname — which always carries a watermark, because a passing check is what
// made it serve — sorted behind all of it and was never reached. What that
// starved was not throughput. It was the only mechanism that ever stops serving a
// hostname whose DNS has gone, and it stopped instance-wide while every pass
// logged healthy counts.
//
// Two draws close it because a rename *un-verifies*: churn can only crowd the
// pending class. The serving class is drawn first and takes what it needs of the
// batch; the pending class takes what is left, so the total is the bound the
// operator configured and not twice it. A pass that runs out of its deadline
// therefore drops pending rows, where waiting another hour costs a hostname
// another hour of not yet serving, rather than serving rows, where it costs
// everyone another hour of a name being served on proof that has expired.
//
// Raising the batch was the other obvious move and it is strictly worse: more
// rows inside the same ten minutes, each able to block for the DNS timeout, is
// the same starvation with a longer queue.
func (s *Service) verificationWorkList(ctx context.Context, batch int32) ([]domainCheckTarget, error) {
	serving, err := s.q.ListServingDomainsForVerification(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("list serving domains for verification: %w", err)
	}
	out := make([]domainCheckTarget, 0, len(serving))
	for _, r := range serving {
		out = append(out, domainCheckTarget{ID: r.ID, Hostname: r.Hostname})
	}

	remaining := batch - int32(len(serving)) //nolint:gosec // G115: LIMIT batch bounds it
	if remaining <= 0 {
		return out, nil
	}
	pending, err := s.q.ListPendingDomainsForVerification(ctx, remaining)
	if err != nil {
		return nil, fmt.Errorf("list pending domains for verification: %w", err)
	}
	for _, r := range pending {
		out = append(out, domainCheckTarget{ID: r.ID, Hostname: r.Hostname})
	}
	return out, nil
}

// systemActor attributes a job-driven change to the instance, in the log of the
// organization that owns the hostname.
//
// Scoped rather than left global: the audit list is read per organization, and
// an event with no organization would be written where nobody who cares about it
// can see it. The label is "system" because that is what took the action —
// nobody pressed anything.
func systemActor(row dbgen.Domain) *auth.Identity {
	a := &auth.Identity{Name: "system"}
	if row.OrganizationID != nil {
		a.OrgID = *row.OrganizationID
	}
	if row.WorkspaceID != nil {
		a.WorkspaceID = *row.WorkspaceID
	}
	return a
}

func (s *Service) warnDomainFailing(ctx context.Context, row dbgen.Domain, reason string, stopsAt time.Time) {
	if s.domainNotify == nil || row.OrganizationID == nil {
		return
	}
	if err := s.domainNotify.WarnDomainFailing(ctx, *row.OrganizationID, row.WorkspaceID,
		row.Hostname, reason, stopsAt); err != nil {
		s.log.Warn("could not warn a workspace that its domain is failing verification",
			slog.String("hostname", row.Hostname), slog.Any("error", err))
	}
}

func (s *Service) warnDomainUnverified(ctx context.Context, row dbgen.Domain, reason string) {
	if s.domainNotify == nil || row.OrganizationID == nil {
		return
	}
	if err := s.domainNotify.WarnDomainUnverified(ctx, *row.OrganizationID, row.WorkspaceID,
		row.Hostname, reason); err != nil {
		s.log.Warn("could not tell a workspace that its domain stopped being served",
			slog.String("hostname", row.Hostname), slog.Any("error", err))
	}
}

// invalidateHosts makes the change visible on every replica.
//
// Called after every write that changes what ListVerifiedDomains returns:
// verification, un-verification, a rename, a removal. Missing one of these is
// precisely the cross-replica staleness gap this milestone exists to close, so
// they are enumerated here rather than left to each call site to remember.
func (s *Service) invalidateHosts(ctx context.Context) {
	if s.hosts != nil {
		s.hosts.InvalidateHosts(ctx)
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// notFound separates "there is no such record" from "the lookup did not
// complete", without this package naming a net symbol.
//
// The distinction matters to the sentence a workspace reads: one says publish
// the record, the other says something is wrong with the query. It is drawn on
// the interface net.DNSError satisfies rather than on the concrete type, so the
// guard test in feed_test.go stays satisfied and a stub resolver can report
// either outcome.
func notFound(err error) bool {
	var e interface {
		error
		Timeout() bool
		Temporary() bool
	}
	if !errors.As(err, &e) {
		return false
	}
	// A DNS miss is neither a timeout nor temporary; every other failure the
	// resolver reports is one or the other.
	return !e.Timeout() && !e.Temporary()
}
