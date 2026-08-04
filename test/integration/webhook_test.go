//go:build integration

package integration

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
)

// Webhooks end to end (M42).
//
// Its own fixture rather than the rule fixture, because the milestone's claims
// are about a queue and a socket rather than about a redirect: nothing here
// touches the redirect tree, and wiring one would be wiring something no
// assertion below reads.
//
// **The delivery half substitutes the transport**, and that is deliberate rather
// than a convenience. The production transport refuses to connect to a loopback
// address, which is the whole of M42's dial-time guard — so a delivery test that
// used it would be a test that could never deliver anything. The guard is
// asserted against the real transport, at unit level, in
// internal/webhook/client_test.go; this file asserts the queue, the signature,
// the retry schedule and the abandonment on top of a receiver it can actually
// reach.

// receiver is a webhook endpoint under a test's control.
type receiver struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []receivedDelivery
	// status is what the next request is answered with.
	status int
}

type receivedDelivery struct {
	Event     string
	Delivery  string
	Timestamp string
	Signature string
	Body      []byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, receivedDelivery{
			Event:     req.Header.Get(webhook.HeaderEvent),
			Delivery:  req.Header.Get(webhook.HeaderDelivery),
			Timestamp: req.Header.Get(webhook.HeaderTimestamp),
			Signature: req.Header.Get(webhook.HeaderSignature),
			Body:      body,
		})
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *receiver) received() []receivedDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]receivedDelivery, len(r.requests))
	copy(out, r.requests)
	return out
}

type webhookFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	links *link.Service
	hooks *webhook.Service
	owner *auth.Identity
	obs   *countingObserver
}

// countingObserver records the metric labels, so the cardinality claim can be
// asserted rather than described.
type countingObserver struct {
	mu     sync.Mutex
	counts map[[2]string]int
}

func (c *countingObserver) ObserveWebhookDelivery(outcome, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[[2]string]int{}
	}
	c.counts[[2]string{outcome, status}]++
}

func (c *countingObserver) get(outcome, status string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[[2]string{outcome, status}]
}

// newWebhooks builds the fixture. transport nil gives the real guarded one,
// which is what the tier tests want; a receiver's transport is what the delivery
// tests pass.
func newWebhooks(t *testing.T, transport webhook.GuardedTransport) *webhookFixture {
	t.Helper()
	pool := newDB(t)

	obs := &countingObserver{}
	hooks := webhook.NewService(pool, webhook.Config{
		Timeout: 2 * time.Second, RetentionDays: 30,
		Transport: transport, Observer: obs, Logger: quietLogger(),
	})
	links := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://links.test",
		Audit: audit.NewService(pool), Events: hooks, Log: quietLogger(),
	})

	authSvc := auth.NewService(pool, auth.ServiceConfig{Params: fastParams})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner",
		Password: webPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}

	return &webhookFixture{t: t, pool: pool, links: links, hooks: hooks, owner: owner, obs: obs}
}

