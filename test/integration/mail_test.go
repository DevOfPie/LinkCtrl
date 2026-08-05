//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// ─── Fakes ───────────────────────────────────────────────────────────────────

// recordingSender stands in for a relay in the tests that are about the outbox
// rather than about SMTP. The transport gets its own test against a real socket
// further down.
type recordingSender struct {
	mu   sync.Mutex
	sent []sentMail
	// failWith, when set, fails every send. What a relay that is refusing looks
	// like from this side.
	failWith error
}

type sentMail struct{ To, Subject, Body string }

func (s *recordingSender) Send(_ context.Context, to, subject, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.sent = append(s.sent, sentMail{To: to, Subject: subject, Body: body})
	return nil
}

func (s *recordingSender) delivered() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMail(nil), s.sent...)
}

// mailFixture is a workspace with an owner, plus whichever half of the mailer a
// test needs.
type mailFixture struct {
	pool   *pgxpool.Pool
	notify *notify.Service
	mail   *mail.Service
	sender *recordingSender
	owner  *auth.Identity
}

// newMailFixture builds the instance. withMailer says whether an SMTP relay is
// configured, which is the only difference between the two halves of this
// milestone's central claim.
func newMailFixture(t *testing.T, withMailer bool) *mailFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	f := &mailFixture{pool: pool, notify: notify.NewService(pool), owner: owner}
	if !withMailer {
		return f
	}

	f.sender = &recordingSender{}
	f.mail = newMailService(t, pool, f.sender)
	f.notify = f.notify.WithMail(f.mail, "https://links.example.com")
	return f
}

func newMailService(t *testing.T, pool *pgxpool.Pool, sender mail.Sender) *mail.Service {
	t.Helper()
	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	svc, err := mail.NewService(pool, mail.Config{Renderer: renderer, Sender: sender})
	if err != nil {
		t.Fatalf("mail.NewService: %v", err)
	}
	return svc
}

// outbox returns every row, newest last, as (recipient, kind, status, attempts).
func (f *mailFixture) outbox(t *testing.T) []outboxRow {
	t.Helper()
	return outboxRows(t, f.pool)
}

type outboxRow struct {
	Recipient, Kind, Subject, Body, Status, LastError string
	Attempts                                          int
}

// ─── The claim that keeps the mailer optional ────────────────────────────────

// With no mailer configured, the audit-growth warning still reaches the owner
// in the dashboard, and nothing at all is queued.
//
// This is M26's headline claim and the reason the outbox was chosen over an
// in-memory retry (D23): "the outbox stays empty" is something a test can hold,
// where "nothing was sent" is not. Both halves are asserted, because a mailer
// that degraded by dropping the notification too would also produce an empty
// outbox.
func TestUnconfiguredMailerLeavesTheOutboxEmptyAndTheInboxWorking(t *testing.T) {
	f := newMailFixture(t, false)

	if err := f.notify.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatalf("WarnAuditGrowth with no mailer configured returned an error; "+
			"a consumer must degrade to its mail-free behaviour, not fail: %v", err)
	}

	if n, err := f.notify.Unread(t.Context(), f.owner); err != nil || n != 1 {
		t.Errorf("owner has %d unread notifications (err %v), want 1: in-app "+
			"delivery is the baseline and does not depend on a mailer", n, err)
	}
	if rows := f.outbox(t); len(rows) != 0 {
		t.Errorf("the outbox has %d rows on an instance with no mailer: %+v", len(rows), rows)
	}
}

