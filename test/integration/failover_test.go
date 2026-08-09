//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// The failover contract (M56), as claims that can be shown false.
//
// docs/operations.md tells an operator what to do with `/readyz` and what
// happens to in-flight work when a replica dies. Every sentence there that is a
// promise rather than an instruction is asserted here. Writing a contract down
// turns an undocumented behaviour into something somebody will build a load
// balancer against, and a promise that turns out to be false is worse than the
// silence it replaced.

// TestReadinessIsDegradedNotUnavailableWhenOnlyRedisIsGone is the distinction
// the load-balancer contract is built on, and the one an operator gets wrong.
//
// `degraded` is **200**. A replica with no Redis still resolves every link —
// the redirect path falls through to Postgres and meets the uncached target —
// so it is a serving replica and must stay in rotation. An operator who reads
// `degraded` as *remove* takes the entire deployment out during a Redis outage,
// which is precisely the failure the two words exist to prevent, and it is why
// the contract is stated as a rule about status **codes** rather than about the
// word in the body.
//
// Postgres is real here. With it down the answer is `unavailable`, and that
// half is asserted in internal/httpx, where it needs no database at all;
// separating them is what makes this one meaningful — a 200 from a replica
// whose database is also gone would prove nothing.
func TestReadinessIsDegradedNotUnavailableWhenOnlyRedisIsGone(t *testing.T) {
	pool := newDB(t)

	// The discard port. go-redis does not dial until a command is issued, so
	// this costs nothing until the probe runs, and then it is refused at once.
	dead := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = dead.Close() })

	h := &httpx.Health{DB: pool, Redis: dead}

	rr := httptest.NewRecorder()
	h.Ready(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d with Redis unreachable and Postgres up, want 200; "+
			"a 503 here removes every replica from rotation over a cache problem",
			rr.Code)
	}
	rep := decodeReport(t, rr)
	if rep.Status != "degraded" {
		t.Errorf("status = %q, want degraded; the word is what tells an operator "+
			"the 200 is not a clean bill of health", rep.Status)
	}
	if got := rep.Dependencies["redis"]; got != "degraded" {
		t.Errorf("dependencies.redis = %q, want degraded", got)
	}
	if got := rep.Dependencies["postgres"]; got != "ok" {
		t.Fatalf("dependencies.postgres = %q, want ok; this test is not measuring what it thinks", got)
	}
	if rep.Errors["redis"] == "" {
		t.Error("no error text for redis; degraded with nothing to look at gives an operator nowhere to start")
	}
}

func decodeReport(t *testing.T, rr *httptest.ResponseRecorder) httpx.Report {
	t.Helper()
	var rep httpx.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return rep
}

// TestAKilledReplicaSWebhookClaimReturnsAndIsDeliveredOnce is the in-flight
// half of the contract, for webhooks.
//
// A replica dies between claiming a delivery and dialling the receiver. What
// the claim already did is spend the attempt and lease the row forward — one
// statement, before anything leaves the process — so the row is not stuck
// pending on a replica that no longer exists. When the lease expires, whichever
// replica holds the family next claims it and delivers.
//
// The claim is issued through the generated query rather than a copy of the SQL,
// so this test cannot drift from what Drain actually runs; dropping its result
// on the floor is exactly what a killed process leaves behind.
//
// The delivery is at-least-once and the contract says so. This asserts the
// window the lease covers — killed *before* the dial, delivered once. A replica
// killed after the request left the socket and before the row was marked would
// deliver twice, which is why every delivery carries a stable idempotency key in
// its `X-LinkCtrl-Delivery` header and why the receiver, not this product, is
// where that window closes.
func TestAKilledReplicaSWebhookClaimReturnsAndIsDeliveredOnce(t *testing.T) {
	rec := newReceiver(t)
	f := newWebhooks(t, rec.server.Client().Transport)
	hookID, _ := f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)

	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "failover", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatalf("create the link: %v", err)
	}
	if n := f.pendingCount(); n != 1 {
		t.Fatalf("%d pending deliveries after one link write, want 1", n)
	}

	// Replica A claims, and dies. Nothing is sent.
	claimed, err := dbgen.New(f.pool).ClaimDueWebhookDeliveries(t.Context(),
		dbgen.ClaimDueWebhookDeliveriesParams{
			BatchSize:    webhook.DrainBatch,
			LeaseSeconds: int32(webhook.Backoff(1).Seconds()),
		})
	if err != nil {
		t.Fatalf("claim as the replica that is about to die: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the dying replica claimed %d deliveries, want 1", len(claimed))
	}
	if n := len(rec.received()); n != 0 {
		t.Fatalf("the receiver saw %d requests from a replica that never dialled it", n)
	}

	// Replica B, immediately. The lease is what stops it re-claiming a row that
	// might still be in flight somewhere: a claim is not a completion, and the
	// only safe assumption within the lease is that the first replica is alive.
	if err := f.hooks.Drain(t.Context()); err != nil {
		t.Fatalf("drain on the second replica: %v", err)
	}
	if n := len(rec.received()); n != 0 {
		t.Errorf("the second replica delivered %d times inside the first one's lease; "+
			"two live replicas would each send the same event", n)
	}

	// The lease expires. In production that is sixty seconds; here the row is
	// aged rather than waited out, which moves the clock and nothing else — the
	// status is still pending and the attempt is still spent.
	f.due()

	if err := f.hooks.Drain(t.Context()); err != nil {
		t.Fatalf("drain after the lease expired: %v", err)
	}

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("the receiver saw %d requests after one failover, want exactly 1; "+
			"the work was %s", len(got), lostOrDoubled(len(got)))
	}
	if _, err := uuid.Parse(got[0].Delivery); err != nil {
		t.Errorf("%s = %q, want the delivery's own uuid — it is the receiver's "+
			"only defence against the duplicate window this contract admits to",
			webhook.HeaderDelivery, got[0].Delivery)
	}

	status, attempts, _, _ := f.deliveryRow(hookID)
	if status != "delivered" {
		t.Errorf("delivery status = %q after the follower drained it, want delivered", status)
	}
	// Two: the dead replica spent one and the live one spent the other. The
	// attempt is spent at claim time on purpose — counting it at send time would
	// let a replica that dies mid-send retry the same delivery forever.
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one spent by the replica that died, one by the "+
			"replica that delivered); a crash loop has stopped being bounded", attempts)
	}
}