// registerRaw writes a webhook row directly, bypassing registration.
//
// It exists because the product will not let a test construct the state a
// delivery test needs. `httptest` listens on loopback, and a loopback URL is
// exactly what CreateWebhook refuses — which is M42's first security bullet and
// is asserted, against the real service, in TestWebhookURLsGoThroughEveryTier.
// Going around it here is what separates "the registration check works" from
// "the queue, the signature and the retry schedule work"; running the second
// through the first would mean either weakening the check or having no delivery
// test at all.
//
// The returned secret is the raw key, so a test can verify the signature the way
// a receiver would.
func (f *webhookFixture) registerRaw(url string, events []string, enabled bool) (uuid.UUID, []byte) {
	f.t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		f.t.Fatalf("mint a secret: %v", err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := f.pool.Exec(f.t.Context(), `
		INSERT INTO webhooks (id, workspace_id, url, secret, events, description, enabled)
		VALUES ($1, $2, $3, $4, $5::text[], 'delivery test', $6)`,
		id, f.owner.WorkspaceID, url, secret, events, enabled); err != nil {
		f.t.Fatalf("insert the webhook row: %v", err)
	}
	return id, secret
}

func (f *webhookFixture) register(url string, events []string, enabled bool) *domain.Webhook {
	f.t.Helper()
	hook, err := f.links.CreateWebhook(f.t.Context(), f.owner, link.CreateWebhookInput{
		URL: url, Events: events, Description: "test", Enabled: enabled,
	})
	if err != nil {
		f.t.Fatalf("register %s: %v", url, err)
	}
	return hook
}

// due forces every pending delivery to be claimable now, so a test does not wait
// out a backoff it already knows the shape of.
func (f *webhookFixture) due() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 second'
		  WHERE status = 'pending'`); err != nil {
		f.t.Fatalf("make deliveries due: %v", err)
	}
}

func (f *webhookFixture) deliveryRow(id uuid.UUID) (status string, attempts int32, code *int32, lastErr string) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.t.Context(), `
		SELECT d.status, d.attempts, d.response_code, d.last_error
		  FROM webhook_deliveries d WHERE d.webhook_id = $1
		 ORDER BY d.created_at LIMIT 1`, id).Scan(&status, &attempts, &code, &lastErr); err != nil {
		f.t.Fatalf("read the delivery row: %v", err)
	}
	return status, attempts, code, lastErr
}

func (f *webhookFixture) pendingCount() int64 {
	f.t.Helper()
	n, err := f.hooks.Pending(f.t.Context())
	if err != nil {
		f.t.Fatalf("count pending: %v", err)
	}
	return n
}

// --- registration ------------------------------------------------------------

// TestWebhookURLsGoThroughEveryTier is what M42 owes M30 for adding the ninth
// and tenth destination-writing surfaces.
//
// It is also the milestone's first security bullet: *every webhook URL passes
// ValidateDestination with the unappealable tier enforced — a metadata-IP target
// is refused, tested.* The source scan in internal/link/surfaces_test.go fails
// the build if either surface stops going through checkDestination; this is the
// assertion about behaviour rather than about structure.
func TestWebhookURLsGoThroughEveryTier(t *testing.T) {
	f := newWebhooks(t, nil)

	refused := []struct{ name, url string }{
		{"the cloud metadata endpoint", "http://169.254.169.254/latest/meta-data/"},
		{"the metadata endpoint with a trailing dot", "http://169.254.169.254./"},
		{"loopback", "http://127.0.0.1:8080/hook"},
		{"localhost by name", "http://localhost/hook"},
		{"a private address", "http://10.0.0.5/hook"},
		{"link-local IPv6", "http://[fe80::1]/hook"},
		{"an obfuscated decimal address", "http://2130706433/hook"},
		{"an obfuscated hex metadata address", "http://0xa9fea9fe/hook"},
		{"a scheme outside the allowlist", "javascript:alert(1)"},
		{"a host on the seeded blocklist", "https://bit.ly/whatever"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.links.CreateWebhook(t.Context(), f.owner, link.CreateWebhookInput{
				URL: tc.url, Events: []string{domain.EventLinkCreated}, Enabled: true,
			})
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("a webhook pointing at %s was accepted (err=%v). This is the "+
					"SSRF the tiers exist for, and a webhook dials it from the server "+
					"rather than from a visitor's browser.", tc.url, err)
			}
			if ve[0].Field != "url" {
				t.Errorf("the refusal was reported against %q, not the url field", ve[0].Field)
			}
		})
	}

	// And on an edit, which is the tenth surface. A URL that was fine when it
	// was registered must not survive an edit onto a refused one.
	hook := f.register("https://example.com/hook", []string{domain.EventLinkCreated}, true)
	bad := "http://169.254.169.254/"
	_, err := f.links.UpdateWebhook(t.Context(), f.owner, hook.ID,
		link.UpdateWebhookInput{URL: &bad})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Errorf("editing a webhook onto the metadata endpoint was accepted (err=%v)", err)
	}

	// Recorded, with this surface named, exactly as every other destination
	// refusal is.
	var n int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'destination.blocked'
		   AND metadata->>'surface' = 'webhook.url'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no refusal was recorded against the webhook surface")
	}
}

// TestWebhookSecretIsShownOnceAndNeverAgain is the other half of the credential
// promise: the value exists in one response and is not readable afterwards.
func TestWebhookSecretIsShownOnceAndNeverAgain(t *testing.T) {
	f := newWebhooks(t, nil)
	created := f.register("https://example.com/hook", []string{domain.EventLinkCreated}, true)

	if len(created.Secret) != 64 {
		t.Fatalf("the created webhook carried a %d-character secret, want 64 hex characters",
			len(created.Secret))
	}

	got, err := f.links.Webhook(t.Context(), f.owner, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "" {
		t.Error("reading a webhook back returned its signing secret; it is shown once")
	}
	list, err := f.links.Webhooks(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range list {
		if h.Secret != "" {
			t.Error("listing webhooks returned a signing secret")
		}
	}

	// Rotation is the documented way back, and it produces a different value.
	rotated, err := f.links.RotateWebhookSecret(t.Context(), f.owner, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Error("rotation did not produce a new secret")
	}
}

// TestTheEventVocabularyIsClosed is the "deliberately small vocabulary" bullet,
// asserted rather than described.
//
// **The count moved from six to seven in M43, and this test is why that was a
// decision rather than a diff.** D79 said widening the vocabulary is a deliberate
// edit, a line in the docs, and a row in the test that asserts the size; M43
// added `automation.fired` and met all three, this one by failing the integration
// suite until somebody came back and changed the number on purpose. See D83 for
// why an automation may emit only that event and nothing else — a rule that could
// choose would be a rule that could manufacture what another rule triggers on.
func TestTheEventVocabularyIsClosed(t *testing.T) {
	f := newWebhooks(t, nil)

	if len(domain.WebhookEvents) != 7 {
		t.Errorf("the vocabulary holds %d events, want 7 — five for a link's "+
			"lifecycle, one for a refusal, and one for an automation rule that "+
			"fired. Widening it is a deliberate edit with a docs line, not a side "+
			"effect.", len(domain.WebhookEvents))
	}

	_, err := f.links.CreateWebhook(t.Context(), f.owner, link.CreateWebhookInput{
		URL: "https://example.com/hook", Events: []string{"link.exploded"}, Enabled: true,
	})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a subscription to an invented event was accepted (err=%v)", err)
	}
	if ve[0].Code != "unknown_event" {
		t.Errorf("the refusal carried code %q, want unknown_event", ve[0].Code)
	}

	// And an empty subscription, which would be a URL nothing ever calls.
	_, err = f.links.CreateWebhook(t.Context(), f.owner, link.CreateWebhookInput{
		URL: "https://example.com/hook", Events: nil, Enabled: true,
	})
	if !errors.As(err, &ve) {
		t.Errorf("a webhook subscribed to nothing was accepted (err=%v)", err)
	}
}

// TestWebhooksWriteIsNotDelegableToAKey pins D18's second limb as this milestone
// applied it.
//
// auth.NonDelegableScopes is the only thing enforcing it — there is no second
// check in the handler or the service — so this is the test that would fail if
// the map entry were removed, which is exactly the reversal the decision
// describes.
func TestWebhooksWriteIsNotDelegableToAKey(t *testing.T) {
	if _, blocked := auth.NonDelegableScopes[link.PermWebhooksWrite]; !blocked {
		t.Errorf("%s is delegable to an API key. A webhook created with a key keeps "+
			"delivering after the key is revoked, which is reach the credential "+
			"retains once it is gone (M42, D18).", link.PermWebhooksWrite)
	}
	if _, blocked := auth.NonDelegableScopes[link.PermWebhooksRead]; blocked {
		t.Errorf("%s is in NonDelegableScopes; M42 concluded reading stays delegable, "+
			"because watching an integration escalates nothing", link.PermWebhooksRead)
	}
}

// --- the queue ---------------------------------------------------------------

// TestALinkWriteQueuesOneDeliveryPerSubscribedWebhook is the fan-out.
func TestALinkWriteQueuesOneDeliveryPerSubscribedWebhook(t *testing.T) {
	f := newWebhooks(t, nil)

	subscribed := f.register("https://a.example.com/hook", []string{domain.EventLinkCreated}, true)
	f.register("https://b.example.com/hook", []string{domain.EventLinkDeleted}, true)
	paused := f.register("https://c.example.com/hook", []string{domain.EventLinkCreated}, false)

	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "fanned", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatalf("create the link: %v", err)
	}

	rows := map[uuid.UUID]int{}
	got, err := f.pool.Query(t.Context(),
		`SELECT webhook_id, count(*) FROM webhook_deliveries GROUP BY webhook_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	for got.Next() {
		var id uuid.UUID
		var n int
		if err := got.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		rows[id] = n
	}

	if rows[subscribed.ID] != 1 {
		t.Errorf("the subscribed webhook got %d deliveries, want 1", rows[subscribed.ID])
	}
	if rows[paused.ID] != 0 {
		t.Errorf("the paused webhook got %d deliveries, want 0. A disabled webhook "+
			"queues nothing rather than queueing and discarding.", rows[paused.ID])
	}
	if len(rows) != 1 {
		t.Errorf("%d webhooks received a delivery, want 1: a webhook subscribed to a "+
			"different event must not hear about this one", len(rows))
	}
}