// The same warning, with a mailer. One mail per owner, queued rather than sent,
// and rendered from the template rather than from the notification's prose.
func TestConfiguredMailerQueuesTheAuditGrowthWarning(t *testing.T) {
	f := newMailFixture(t, true)

	if err := f.notify.WarnAuditGrowth(t.Context(), 6_000_000_000, 5_368_709_120); err != nil {
		t.Fatal(err)
	}

	// Still in the inbox. Mail is the addition, never the delivery: an owner
	// who reads the dashboard must not have to also read their mail.
	if n, _ := f.notify.Unread(t.Context(), f.owner); n != 1 {
		t.Errorf("owner has %d unread notifications, want 1", n)
	}

	rows := f.outbox(t)
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Recipient != "owner@example.com" {
		t.Errorf("recipient = %q", got.Recipient)
	}
	if got.Kind != notify.MailAuditGrowth {
		t.Errorf("kind = %q, want %q", got.Kind, notify.MailAuditGrowth)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending: Enqueue must not send", got.Status)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d on a freshly queued row, want 0", got.Attempts)
	}
	// The numbers a person actually needs, in the words the template chose.
	for _, want := range []string{"5.0 GiB", "LINKCTRL_AUDIT_RETENTION_DAYS"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body does not mention %q:\n%s", want, got.Body)
		}
	}
	if !strings.Contains(got.Subject, "audit log") {
		t.Errorf("subject = %q", got.Subject)
	}
	// Greeted by name, not by address. The name column defaults to empty, so
	// the fallback is the common case and this is the one that is not.
	if !strings.Contains(got.Body, "Hello Owner,") {
		t.Errorf("the mail does not greet the owner by name:\n%s", got.Body)
	}
	// Nothing has been delivered: queueing is not sending, and the scheduler is
	// what closes that gap.
	if n := len(f.sender.delivered()); n != 0 {
		t.Errorf("%d messages were delivered by Enqueue; sends belong to the scheduler", n)
	}

	// The re-notify guard covers the mail as well as the inbox row, because the
	// mail hangs off it. A threshold that stays crossed must not produce a mail
	// an hour.
	for range 3 {
		if err := f.notify.WarnAuditGrowth(t.Context(), 6_000_000_000, 5_368_709_120); err != nil {
			t.Fatal(err)
		}
	}
	if rows := f.outbox(t); len(rows) != 1 {
		t.Errorf("four runs over the threshold queued %d mails, want 1", len(rows))
	}
}

// Durability, which is the entire reason D23 chose a table.
//
// The message is queued by one Service and delivered by a different one, over a
// pool opened afterwards. Nothing in the first process's memory is available to
// the second — which is what a restart is.
func TestQueuedMailSurvivesARestart(t *testing.T) {
	f := newMailFixture(t, true)

	if err := f.notify.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}
	if rows := f.outbox(t); len(rows) != 1 {
		t.Fatalf("outbox has %d rows before the restart, want 1", len(rows))
	}

	// The restart. A new pool, a new Service, a new sender: the only thing
	// carried across is the row.
	restarted, err := pgxpool.New(t.Context(), f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	sender := &recordingSender{}
	if err := newMailService(t, restarted, sender).Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	sent := sender.delivered()
	if len(sent) != 1 {
		t.Fatalf("the restarted process delivered %d messages, want 1: mail queued "+
			"before a restart is exactly what the outbox exists to keep", len(sent))
	}
	if sent[0].To != "owner@example.com" {
		t.Errorf("delivered to %q", sent[0].To)
	}

	rows := f.outbox(t)
	if len(rows) != 1 || rows[0].Status != "sent" {
		t.Fatalf("after draining, the row is %+v; want one row marked sent", rows)
	}
	if rows[0].Attempts != 1 {
		t.Errorf("attempts = %d after one successful delivery, want 1", rows[0].Attempts)
	}

	// Draining again must not send it twice. A second delivery of an invitation
	// is confusing; a second delivery of a verification link is worse.
	if err := newMailService(t, restarted, sender).Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(sender.delivered()); n != 1 {
		t.Errorf("a second drain delivered the message again (%d total)", n)
	}
}

