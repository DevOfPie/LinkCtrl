// Package webhook queues, signs and delivers outbound event notifications.
//
// Nothing is delivered on the request path. A consumer calls Emit, which renders
// one payload and fans it out to every subscribed webhook in the workspace with
// a single INSERT ... SELECT; the scheduler drains that table on its own clock.
// The table is the reason for the split, and it is the same reason the mail
// outbox exists (D23): a delivery that vanished because a deploy landed mid-retry
// would be invisible on both ends — nobody receives it, and nobody knows one was
// attempted.
//
// **The queue is Postgres and Redis stays a cache.** Plan.md lists Redis Streams
// as an upgrade path for exactly this work. It is not taken, and this package is
// where that is settled: `webhook_deliveries` has shipped since 00600 with the
// shape a queue needs, the mail outbox already proved the claim-and-lease pattern
// on it, and a queue that lives in the cache is a queue that disappears when
// somebody flushes the cache. The upgrade path remains *unexercised* rather than
// adopted — nothing in the tree is written against Streams, and nothing here
// would have to be undone to move later.
//
// **What makes this different from every other outbound call in the product** is
// that the target is chosen by whoever holds a workspace rather than by the
// operator. internal/feed dials one URL an operator named in configuration, and
// it refuses to follow redirects because *"a feed that answers 302 is a feed
// pointing this process somewhere nobody configured."* That sentence applies here
// with more force and less trust, which is why this package has a dialer of its
// own — see client.go, and decisions.md for why the two clients are not shared.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Retry policy. Bounded, and bounded on purpose: a receiver that has refused
// seven deliveries over an hour is not going to accept the eighth, and a queue
// that retries forever is one where a single dead endpoint is dialled on every
// tick until somebody notices.
const (
	// MaxAttempts is how many deliveries one event gets before it is abandoned.
	// Counted at claim time, so a process that dies mid-delivery spends an
	// attempt — otherwise a crash loop would retry the same event forever.
	//
	// Seven rather than the mailer's five, and the extra two are the hour
	// boundary: with the backoff below, seven attempts span 61 minutes, which is
	// long enough to ride out a receiver's deploy and short enough that "we were
	// down this morning, did we lose events" has the answer "anything older than
	// an hour, yes" rather than a calculation.
	//
	// TestBackoffDoublesAndCaps holds the arithmetic to that sentence. It is
	// there because the first draft of this comment said six attempts and an
	// hour, and six is thirty-one minutes.
	MaxAttempts = 7
	// BackoffBase is the delay before the second attempt, doubling up to
	// BackoffMax. With these values the seven attempts span 61 minutes:
	// 1m, 2m, 4m, 8m, 16m, 30m.
	BackoffBase = time.Minute
	BackoffMax  = 30 * time.Minute

	// DrainBatch bounds one drain. Each row is a network round trip to somebody
	// else's server and the scheduler runs every half minute, so a backlog
	// drains over several runs instead of holding the job for minutes.
	DrainBatch = 20

	// DefaultRetentionDays is how long a delivered or abandoned row is kept when
	// the operator has not said. The setting is WEBHOOK_RETENTION_DAYS; this is
	// the fallback, not a second policy.
	DefaultRetentionDays = 30
)

// Emitter is the writing half, as a consumer sees it.
//
// internal/link holds this rather than *Service, so "no webhook delivery in this
// process" is a nil interface rather than a flag every call site has to remember
// to check — and so internal/link's tests need neither this package nor a
// delivery client. It is also what keeps the import graph one-way: internal/link
// never imports internal/webhook, and this package never imports internal/link
// except for the one address predicate in client.go.
type Emitter interface {
	Emit(ctx context.Context, workspaceID uuid.UUID, event string, data map[string]any)
}

// Observer counts delivery outcomes. Nil counts nothing.
//
// The label vocabulary is fixed by the implementation, not assembled here: M13's
// cardinality rule is that a bounded label is fine and an unbounded one is not,
// so an outcome and an HTTP status *class* are counted and a URL never is.
type Observer interface {
	ObserveWebhookDelivery(outcome, status string)
}