// TestABlockedAttemptIsQueuedDefanged is the blocked-attempt half of the
// vocabulary, and the promise that the payload carries no live URL.
func TestABlockedAttemptIsQueuedDefanged(t *testing.T) {
	f := newWebhooks(t, nil)
	f.register("https://a.example.com/hook", []string{domain.EventDestinationBlocked}, true)

	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "refused", URL: "http://169.254.169.254/latest/meta-data/",
	}); err == nil {
		t.Fatal("the metadata endpoint was accepted as a link destination")
	}

	var payload []byte
	if err := f.pool.QueryRow(t.Context(),
		`SELECT payload FROM webhook_deliveries WHERE event = $1`,
		domain.EventDestinationBlocked).Scan(&payload); err != nil {
		t.Fatalf("no blocked-attempt delivery was queued: %v", err)
	}

	var body struct {
		Event string `json:"event"`
		Data  struct {
			Tier        string `json:"tier"`
			Rule        string `json:"rule"`
			Surface     string `json:"surface"`
			URLDefanged string `json:"url_defanged"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("the payload is not the documented shape: %v\n%s", err, payload)
	}
	if body.Event != domain.EventDestinationBlocked {
		t.Errorf("payload event = %q, want %q", body.Event, domain.EventDestinationBlocked)
	}
	if body.Data.Tier != "unappealable" {
		t.Errorf("payload tier = %q, want unappealable", body.Data.Tier)
	}
	if body.Data.Surface != "link.create" {
		t.Errorf("payload surface = %q, want link.create", body.Data.Surface)
	}
	// The whole point: a receiver piping this into a chat room or a console must
	// not be handing somebody a live link to the thing that was refused.
	if got := body.Data.URLDefanged; !contains(got, "[:]") || contains(got, "://") {
		t.Errorf("the attempted URL was not defanged: %q", got)
	}
}

// --- delivery ----------------------------------------------------------------

// TestDrainDeliversASignedPayload is the happy path, end to end through the
// queue and the client.
func TestDrainDeliversASignedPayload(t *testing.T) {
	rec := newReceiver(t)
	f := newWebhooks(t, rec.server.Client().Transport)

	hookID, secret := f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)

	created, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "delivered", URL: "https://example.com/dest",
	})
	if err != nil {
		t.Fatalf("create the link: %v", err)
	}
	if n := f.pendingCount(); n != 1 {
		t.Fatalf("%d pending deliveries after one link write, want 1", n)
	}

	if err := f.hooks.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("the receiver saw %d requests, want 1", len(got))
	}
	if got[0].Event != domain.EventLinkCreated {
		t.Errorf("%s = %q, want %q", webhook.HeaderEvent, got[0].Event, domain.EventLinkCreated)
	}
	if _, err := uuid.Parse(got[0].Delivery); err != nil {
		t.Errorf("%s = %q, want a uuid — it is the receiver's idempotency key",
			webhook.HeaderDelivery, got[0].Delivery)
	}

	// The signature, verified the way docs/usage.md tells a receiver to verify
	// it: the key is the secret string as it was shown, the message is the
	// timestamp, a dot, and the body exactly as it arrived.
	//
	// Written out here rather than calling webhook.Sign, because a test that
	// compared Sign against Sign would pass whatever the scheme was. This one
	// fails if the key encoding, the separator or the signed message changes,
	// which is the whole set of things a published signature must not change
	// quietly.
	mac := hmac.New(sha256.New, []byte(hex.EncodeToString(secret)))
	mac.Write([]byte(got[0].Timestamp))
	mac.Write([]byte{'.'})
	mac.Write(got[0].Body)
	want := webhook.SignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(got[0].Signature), []byte(want)) {
		t.Errorf("%s = %q, and a receiver following docs/usage.md computed %q",
			webhook.HeaderSignature, got[0].Signature, want)
	}

	var body struct {
		Event       string `json:"event"`
		WorkspaceID string `json:"workspace_id"`
		Data        struct {
			ID       string `json:"id"`
			Alias    string `json:"alias"`
			ShortURL string `json:"short_url"`
			URL      string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].Body, &body); err != nil {
		t.Fatalf("the delivered body is not the documented shape: %v\n%s", err, got[0].Body)
	}
	if body.Data.Alias != "delivered" || body.Data.URL != "https://example.com/dest" {
		t.Errorf("the payload does not describe the link that was created: %+v", body.Data)
	}
	if body.Data.ID != created.ID.String() {
		t.Errorf("payload id = %q, want %q", body.Data.ID, created.ID)
	}
	if body.WorkspaceID != f.owner.WorkspaceID.String() {
		t.Errorf("payload workspace_id = %q, want %q", body.WorkspaceID, f.owner.WorkspaceID)
	}

	// The row records the outcome, which is what an operator reads.
	status, attempts, code, _ := f.deliveryRow(hookID)
	if status != domain.DeliveryDelivered {
		t.Errorf("delivery status = %q, want %q", status, domain.DeliveryDelivered)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if code == nil || *code != http.StatusOK {
		t.Errorf("response_code = %v, want 200", code)
	}
	if n := f.pendingCount(); n != 0 {
		t.Errorf("%d deliveries still pending after a successful drain, want 0", n)
	}
	if f.obs.get("delivered", "2xx") != 1 {
		t.Error("no delivered/2xx observation was counted")
	}
}

// TestDrainRetriesAndAbandonsAfterTheDocumentedAttemptCount is the retry policy
// and the abandonment bullet.
//
// The receiver answers 500 throughout, so every attempt fails for a reason the
// row can record — which is what makes the response code assertion meaningful.
func TestDrainRetriesAndAbandonsAfterTheDocumentedAttemptCount(t *testing.T) {
	rec := newReceiver(t)
	rec.answer(http.StatusInternalServerError)
	f := newWebhooks(t, rec.server.Client().Transport)

	hookID, _ := f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "doomed", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatalf("create the link: %v", err)
	}

	for attempt := 1; attempt <= webhook.MaxAttempts; attempt++ {
		f.due()
		// A failing drain returns the receiver's refusal, which is expected here.
		_ = f.hooks.Drain(t.Context())

		status, attempts, code, lastErr := f.deliveryRow(hookID)
		if attempts != int32(attempt) {
			t.Fatalf("after drain %d the row records %d attempts", attempt, attempts)
		}
		if code == nil || *code != http.StatusInternalServerError {
			t.Errorf("after drain %d the response code is %v, want 500", attempt, code)
		}
		if lastErr == "" {
			t.Errorf("after drain %d the row records no error", attempt)
		}
		if attempt < webhook.MaxAttempts && status != domain.DeliveryPending {
			t.Fatalf("after drain %d the delivery is %q, want pending — abandonment "+
				"before the documented attempt count loses events a receiver would "+
				"have accepted on its next deploy", attempt, status)
		}
		if attempt == webhook.MaxAttempts && status != domain.DeliveryAbandoned {
			t.Fatalf("after %d attempts the delivery is %q, want abandoned — a queue "+
				"that retries forever dials a dead endpoint on every tick",
				webhook.MaxAttempts, status)
		}
	}

	if len(rec.received()) != webhook.MaxAttempts {
		t.Errorf("the receiver saw %d attempts, want %d",
			len(rec.received()), webhook.MaxAttempts)
	}
	if n := f.pendingCount(); n != 0 {
		t.Errorf("%d deliveries still pending after abandonment, want 0", n)
	}
	if f.obs.get("abandoned", "5xx") != 1 {
		t.Error("no abandoned/5xx observation was counted")
	}
	if f.obs.get("retry", "5xx") != webhook.MaxAttempts-1 {
		t.Errorf("retry/5xx counted %d, want %d",
			f.obs.get("retry", "5xx"), webhook.MaxAttempts-1)
	}
}

// TestAClaimedDeliveryIsLeasedForward is the crash-safety property the claim
// query is written for: the attempt is spent and the row is pushed forward
// before anything is sent.
func TestAClaimedDeliveryIsLeasedForward(t *testing.T) {
	rec := newReceiver(t)
	rec.answer(http.StatusInternalServerError)
	f := newWebhooks(t, rec.server.Client().Transport)

	hookID, _ := f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "leased", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatalf("create the link: %v", err)
	}

	_ = f.hooks.Drain(t.Context())
	if _, attempts, _, _ := f.deliveryRow(hookID); attempts != 1 {
		t.Fatalf("attempts = %d after one drain, want 1", attempts)
	}

	// A second drain immediately after finds nothing due: the backoff is a
	// minute, so the row is not claimable again yet.
	before := len(rec.received())
	_ = f.hooks.Drain(t.Context())
	if after := len(rec.received()); after != before {
		t.Errorf("a second drain re-delivered immediately (%d then %d requests); the "+
			"backoff is what keeps a failing receiver from being dialled every tick",
			before, after)
	}
}

// barrier is a receiver that holds every request until `want` of them are in
// flight together, and records the high-water mark.
//
// It exists because *the batch went out at once* is a claim about overlap rather
// than about duration. A stopwatch alone would pass on a machine fast enough to
// serialize twenty round trips inside whatever threshold the test picked, and
// would fail on a loaded one for reasons that have nothing to do with the code.
// The high-water mark cannot be reached at all without concurrency.
//
// The grace is the fallback a serialized drain hits instead of the barrier, so a
// run against sequential delivery finishes in DrainBatch × grace rather than
// hanging until the client's own timeout.
type barrier struct {
	server *httptest.Server
	want   int
	grace  time.Duration

	mu       sync.Mutex
	inFlight int
	high     int
	full     chan struct{}
	filled   bool
}

func newBarrier(t *testing.T, want int, grace time.Duration) *barrier {
	t.Helper()
	b := &barrier{want: want, grace: grace, full: make(chan struct{})}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b.mu.Lock()
		b.inFlight++
		b.high = max(b.high, b.inFlight)
		if b.inFlight >= b.want && !b.filled {
			b.filled = true
			close(b.full)
		}
		b.mu.Unlock()

		select {
		case <-b.full:
		case <-time.After(b.grace):
		case <-req.Context().Done():
		}

		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *barrier) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.high
}

// TestOneDrainSpendsOneAttemptRatherThanTwenty is M42's reopening.
//
// The shipped drain delivered a claimed batch one row after another on the
// scheduler's single goroutine, so at the default WEBHOOK_TIMEOUT one workspace
// pointing a webhook at a public address that drops SYNs held every other job on
// the instance — mail, automation, the rollups, domain verification — for
// DrainBatch × WEBHOOK_TIMEOUT. Go tickers hold a cap-one channel, so those ticks
// were dropped rather than queued.
//
// What is asserted is the claim the comment above DrainBatch makes: one drain
// costs one attempt, whatever the batch size is.
func TestOneDrainSpendsOneAttemptRatherThanTwenty(t *testing.T) {
	rec := newBarrier(t, webhook.DrainBatch, 400*time.Millisecond)
	f := newWebhooks(t, rec.server.Client().Transport)

	_, _ = f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)
	for i := range webhook.DrainBatch {
		if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
			Alias: fmt.Sprintf("backlog-%02d", i), URL: "https://example.com/dest",
		}); err != nil {
			t.Fatalf("create link %d: %v", i, err)
		}
	}
	if n := f.pendingCount(); n != webhook.DrainBatch {
		t.Fatalf("%d pending deliveries, want %d — the batch under test is one full "+
			"claim", n, webhook.DrainBatch)
	}

	start := time.Now()
	if err := f.hooks.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	elapsed := time.Since(start)

	if peak := rec.peak(); peak != webhook.DrainBatch {
		t.Errorf("at most %d deliveries were in flight at once, want %d — a batch "+
			"delivered one row at a time holds the scheduler goroutine for "+
			"DrainBatch × WEBHOOK_TIMEOUT, and every other job's tick is dropped "+
			"for the duration", peak, webhook.DrainBatch)
	}
	// The consequence, as arithmetic rather than as a feeling about speed:
	// delivering these rows one after another cannot finish inside half the time
	// the sequential path is obliged to spend.
	if limit := time.Duration(webhook.DrainBatch/2) * rec.grace; elapsed >= limit {
		t.Errorf("the drain took %s; %d rows delivered one at a time take at least "+
			"%s, and delivered together take about %s",
			elapsed, webhook.DrainBatch,
			time.Duration(webhook.DrainBatch)*rec.grace, rec.grace)
	}

	// Concurrency must not have cost the bookkeeping. Every row is marked, once.
	if n := f.pendingCount(); n != 0 {
		t.Errorf("%d deliveries still pending after the drain, want 0", n)
	}
	if got := f.obs.get("delivered", "2xx"); got != webhook.DrainBatch {
		t.Errorf("delivered/2xx counted %d, want %d", got, webhook.DrainBatch)
	}
}

// TestDeliveriesArePrunedByAge is the retention bullet.
func TestDeliveriesArePrunedByAge(t *testing.T) {
	rec := newReceiver(t)
	f := newWebhooks(t, rec.server.Client().Transport)

	_, _ = f.registerRaw(rec.server.URL, []string{domain.EventLinkCreated}, true)
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "aged", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatalf("create the link: %v", err)
	}
	if err := f.hooks.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Not yet: a finished row inside the window stays.
	n, err := f.hooks.PurgeFinished(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("purged %d rows inside the retention window, want 0", n)
	}

	// Aged past it, and it goes.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE webhook_deliveries SET created_at = now() - interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	n, err = f.hooks.PurgeFinished(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d rows past the window, want 1. The delivery log is a "+
			"record of what was attempted, not an archive.", n)
	}

	// A pending row is never purged, whatever its age: it is still work.
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "still-queued", URL: "https://example.com/dest2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE webhook_deliveries SET created_at = now() - interval '90 days'
		  WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	if n, err = f.hooks.PurgeFinished(t.Context()); err != nil || n != 0 {
		t.Errorf("purged %d pending rows (err=%v), want 0", n, err)
	}
}

// TestDeletingAWebhookTakesItsDeliveryLog is the cascade 00600 declared,
// asserted because the reset in the demo seeder and the API's promise both rest
// on it.
func TestDeletingAWebhookTakesItsDeliveryLog(t *testing.T) {
	f := newWebhooks(t, nil)
	hook := f.register("https://a.example.com/hook", []string{domain.EventLinkCreated}, true)
	if _, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "cascade", URL: "https://example.com/dest",
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.pendingCount(); n != 1 {
		t.Fatalf("%d pending deliveries, want 1", n)
	}
	if err := f.links.DeleteWebhook(t.Context(), f.owner, hook.ID); err != nil {
		t.Fatalf("delete the webhook: %v", err)
	}
	if n := f.pendingCount(); n != 0 {
		t.Errorf("%d deliveries survived their webhook, want 0", n)
	}
}