// Bounded retry, at both ends: a failure reschedules rather than losing the
// message, and the fifth failure is the last.
func TestFailedDeliveryRetriesAndThenGivesUp(t *testing.T) {
	f := newMailFixture(t, true)
	f.sender.failWith = fmt.Errorf("mail: connect to smtp.example.com:587: connection refused")

	if err := f.notify.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= mail.MaxAttempts; attempt++ {
		// Drain reports the failure rather than swallowing it: the job runner
		// is what turns that into a log line and a metric.
		if err := f.mail.Drain(t.Context()); err == nil {
			t.Fatalf("attempt %d: Drain returned no error although every send failed", attempt)
		}

		rows := f.outbox(t)
		if len(rows) != 1 {
			t.Fatalf("outbox has %d rows", len(rows))
		}
		if rows[0].Attempts != attempt {
			t.Errorf("after %d drains, attempts = %d", attempt, rows[0].Attempts)
		}
		// The error is kept verbatim. It is what an operator reads when
		// somebody reports that mail never arrived.
		if !strings.Contains(rows[0].LastError, "connection refused") {
			t.Errorf("last_error = %q, want the send failure verbatim", rows[0].LastError)
		}

		wantStatus := "pending"
		if attempt == mail.MaxAttempts {
			wantStatus = "failed"
		}
		if rows[0].Status != wantStatus {
			t.Fatalf("after attempt %d of %d the row is %q, want %q",
				attempt, mail.MaxAttempts, rows[0].Status, wantStatus)
		}

		// Each attempt is scheduled into the future, so the next tick does not
		// immediately retry it. Wound back here rather than waited out.
		if attempt < mail.MaxAttempts {
			if err := f.mail.Drain(t.Context()); err != nil {
				t.Errorf("a drain before the backoff elapsed did something: %v", err)
			}
			if rows := f.outbox(t); rows[0].Attempts != attempt {
				t.Fatalf("a message was retried before its backoff elapsed "+
					"(attempts = %d after %d drains)", rows[0].Attempts, attempt)
			}
			if _, err := f.pool.Exec(t.Context(),
				`UPDATE mail_outbox SET next_attempt_at = now() - interval '1 hour'`); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Terminal. A failed row is never picked up again, however long it sits.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE mail_outbox SET next_attempt_at = now() - interval '1 year'`); err != nil {
		t.Fatal(err)
	}
	if err := f.mail.Drain(t.Context()); err != nil {
		t.Errorf("a failed row was picked up again: %v", err)
	}
	if rows := f.outbox(t); rows[0].Attempts != mail.MaxAttempts {
		t.Errorf("attempts = %d after the row was abandoned, want %d",
			rows[0].Attempts, mail.MaxAttempts)
	}
}

// Finding F32, on the half a delivered message never reaches.
//
// A message that was never delivered is the case that needs the scrub most: its
// token is by definition unspent, and a failed row is kept for thirty days
// against an invitation that lives seven. So the abandoned row keeps the whole
// record of what was attempted and none of what was going to be said.
//
// The second half is the part that cannot rot. F32 exists because a claim held
// by convention — *"the first mail this ships contains no secret"* — stayed in
// the documentation while two templates carrying a token landed beside it. So
// the guarantee is a CHECK constraint rather than two queries that happen to
// agree, and this asserts the database itself refuses the state, which is what
// makes a future edit dropping the scrub fail loudly instead of quietly.
func TestAnAbandonedMessageKeepsItsRecordAndNotItsContents(t *testing.T) {
	f := newMailFixture(t, true)
	f.sender.failWith = fmt.Errorf("mail: connect to smtp.example.com:587: connection refused")

	if err := f.notify.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}
	// While it is still being retried it keeps its body, because it is still
	// going to be sent. That is the boundary, and asserting it here is what
	// stops the fix drifting into "scrub on the first failure", which would
	// deliver an empty message on the second attempt.
	for attempt := 1; attempt <= mail.MaxAttempts; attempt++ {
		if err := f.mail.Drain(t.Context()); err == nil {
			t.Fatalf("attempt %d: Drain succeeded although every send failed", attempt)
		}
		rows := f.outbox(t)
		if attempt < mail.MaxAttempts {
			if rows[0].Body == "" {
				t.Fatalf("the message was emptied on attempt %d of %d, with retries "+
					"still to come; the next attempt would send nothing",
					attempt, mail.MaxAttempts)
			}
			if _, err := f.pool.Exec(t.Context(),
				`UPDATE mail_outbox SET next_attempt_at = now() - interval '1 hour'`); err != nil {
				t.Fatal(err)
			}
		}
	}

	rows := f.outbox(t)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("after %d failures the outbox is %+v, want one failed row",
			mail.MaxAttempts, rows)
	}
	if rows[0].Body != "" {
		t.Errorf("an abandoned message kept its body: %q", rows[0].Body)
	}
	// Everything an operator reads when somebody says the mail never arrived.
	if rows[0].Recipient == "" || rows[0].Subject == "" || rows[0].Kind == "" ||
		!strings.Contains(rows[0].LastError, "connection refused") ||
		rows[0].Attempts != mail.MaxAttempts {
		t.Errorf("the record of the attempt was damaged by the scrub: %+v", rows[0])
	}

	// And the state cannot be reached by anything, including SQL that means well.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE mail_outbox SET body = 'the message, put back'`); err == nil {
		t.Error("a finished row accepted a body; the guarantee is a convention " +
			"again, and mail_outbox_finished_body_scrubbed is not doing its job")
	} else if !strings.Contains(err.Error(), "mail_outbox_finished_body_scrubbed") {
		t.Errorf("the write was refused by something other than the constraint: %v", err)
	}
}

// The outbox is a record of what was attempted, not an archive.
func TestFinishedMailIsPurgedAndPendingMailIsNot(t *testing.T) {
	f := newMailFixture(t, true)

	if err := f.notify.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}
	if err := f.mail.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A second message, left pending.
	if err := f.mail.Enqueue(t.Context(), "someone@example.com", notify.MailAuditGrowth,
		map[string]string{
			"Instance": "links.example.com", "Name": "someone@example.com",
			"Size": "6.0 GiB", "Threshold": "5.0 GiB", "AppURL": "https://links.example.com",
		}); err != nil {
		t.Fatal(err)
	}

	// Nothing is old enough yet.
	if n, err := f.mail.PurgeFinished(t.Context()); err != nil || n != 0 {
		t.Errorf("purged %d rows (err %v) before anything aged out", n, err)
	}

	if _, err := f.pool.Exec(t.Context(),
		fmt.Sprintf(`UPDATE mail_outbox SET created_at = now() - interval '%d days'`,
			mail.FinishedRetentionDays+1)); err != nil {
		t.Fatal(err)
	}

	n, err := f.mail.PurgeFinished(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}
	rows := f.outbox(t)
	if len(rows) != 1 || rows[0].Status != "pending" {
		t.Fatalf("after the purge the outbox is %+v; a pending message must "+
			"never be deleted by age — it has not been delivered yet", rows)
	}
	if p, _ := f.mail.Pending(t.Context()); p != 1 {
		t.Errorf("Pending() = %d, want 1", p)
	}
}