// Service is the outbox: Emit on one side, Drain on the other.
type Service struct {
	q      *dbgen.Queries
	client *Client
	log    *slog.Logger
	obs    Observer
	// retentionDays is the operator's window for finished rows.
	retentionDays int
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree: the package that does the work does
// not read the environment.
type Config struct {
	// Timeout bounds one delivery attempt end to end — connect, write, read.
	// Zero takes DefaultTimeout.
	Timeout time.Duration
	// RetentionDays is how long a finished delivery is kept. Zero takes
	// DefaultRetentionDays; it is never "forever", because a table with one row
	// per link write per webhook and no window is the growth problem D5 exists
	// to stop repeating.
	RetentionDays int
	// Transport is for tests. Nil builds the guarded transport in client.go,
	// which is the only one production ever has.
	Transport GuardedTransport
	Logger    *slog.Logger
	Observer  Observer
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	retention := cfg.RetentionDays
	if retention <= 0 {
		retention = DefaultRetentionDays
	}
	return &Service{
		q:             dbgen.New(pool),
		client:        NewClient(cfg.Timeout, cfg.Transport),
		log:           log,
		obs:           cfg.Observer,
		retentionDays: retention,
	}
}

// Emit queues one event for every subscribed webhook in the workspace.
//
// It runs inside a link write, so its cost where nobody has registered anything —
// which is every workspace on a default instance — is one indexed lookup that
// returns no rows. That is what the partial index `webhooks_workspace_idx ...
// WHERE enabled` (00600) makes it.
//
// **It returns nothing, deliberately.** A caller that could fail because a
// notification could not be queued would be a caller whose link creation fails
// when the webhook table is unhappy. The event is a consequence of a change that
// has already been committed; losing the notification is worse than losing the
// change only if you think a webhook is the product. Logged at warn so the gap is
// visible to whoever goes looking, which is the same trade the audit writer makes.
func (s *Service) Emit(ctx context.Context, workspaceID uuid.UUID, event string, data map[string]any) {
	if s == nil {
		return
	}
	if !domain.IsWebhookEvent(event) {
		// A caller naming an event outside the vocabulary is a bug, not an
		// operational condition: the fan-out query would match no subscription
		// and the event would vanish silently.
		s.log.Error("refusing to emit an event outside the webhook vocabulary",
			slog.String("event", event))
		return
	}

	payload, err := json.Marshal(map[string]any{
		"event":        event,
		"occurred_at":  time.Now().UTC().Format(time.RFC3339),
		"workspace_id": workspaceID,
		"data":         data,
	})
	if err != nil {
		s.log.Warn("webhook event could not be encoded",
			slog.String("event", event), slog.Any("error", err))
		return
	}

	if _, err := s.q.EnqueueWebhookDeliveries(ctx, dbgen.EnqueueWebhookDeliveriesParams{
		Event: event, Payload: payload, WorkspaceID: workspaceID,
	}); err != nil {
		s.log.Warn("webhook event was not queued",
			slog.String("event", event), slog.Any("error", err))
	}
}

// Drain delivers everything due, and is what the scheduler calls.
//
// One delivery's failure never stops the batch: errors are collected and the
// remaining rows are still attempted, because one dead receiver must not hold up
// everybody else's events.
func (s *Service) Drain(ctx context.Context) error {
	rows, err := s.q.ClaimDueWebhookDeliveries(ctx, dbgen.ClaimDueWebhookDeliveriesParams{
		BatchSize: DrainBatch,
		// The lease. A claimed row is pushed past its own retry delay, so a
		// process killed between claiming and delivering leaves a row that comes
		// back on its own rather than one stuck pending forever.
		//nolint:gosec // G115: Backoff(1) is a constant minute.
		LeaseSeconds: int32(Backoff(1).Seconds()),
	})
	if err != nil {
		return fmt.Errorf("webhook: claim due deliveries: %w", err)
	}

	var errs []error
	for _, row := range rows {
		errs = append(errs, s.deliver(ctx, row)...)
	}
	return errors.Join(errs...)
}

// deliver makes one attempt and records what it produced.
func (s *Service) deliver(ctx context.Context, row dbgen.ClaimDueWebhookDeliveriesRow) []error {
	code, sendErr := s.client.Deliver(ctx, row.Url, row.Secret, Delivery{
		ID: row.ID, Event: row.Event, Payload: row.Payload,
	})

	var responseCode *int32
	if code > 0 {
		//nolint:gosec // G115: an HTTP status code is three digits.
		c := int32(code)
		responseCode = &c
	}

	if sendErr == nil {
		s.observe("delivered", code)
		if err := s.q.MarkWebhookDelivered(ctx, dbgen.MarkWebhookDeliveredParams{
			ID: row.ID, ResponseCode: responseCode,
		}); err != nil {
			return []error{fmt.Errorf("webhook: mark %s delivered: %w", row.ID, err)}
		}
		return nil
	}

	// **The URL is not logged, on any path.** A transport error carries it
	// inside the message, and this log is shared by every tenant on the
	// instance; the same shape is an open finding against the reputation feed
	// (F34). The delivery row carries the message, and that row belongs to the
	// one workspace that registered the URL in it.
	if row.Attempts >= MaxAttempts {
		s.observe("abandoned", code)
		s.log.Warn("webhook delivery abandoned after the last attempt",
			slog.String("event", row.Event),
			slog.String("webhook_id", row.WebhookID.String()),
			slog.Int("attempts", int(row.Attempts)),
			slog.Int("response_code", code))
		if err := s.q.MarkWebhookAbandoned(ctx, dbgen.MarkWebhookAbandonedParams{
			ID: row.ID, ResponseCode: responseCode, LastError: truncateError(sendErr),
		}); err != nil {
			return []error{fmt.Errorf("webhook: mark %s abandoned: %w", row.ID, err)}
		}
		return nil
	}

	s.observe("retry", code)
	if err := s.q.MarkWebhookRetry(ctx, dbgen.MarkWebhookRetryParams{
		ID: row.ID,
		//nolint:gosec // G115: Backoff is capped at BackoffMax.
		BackoffSeconds: int32(Backoff(int(row.Attempts)).Seconds()),
		ResponseCode:   responseCode,
		LastError:      truncateError(sendErr),
	}); err != nil {
		return []error{fmt.Errorf("webhook: reschedule %s: %w", row.ID, err)}
	}
	return nil
}

// observe counts one outcome, by status class rather than by code.
//
// M13's cardinality rule is what picks the labels: an outcome is one of four
// words and a status class is one of five, so the whole metric is bounded at
// twenty series however many webhooks exist. A URL label would be one series per
// registration, chosen by users, which is the unbounded shape that rule exists
// to refuse.
func (s *Service) observe(outcome string, code int) {
	if s.obs == nil {
		return
	}
	s.obs.ObserveWebhookDelivery(outcome, statusClass(code))
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		// No response at all: a refused connection, a timeout, or this instance
		// declining to open the socket. The most interesting bucket, and the one
		// a code label alone would have hidden as a zero.
		return "none"
	}
}

