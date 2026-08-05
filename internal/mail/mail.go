// Package mail queues and delivers outbound mail.
//
// Nothing sends on the request path. A consumer calls Enqueue, which renders
// the message and writes one row; the scheduler drains that table on its own
// clock. The table is the reason for the split (decision D23): an invitation
// that vanished because a deploy landed mid-retry would be invisible on both
// ends — nobody receives it, and nobody knows one was attempted.
//
// The whole package is optional. An instance with no SMTP_HOST never builds a
// Service, every consumer holds nil, and the outbox stays empty — which is the
// claim that keeps the mailer optional rather than quietly required.
//
// # What a queued message is worth to somebody reading the database
//
// A rendered body is a credential while the message it renders carries one, and
// two of the four templates do: an invitation and an address verification each
// contain a single-use token whose only other copy in the schema is a SHA-256
// hash. So a row's body is blanked in the same statement that marks it sent or
// failed (finding F32), and the database refuses to hold a finished row that
// still has one. Enqueue to delivery is the window, not the retention window,
// and nothing here shortens it further: a message that has not been delivered
// has to keep the message it is going to send.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Renderer turns a template name and its data into a message.
//
// An interface so this package never imports the one that owns the words.
// internal/ui satisfies it; a test satisfies it in four lines.
type Renderer interface {
	RenderMail(name string, data map[string]string) (subject, body string, err error)
}

// Sender delivers one message. The seam the integration tests substitute at,
// because standing up a real relay to prove the outbox drains would be testing
// somebody else's SMTP server.
//
// **Called from SendConcurrency goroutines at once**, because Drain hands a
// claimed batch to the relay together. SMTPSender qualifies by holding nothing
// across a call — its options are read-only and each Send dials its own
// connection — and anything substituted here has to do the same.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Retry policy. Bounded, and bounded on purpose: a relay that has refused a
// message five times over half an hour is not going to accept it on the sixth,
// and a queue that retries forever is one where a single poisoned row is
// attempted every tick until somebody notices.
const (
	// MaxAttempts is how many deliveries one message gets before it is marked
	// failed. Counted at claim time, so a process that dies mid-send spends an
	// attempt — otherwise a crash loop would retry the same message forever.
	MaxAttempts = 5
	// BackoffBase is the first delay, doubling per attempt up to BackoffMax.
	// With these values the five attempts span roughly half an hour: 1m, 2m,
	// 4m, 8m, 16m.
	BackoffBase = time.Minute
	BackoffMax  = 30 * time.Minute

	// DrainBatch bounds one drain. Small, because each row is a network round
	// trip to somebody else's server and the scheduler runs every half minute
	// anyway: a backlog drains over several runs instead of holding the job for
	// minutes.
	DrainBatch = 20

	// SendConcurrency is how many of a claimed batch are handed to the relay at
	// once, and it is DrainBatch because that is what makes the sentence above
	// true.
	//
	// There was no such constant before and the value it replaces was one, which
	// is finding F133 — the shape M42's reopening had already fixed one package
	// over (D95), on the same scheduler goroutine, one line earlier in the same
	// select case. A batch sent one message after another costs DrainBatch times
	// SMTP_TIMEOUT: twenty times ten seconds at the shipped defaults, against a
	// relay that accepts connections and then says nothing, which is what a
	// firewalled SMTP host looks like from here. That time is spent inline on the
	// goroutine that reads every scheduled job's ticker, and a Go ticker holds one
	// tick, so the rest are dropped rather than queued. The cost never landed on
	// mail: it landed on webhook delivery, on automation's advertised clock, on
	// domain re-verification and on the rollups.
	//
	// Equal to DrainBatch rather than smaller, for D95's reason. Any value below
	// it puts the batch size back into the wall-clock cost — a limit of eight is
	// three waves, thirty seconds at the default timeout, and back inside the tick
	// it has to fit under. What keeps the number small is the claim itself: a
	// drain never holds more than DrainBatch rows, so it can never open more than
	// that at once.
	//
	// **The difference from webhooks is where the connections go.** A webhook
	// batch is spread over as many receivers as it has rows; this one opens up to
	// twenty sessions to the single relay the operator named. A relay that caps
	// concurrent connections below twenty refuses the extra ones, and a refused
	// message is a spent attempt that retries with backoff — the path a refusal
	// already takes, and no message is lost by it. What it costs is the tail: a
	// backlog held continuously above the cap for five attempts abandons the
	// overflow where the sequential version would have crawled. That trade is
	// taken deliberately, because the stall it removes is instance-wide and
	// needs no misconfiguration, while this needs a relay that is both
	// connection-capped and continuously saturated.
	SendConcurrency = DrainBatch

	// FinishedRetentionDays is how long a sent or failed row is kept.
	//
	// The outbox is a record of what was attempted, not an archive. Without a
	// window it would be the one table in this schema that grows forever with
	// nothing watching it, which is the shape D5 and M21 exist to stop
	// repeating.
	//
	// It bounds the record and not a secret. A row that reaches this window lost
	// its body when it finished, so lowering the number would shorten no
	// credential's exposure — which is why F32 was fixed by scrubbing rather
	// than by tightening this.
	FinishedRetentionDays = 30
)