// TestPendingMailIsAbandonedOnceThereIsNoRelayToSendIt is the complement of the
// test above, and the two together are the whole rule.
//
// "A pending message must never be deleted by age" is right while a mailer
// exists: Drain claims every pending row and moves it to sent or failed, so a
// pending row is one still on its way. Clearing SMTP_HOST breaks the premise
// rather than the rule — the drain is gated on the mailer and stops running,
// the rows enqueued before the change stay pending forever, and the purge skips
// them by design. Nothing but CountPendingMail could even see them (F52).
//
// Failed rather than deleted, so the record leaves by the same retention path as
// every other finished message; past the same window rather than at once, so an
// operator who clears SMTP_HOST by mistake and restores it the same afternoon
// still gets their queue delivered.
func TestPendingMailIsAbandonedOnceThereIsNoRelayToSendIt(t *testing.T) {
	f := newMailFixture(t, true)
	ctx := t.Context()
	q := dbgen.New(f.pool)

	warn := func(to string) {
		t.Helper()
		if err := f.mail.Enqueue(ctx, to, notify.MailAuditGrowth, map[string]string{
			"Instance": "links.example.com", "Name": to,
			"Size": "6.0 GiB", "Threshold": "5.0 GiB", "AppURL": "https://links.example.com",
		}); err != nil {
			t.Fatal(err)
		}
	}
	warn("stale@example.com")
	warn("fresh@example.com")

	// Nothing is old enough, so the relay going away costs nothing yet. This is
	// the half that makes the window real rather than decorative.
	if n, err := q.AbandonUnsendableMail(ctx, int32(mail.FinishedRetentionDays)); err != nil || n != 0 {
		t.Fatalf("abandoned %d rows (err %v) while both were inside the window", n, err)
	}

	if _, err := f.pool.Exec(ctx,
		fmt.Sprintf(`UPDATE mail_outbox SET created_at = now() - interval '%d days'
		              WHERE recipient = 'stale@example.com'`,
			mail.FinishedRetentionDays+1)); err != nil {
		t.Fatal(err)
	}

	n, err := q.AbandonUnsendableMail(ctx, int32(mail.FinishedRetentionDays))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("abandoned %d rows, want only the aged one", n)
	}

	var staleRow, freshRow outboxRow
	for _, r := range f.outbox(t) {
		switch r.Recipient {
		case "stale@example.com":
			staleRow = r
		case "fresh@example.com":
			freshRow = r
		}
	}
	if staleRow.Status != "failed" {
		t.Errorf("the aged message is %q, want failed", staleRow.Status)
	}
	if staleRow.Body != "" {
		t.Error("an abandoned message kept its body; it will never be sent and should not hold what it was going to say")
	}
	if !strings.Contains(staleRow.LastError, "no SMTP relay configured") {
		t.Errorf("last_error = %q, want it to say why it was abandoned", staleRow.LastError)
	}
	if freshRow.Status != "pending" {
		t.Errorf("the message inside the window is %q, want it left pending", freshRow.Status)
	}

	// And it now leaves by the ordinary retention path rather than needing a
	// second delete, which is the reason for failing it instead of deleting it.
	if purged, err := f.mail.PurgeFinished(ctx); err != nil || purged != 1 {
		t.Errorf("PurgeFinished removed %d rows (err %v), want the abandoned one", purged, err)
	}
	if p, _ := f.mail.Pending(ctx); p != 1 {
		t.Errorf("Pending() = %d, want the one still inside the window", p)
	}
}