// maxStoredError bounds what a delivery row keeps. A message is evidence, not an
// archive, and an error from a hostile receiver is attacker-influenced text.
const maxStoredError = 500

func truncateError(err error) string {
	s := err.Error()
	if len(s) > maxStoredError {
		return s[:maxStoredError]
	}
	return s
}

// PurgeFinished deletes delivered and abandoned rows past the retention window,
// reporting how many went.
func (s *Service) PurgeFinished(ctx context.Context) (int64, error) {
	//nolint:gosec // G115: retentionDays comes from config validation, which bounds it.
	n, err := s.q.PurgeFinishedWebhookDeliveries(ctx, int32(s.retentionDays))
	if err != nil {
		return 0, fmt.Errorf("webhook: purge finished deliveries: %w", err)
	}
	return n, nil
}

// Pending counts what is still queued, for tests and for anyone asking whether
// delivery is keeping up.
func (s *Service) Pending(ctx context.Context) (int64, error) {
	n, err := s.q.CountPendingWebhookDeliveries(ctx)
	if err != nil {
		return 0, fmt.Errorf("webhook: count pending deliveries: %w", err)
	}
	return n, nil
}

// RetentionDays is the configured window, for the docs endpoint and the page.
func (s *Service) RetentionDays() int { return s.retentionDays }

// Backoff is the delay before attempt n+1, given that n attempts have been made.
// Doubling from BackoffBase, capped at BackoffMax.
//
// No jitter: this is one leader draining one queue on a fixed tick, not N clients
// stampeding a service, so there is nothing to spread out. The same reasoning
// mail.Backoff records, and the same shape, because two spellings of one policy
// is one more thing to learn.
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