// Service is the outbox: enqueue on one side, drain on the other.
type Service struct {
	q        *dbgen.Queries
	renderer Renderer
	sender   Sender
	log      *slog.Logger
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree: the package that does the work
// does not read the environment.
type Config struct {
	Renderer Renderer
	Sender   Sender
	Logger   *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Renderer == nil {
		return nil, errors.New("mail: no renderer")
	}
	if cfg.Sender == nil {
		return nil, errors.New("mail: no sender")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{q: dbgen.New(pool), renderer: cfg.Renderer, sender: cfg.Sender, log: log}, nil
}

// Enqueuer is the writing half, as a consumer sees it.
//
// Consumers hold this rather than *Service so that "no mailer configured" is a
// nil interface rather than a flag every consumer has to remember to check —
// and so a consumer's tests need neither a database nor a relay.
type Enqueuer interface {
	Enqueue(ctx context.Context, to, kind string, data map[string]string) error
}

// Enqueue renders a message and queues it.
//
// The message is rendered here, not at send time, and stored rendered. A
// template change must not silently rewrite a mail somebody is already waiting
// for, and a row has to stay readable after the code that produced it is gone.
func (s *Service) Enqueue(ctx context.Context, to, kind string, data map[string]string) error {
	addr, err := mail.ParseAddress(to)
	if err != nil {
		// Refused rather than sanitized. An address is not free text: a value
		// that does not parse is a caller bug or an attack, and silently
		// repairing it would send the mail somewhere nobody chose.
		return fmt.Errorf("mail: recipient %q is not a valid address: %w", to, err)
	}

	subject, body, err := s.renderer.RenderMail(kind, data)
	if err != nil {
		return err
	}

	// The bare address, not the display name the caller may have wrapped it in.
	// Nothing downstream needs the name, and dropping it here means no display
	// name ever reaches a header.
	if err := s.q.EnqueueMail(ctx, dbgen.EnqueueMailParams{
		ID:        uuid.Must(uuid.NewV7()),
		Recipient: addr.Address,
		Subject:   subject,
		Body:      body,
		Kind:      kind,
	}); err != nil {
		return fmt.Errorf("mail: enqueue %s: %w", kind, err)
	}
	return nil
}

// Drain sends everything due, and is what the scheduler calls.
//
// One row's failure never stops the batch: errors are collected and the
// remaining messages are still attempted, because a single unreachable
// recipient must not hold up everyone else's mail.
//
// **The batch goes to the relay together, and Drain does not return until all of
// it has.** Both halves are load-bearing, and the second is why this is written
// with a WaitGroup rather than fired and forgotten. The caller is `withLeadership`
// in cmd/linkctrl/jobs.go — the same caller the webhook drain has, with the same
// constraint, which is what made D95's shape correct here rather than merely
// similar. It holds D77's advisory lock on a pooled connection it releases the
// moment the function it was given returns, so a goroutine that outlived this call
// would send *without* the lock, and `pg_try_advisory_lock` is session-scoped, so
// a goroutine of its own would not even have one. Waiting here means the lock
// covers every send exactly as it did when they were sequential, and neither D77
// nor the skip-locked claim beneath it moves.
func (s *Service) Drain(ctx context.Context) error {
	rows, err := s.q.ClaimDueMail(ctx, dbgen.ClaimDueMailParams{
		BatchSize: DrainBatch,
		// The lease. A claimed row is pushed past its own retry delay, so a
		// process killed between claiming and sending leaves a row that comes
		// back on its own rather than one stuck pending forever.
		LeaseSeconds: int32(Backoff(1).Seconds()),
	})
	if err != nil {
		return fmt.Errorf("mail: claim due mail: %w", err)
	}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	// Bounded by construction — the claim returns at most DrainBatch rows — and
	// bounded again here, so that raising the batch size is a decision about how
	// many sessions this opens rather than an accident of one.
	inFlight := make(chan struct{}, SendConcurrency)
	for _, row := range rows {
		inFlight <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-inFlight }()
			if e := s.send(ctx, row); len(e) > 0 {
				mu.Lock()
				errs = append(errs, e...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// send makes one delivery attempt and records what it produced.
//
// Called from SendConcurrency goroutines at once, so it holds nothing across the
// call: the row is its own, `s.q` is a pgxpool the driver already shares, `s.log`
// is a *slog.Logger, and `s.sender` is required to be safe for concurrent use —
// see Sender. Anything added here that touches Service state needs its own guard.
func (s *Service) send(ctx context.Context, row dbgen.ClaimDueMailRow) []error {
	var errs []error

	sendErr := s.sender.Send(ctx, row.Recipient, row.Subject, row.Body)
	if sendErr == nil {
		if err := s.q.MarkMailSent(ctx, row.ID); err != nil {
			errs = append(errs, fmt.Errorf("mail: mark %s sent: %w", row.ID, err))
		}
		// Logged at info on any attempt past the first. "It arrived on the
		// fourth try" is a different operational story from "it arrived",
		// and the row alone does not put it in the log.
		if row.Attempts > 1 {
			s.log.Info("mail delivered after retrying",
				slog.String("kind", row.Kind), slog.Int("attempts", int(row.Attempts)))
		}
		return errs
	}

	errs = append(errs, sendErr)
	if row.Attempts >= MaxAttempts {
		// Terminal, and kept. A row saying what was attempted and why it
		// never arrived is the whole reason this is a table.
		if err := s.q.MarkMailFailed(ctx, dbgen.MarkMailFailedParams{
			ID: row.ID, LastError: sendErr.Error(),
		}); err != nil {
			errs = append(errs, fmt.Errorf("mail: mark %s failed: %w", row.ID, err))
		}
		s.log.Error("mail abandoned after the last attempt",
			slog.String("kind", row.Kind),
			slog.Int("attempts", int(row.Attempts)),
			slog.Any("error", sendErr))
		return errs
	}
	if err := s.q.MarkMailRetry(ctx, dbgen.MarkMailRetryParams{
		ID:             row.ID,
		BackoffSeconds: int32(Backoff(int(row.Attempts)).Seconds()),
		LastError:      sendErr.Error(),
	}); err != nil {
		errs = append(errs, fmt.Errorf("mail: reschedule %s: %w", row.ID, err))
	}
	return errs
}

// PurgeFinished deletes sent and failed rows past the retention window,
// reporting how many went.
func (s *Service) PurgeFinished(ctx context.Context) (int64, error) {
	n, err := s.q.PurgeFinishedMail(ctx, FinishedRetentionDays)
	if err != nil {
		return 0, fmt.Errorf("mail: purge finished mail: %w", err)
	}
	return n, nil
}

// Pending counts what is still queued. Read by tests and by anyone asking
// whether the mailer is keeping up.
func (s *Service) Pending(ctx context.Context) (int64, error) {
	n, err := s.q.CountPendingMail(ctx)
	if err != nil {
		return 0, fmt.Errorf("mail: count pending mail: %w", err)
	}
	return n, nil
}

// Backoff is the delay before attempt n+1, given that n attempts have been
// made. Doubling from BackoffBase, capped at BackoffMax.
//
// **BackoffMax is unreachable at the shipped MaxAttempts**, and that is worth
// stating where somebody reads the constants rather than leaving it to be
// derived. Drain refuses a row at `attempts >= MaxAttempts` before asking for a
// delay, so with five attempts the waits are 1m, 2m, 4m and 8m — fifteen minutes
// end to end. Raising MaxAttempts is what makes the 30m cap do anything; a
// document that read the cap off this function and called the span 1m to 16m was
// wrong for exactly that reason (F45).
//
// No jitter: this is one leader draining one queue on a fixed tick, not N
// clients stampeding a service, so there is nothing to spread out.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := BackoffBase
	for range attempts - 1 {
		d *= 2
		if d >= BackoffMax {
			return BackoffMax
		}
	}
	return d
}