// ─── Finding F133: what one drain costs the rest of the scheduler ────────────

// blockingSender is a relay that holds every send until `want` of them are in
// flight together, and records the high-water mark.
//
// It exists because *the batch went out at once* is a claim about overlap rather
// than about duration. A stopwatch alone would pass on a machine fast enough to
// serialize twenty round trips inside whatever threshold the test picked, and
// fail on a loaded one for reasons that have nothing to do with the code. The
// high-water mark cannot be reached at all without concurrency.
//
// The grace is the fallback a serialized drain hits instead of the barrier, so a
// run against sequential sending finishes in DrainBatch × grace rather than
// hanging until the test's own deadline.
type blockingSender struct {
	want  int
	grace time.Duration

	mu       sync.Mutex
	inFlight int
	high     int
	full     chan struct{}
	filled   bool
}

func newBlockingSender(want int, grace time.Duration) *blockingSender {
	return &blockingSender{want: want, grace: grace, full: make(chan struct{})}
}

func (s *blockingSender) Send(ctx context.Context, _, _, _ string) error {
	s.mu.Lock()
	s.inFlight++
	s.high = max(s.high, s.inFlight)
	if s.inFlight >= s.want && !s.filled {
		s.filled = true
		close(s.full)
	}
	s.mu.Unlock()

	select {
	case <-s.full:
	case <-time.After(s.grace):
	case <-ctx.Done():
	}

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return nil
}

func (s *blockingSender) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.high
}