// TestAKilledReplicaSMailClaimReturnsAndIsSentOnce is the same shape for the
// outbox, and it is the same shape on purpose: two queues that fail over
// differently are two things to reason about during an incident.
func TestAKilledReplicaSMailClaimReturnsAndIsSentOnce(t *testing.T) {
	f := newMailFixture(t, true)
	q := dbgen.New(f.pool)

	if err := q.EnqueueMail(t.Context(), dbgen.EnqueueMailParams{
		ID: uuid.Must(uuid.NewV7()), Recipient: "someone@example.com",
		Subject: "queued before the replica died", Body: "body", Kind: "invitation",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Replica A claims, and dies.
	claimed, err := q.ClaimDueMail(t.Context(), dbgen.ClaimDueMailParams{
		BatchSize: mail.DrainBatch, LeaseSeconds: int32(mail.Backoff(1).Seconds()),
	})
	if err != nil {
		t.Fatalf("claim as the replica that is about to die: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the dying replica claimed %d messages, want 1", len(claimed))
	}

	// Replica B, inside the lease: the message is somebody else's until it
	// expires.
	if err := f.mail.Drain(t.Context()); err != nil {
		t.Fatalf("drain on the second replica: %v", err)
	}
	if n := len(f.sender.delivered()); n != 0 {
		t.Errorf("the second replica sent %d messages inside the first one's lease; "+
			"a recipient would have received the invitation twice", n)
	}

	expireMailLease(t, f)

	if err := f.mail.Drain(t.Context()); err != nil {
		t.Fatalf("drain after the lease expired: %v", err)
	}

	sent := f.sender.delivered()
	if len(sent) != 1 {
		t.Fatalf("the relay saw %d messages after one failover, want exactly 1; "+
			"the work was %s", len(sent), lostOrDoubled(len(sent)))
	}

	rows := f.outbox(t)
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows, want 1", len(rows))
	}
	if rows[0].Status != "sent" {
		t.Errorf("outbox status = %q after the follower drained it, want sent", rows[0].Status)
	}
	if rows[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one spent by the replica that died, one by the "+
			"replica that sent it)", rows[0].Attempts)
	}
}

// expireMailLease moves the clock, and nothing else: the row stays pending and
// the attempt stays spent, exactly as it would sixty seconds later.
func expireMailLease(t *testing.T, f *mailFixture) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE mail_outbox SET next_attempt_at = now() - interval '1 second'
		  WHERE status = 'pending'`); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
}

// lostOrDoubled names which way the count went, so a failure reads as the
// property that broke rather than as arithmetic.
func lostOrDoubled(n int) string {
	if n == 0 {
		return "lost with the replica that claimed it"
	}
	return "done more than once"
}

// TestTheClaimLeasesAreLongEnoughToBeLeases guards the number the two tests
// above depend on without stating.
//
// A lease shorter than the work it covers is not a lease: the row becomes
// claimable again while the first replica is still dialling, and every slow
// relay turns into a duplicate. Sixty seconds against a per-message
// SMTP_TIMEOUT of ten is the margin, and it is the first attempt's backoff
// rather than a constant of its own so that a row's own retry schedule and the
// lease cannot drift apart.
func TestTheClaimLeasesAreLongEnoughToBeLeases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lease time.Duration
	}{
		{"mail", mail.Backoff(1)},
		{"webhooks", webhook.Backoff(1)},
	} {
		if tc.lease < 30*time.Second {
			t.Errorf("%s lease is %s; a claim that expires while the first replica is "+
				"still working turns a slow remote into a duplicate", tc.name, tc.lease)
		}
	}
}