// TestOneMailDrainSpendsOneSendRatherThanTwenty is finding F133.
//
// The shipped drain sent a claimed batch one message after another on the
// scheduler's single goroutine, so at the default SMTP_TIMEOUT a relay that
// accepts connections and never answers — a firewalled SMTP host, from here —
// held every other job on the instance for DrainBatch × SMTP_TIMEOUT. Go tickers
// hold a cap-one channel, so those ticks were dropped rather than queued, and the
// job that suffered most was the webhook drain running one line later in the same
// select case. That is the shape M42 was reopened for (D95); this is the same
// defect in the package immediately beside it.
//
// What is asserted is the claim mail.go's own DrainBatch comment makes: a backlog
// drains without holding the job for minutes.
func TestOneMailDrainSpendsOneSendRatherThanTwenty(t *testing.T) {
	pool := newDB(t)
	sender := newBlockingSender(mail.DrainBatch, 400*time.Millisecond)
	svc := newMailService(t, pool, sender)

	for i := range mail.DrainBatch {
		if err := svc.Enqueue(t.Context(),
			fmt.Sprintf("backlog-%02d@example.com", i), notify.MailAuditGrowth,
			map[string]string{
				"Instance": "links.example.com", "Name": "Someone",
				"Size": "6.0 GiB", "Threshold": "5.0 GiB", "AppURL": "https://links.example.com",
			}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if n, err := svc.Pending(t.Context()); err != nil || n != mail.DrainBatch {
		t.Fatalf("%d pending messages (err %v), want %d — the batch under test is "+
			"one full claim", n, err, mail.DrainBatch)
	}

	start := time.Now()
	if err := svc.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	elapsed := time.Since(start)

	if peak := sender.peak(); peak != mail.DrainBatch {
		t.Errorf("at most %d messages were in flight at once, want %d — a batch sent "+
			"one message at a time holds the scheduler goroutine for "+
			"DrainBatch × SMTP_TIMEOUT, and every other job's tick is dropped for "+
			"the duration", peak, mail.DrainBatch)
	}
	// The consequence, as arithmetic rather than as a feeling about speed:
	// sending these one after another cannot finish inside half the time the
	// sequential path is obliged to spend.
	if limit := time.Duration(mail.DrainBatch/2) * sender.grace; elapsed >= limit {
		t.Errorf("the drain took %s; %d messages sent one at a time take at least "+
			"%s, and sent together take about %s",
			elapsed, mail.DrainBatch,
			time.Duration(mail.DrainBatch)*sender.grace, sender.grace)
	}

	// Concurrency must not have cost the bookkeeping. Every row is marked, once.
	if n, err := svc.Pending(t.Context()); err != nil || n != 0 {
		t.Errorf("%d messages still pending after the drain (err %v), want 0", n, err)
	}
	rows := outboxRows(t, pool)
	if len(rows) != mail.DrainBatch {
		t.Fatalf("the outbox holds %d rows after the drain, want %d", len(rows), mail.DrainBatch)
	}
	for _, r := range rows {
		if r.Status != "sent" || r.Attempts != 1 {
			t.Errorf("row %s is %q after %d attempts, want sent after 1",
				r.Recipient, r.Status, r.Attempts)
		}
	}
}

// outboxRows reads every row, newest last. A free function rather than a method
// so the tests that build a Service directly, with no mailFixture around it, read
// the table the same way.
func outboxRows(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT recipient, kind, subject, body, status, attempts, last_error
		  FROM mail_outbox ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.Recipient, &r.Kind, &r.Subject, &r.Body,
			&r.Status, &r.Attempts, &r.LastError); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ─── The transport ───────────────────────────────────────────────────────────

// fakeRelay is the smallest thing that speaks enough SMTP to accept a message,
// recording the DATA phase exactly as it arrived on the wire.
//
// A real socket rather than a mocked client, because the properties worth
// asserting here are properties of the bytes: dot-stuffing, CRLF, and a header
// block that a relay would accept.
type fakeRelay struct {
	ln net.Listener

	mu       sync.Mutex
	received []string
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRelay{ln: ln}
	go r.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return r
}

func (r *fakeRelay) addrParts(t *testing.T) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(r.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatal(err)
	}
	return host, p
}

func (r *fakeRelay) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.received...)
}

func (r *fakeRelay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		go r.handle(conn)
	}
}

func (r *fakeRelay) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	br := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	write("220 fake.test ESMTP")

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			// No STARTTLS advertised: these tests dial in clear, which is what
			// SMTP_TLS=none means and the only mode a fake can honestly offer.
			write("250-fake.test")
			write("250 SIZE 10240000")
		case strings.HasPrefix(cmd, "HELO"):
			write("250 fake.test")
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			write("250 OK")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 send it")
			var raw strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				// Recorded before un-stuffing, so a test can see what the
				// client actually put on the wire.
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				raw.WriteString(l)
			}
			r.mu.Lock()
			r.received = append(r.received, raw.String())
			r.mu.Unlock()
			write("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		case strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			write("250 OK")
		default:
			write("500 unknown")
		}
	}
}

// End to end over a socket: enqueue, drain, and read what the relay received.
//
// The assertions are about the wire format, because that is the part no unit
// test over BuildMessage can hold — dot-stuffing is the transport's doing, not
// the message's.
func TestOutboxDeliversOverSMTP(t *testing.T) {
	relay := newFakeRelay(t)
	host, port := relay.addrParts(t)

	sender, err := mail.NewSMTPSender(mail.SMTPOptions{
		Host: host, Port: port,
		From:    "LinkCtrl <links@example.com>",
		TLS:     mail.TLSNone,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify is what runs at boot. A relay that answers must be reported as
	// reachable, or the startup check is noise.
	if err := sender.Verify(t.Context()); err != nil {
		t.Fatalf("Verify against a relay that is answering: %v", err)
	}

	pool := newDB(t)
	svc := newMailService(t, pool, sender)

	// A recipient name carrying a header injection and a body value carrying a
	// lone dot. Both are the hostile-input case, asserted where it actually
	// matters: on the bytes a relay receives.
	if err := svc.Enqueue(t.Context(), "owner@example.com", notify.MailAuditGrowth,
		map[string]string{
			"Instance":  "links.example.com",
			"Name":      "Bob\r\nBcc: everyone@example.com",
			"Size":      "6.0 GiB",
			"Threshold": "5.0 GiB",
			"AppURL":    "https://links.example.com\n.\nMAIL FROM:<attacker@example.com>",
		}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	msgs := relay.messages()
	if len(msgs) != 1 {
		t.Fatalf("the relay received %d messages, want 1", len(msgs))
	}
	wire := msgs[0]

	for _, want := range []string{
		"From: \"LinkCtrl\" <links@example.com>\r\n",
		"To: <owner@example.com>\r\n",
		`Content-Type: text/plain; charset="utf-8"` + "\r\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("the relay did not receive %q\n---\n%s", want, wire)
		}
	}
	// The injection did not become a header, and did not become a line either.
	for _, line := range strings.Split(wire, "\r\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Bcc:") {
			t.Errorf("an interpolated value became a Bcc header: %q", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "MAIL FROM:") {
			t.Errorf("an interpolated value became an SMTP command: %q", line)
		}
		if strings.TrimSpace(line) == "." {
			t.Error("an unstuffed lone dot reached the wire; the DATA phase would " +
				"have ended early and the rest read as commands")
		}
	}
	// Every line ends CRLF: no bare LF anywhere in what the relay was handed.
	if strings.Contains(strings.ReplaceAll(wire, "\r\n", ""), "\n") {
		t.Errorf("the message contains a bare newline:\n%q", wire)
	}

	if rows, err := pool.Query(t.Context(), `SELECT status FROM mail_outbox`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			_ = rows.Scan(&status)
			if status != "sent" {
				t.Errorf("status = %q after a relay accepted the message", status)
			}
		}
	}
}

// A relay that is not there must not stop the process, and must not lose the
// message. Verify reports it; the outbox keeps it.
func TestSendingToAnUnreachableRelayFailsWithoutLosingTheMail(t *testing.T) {
	// A listener opened and immediately closed: the port is certain to be free
	// and certain to refuse, which is what an unconfigured relay looks like.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	_ = ln.Close()

	sender, err := mail.NewSMTPSender(mail.SMTPOptions{
		Host: host, Port: p, From: "links@example.com",
		TLS: mail.TLSNone, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(t.Context()); err == nil {
		t.Fatal("Verify reported a closed port as reachable")
	}

	pool := newDB(t)
	svc := newMailService(t, pool, sender)
	if err := svc.Enqueue(t.Context(), "owner@example.com", notify.MailAuditGrowth,
		map[string]string{
			"Instance": "links.example.com", "Name": "Owner",
			"Size": "6.0 GiB", "Threshold": "5.0 GiB", "AppURL": "https://links.example.com",
		}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Drain(t.Context()); err == nil {
		t.Error("Drain reported success against a closed port")
	}

	var status string
	var attempts int
	if err := pool.QueryRow(t.Context(),
		`SELECT status, attempts FROM mail_outbox`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 1 {
		t.Errorf("status = %q, attempts = %d; a message must survive a relay "+
			"being down, not be dropped", status, attempts)
	}
}

// ─── The schema ──────────────────────────────────────────────────────────────

// The migration is additive, asserted against the file rather than inferred.
//
// Phase 2's standing rule is that DDL is additive within a minor version, and
// the cheapest way to break it is a well-meant ALTER on a table something else
// already depends on. The Up half of this migration creates and does nothing
// else.
func TestMailOutboxMigrationIsAdditive(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root,
		"internal/store/migrations/01100_mail_outbox.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(b), "-- +goose Down")

	for _, line := range strings.Split(up, "\n") {
		stmt := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(stmt, "--") {
			continue
		}
		for _, forbidden := range []string{"DROP ", "ALTER ", "TRUNCATE ", "DELETE ", "UPDATE "} {
			if strings.HasPrefix(stmt, forbidden) {
				t.Errorf("the Up migration is not additive: %q", strings.TrimSpace(line))
			}
		}
	}

	// And the table it creates is really there, with the columns the queries
	// name. A migration that is additive but wrong is no better.
	pool := newDB(t)
	rows, err := pool.Query(t.Context(), `
		SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'mail_outbox'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got[name] = true
	}
	for _, want := range []string{
		"id", "recipient", "subject", "body", "kind",
		"status", "attempts", "next_attempt_at", "last_error", "created_at", "sent_at",
	} {
		if !got[want] {
			t.Errorf("mail_outbox has no %s column", want)
		}
	}
}
