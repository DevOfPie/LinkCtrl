package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The tests for M42's security bullets.
//
// Two of them are about a refusal, and a refusal is the hardest thing to test
// honestly: a test that asserts "connecting to 127.0.0.1 failed" passes just as
// well when the guard is deleted and the port is simply closed. Every refusal
// below therefore asserts the *specific* error, and the guard tests run against
// a listener that is genuinely accepting connections, so removing the check
// turns them green-to-red rather than red-to-red.

// TestGuardDialRefusesEveryRestrictedRange is the unit-level half: the Control
// hook itself, against the shapes an attacker reaches for.
func TestGuardDialRefusesEveryRestrictedRange(t *testing.T) {
	refused := []struct {
		name, address string
	}{
		{"the cloud metadata endpoint", "169.254.169.254:80"},
		{"loopback", "127.0.0.1:8080"},
		{"loopback, a different octet", "127.9.9.9:80"},
		{"private, 10/8", "10.0.0.5:443"},
		{"private, 172.16/12", "172.16.3.4:443"},
		{"private, 192.168/16", "192.168.1.1:443"},
		{"carrier-grade NAT", "100.64.0.1:443"},
		{"link-local", "169.254.1.1:443"},
		{"unspecified", "0.0.0.0:80"},
		{"IPv6 loopback", "[::1]:8080"},
		{"IPv6 unique-local", "[fd00::1]:443"},
		{"IPv6 link-local", "[fe80::1]:443"},
		{"IPv4-mapped IPv6 loopback", "[::ffff:127.0.0.1]:80"},
		{"IPv4-mapped IPv6 metadata", "[::ffff:169.254.169.254]:80"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := guardDial("tcp", tc.address, syscall.RawConn(nil))
			if !errors.Is(err, ErrPrivateAddress) {
				t.Fatalf("guardDial(%q) = %v, want ErrPrivateAddress. A webhook "+
					"delivery reaching this address is the SSRF this milestone "+
					"exists to prevent.", tc.address, err)
			}
		})
	}

	allowed := []string{"93.184.216.34:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"}
	for _, address := range allowed {
		if err := guardDial("tcp", address, syscall.RawConn(nil)); err != nil {
			t.Errorf("guardDial(%q) = %v, want nil. A guard that refuses public "+
				"addresses refuses every webhook.", address, err)
		}
	}

	// A network an HTTP transport should never ask for, and an address that is
	// not a resolved literal. Both fail closed, because the safe answer to a
	// state nobody described is no.
	if err := guardDial("unix", "/tmp/x:0", syscall.RawConn(nil)); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("guardDial on a unix socket = %v, want ErrPrivateAddress", err)
	}
	if err := guardDial("tcp", "not-an-address:80", syscall.RawConn(nil)); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("guardDial on an unresolved host = %v, want ErrPrivateAddress", err)
	}
}

// TestDeliveryRefusesAPrivateAddressAtDialTime is the claim m42.md makes, run
// end to end through the real client.
//
// **The receiver in this test is up and answering.** That is the whole design of
// it: a delivery to a loopback listener that is genuinely accepting connections
// succeeds the moment the guard is removed, so this test cannot pass for the
// wrong reason. It also stands in for the rebinding case exactly — a name that
// resolved publicly at registration and resolves to 127.0.0.1 now is
// indistinguishable, at the socket, from this.
func TestDeliveryRefusesAPrivateAddressAtDialTime(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(2*time.Second, nil)
	code, err := client.Deliver(context.Background(), srv.URL, []byte("secret"), Delivery{
		ID: uuid.Must(uuid.NewV7()), Event: "link.created", Payload: []byte(`{}`),
	})

	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("Deliver to %s = (%d, %v), want ErrPrivateAddress. The receiver "+
			"in this test is up and answering, so a nil error here means the "+
			"dial-time check is gone.", srv.URL, code, err)
	}
	if reached {
		t.Fatal("the loopback receiver was reached; the connection was opened before " +
			"anything checked the address")
	}
	if code != 0 {
		t.Errorf("response code = %d, want 0; nothing answered because nothing was dialled", code)
	}
}

// TestDeliveryFollowsNoRedirect asserts the posture, not merely its private-address
// half.
//
// m42.md requires that no redirect to a private address is followed. This client
// follows none at all, which is strictly stronger and is the same stance
// internal/feed takes for the same stated reason. The 3xx is recorded rather
// than chased, so whoever owns the receiver can see what happened.
func TestDeliveryFollowsNoRedirect(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(2*time.Second, srv.Client().Transport)
	code, err := client.Deliver(context.Background(), srv.URL+"/hook", []byte("secret"), Delivery{
		ID: uuid.Must(uuid.NewV7()), Event: "link.created", Payload: []byte(`{}`),
	})

	if code != http.StatusFound {
		t.Fatalf("response code = %d, want %d; the redirect itself is what gets "+
			"recorded", code, http.StatusFound)
	}
	if err == nil {
		t.Fatal("a 302 was treated as a delivery. A receiver that answers 302 is " +
			"pointing this process at a URL nobody registered.")
	}
	if hops != 1 {
		t.Fatalf("the receiver was called %d times, want 1; the redirect was followed", hops)
	}
}

// TestDeliverySignsWhatAReceiverCanVerify reproduces the documented scheme from
// the outside, using nothing from this package but the constants a receiver
// would read out of the docs.
//
// Written as a receiver rather than as a call to Sign, deliberately. A test that
// compared Sign against Sign would pass whatever the scheme was; this one fails
// if the key encoding, the separator, the header names or the signed message
// change, which is exactly the set of things a published signature must not
// change silently.
func TestDeliverySignsWhatAReceiverCanVerify(t *testing.T) {
	secret := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04}
	payload := []byte(`{"event":"link.created","data":{"alias":"launch"}}`)
	deliveryID := uuid.Must(uuid.NewV7())

	var (
		gotBody      string
		gotSignature string
		gotTimestamp string
		gotEvent     string
		gotDelivery  string
		gotType      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotSignature = r.Header.Get(HeaderSignature)
		gotTimestamp = r.Header.Get(HeaderTimestamp)
		gotEvent = r.Header.Get(HeaderEvent)
		gotDelivery = r.Header.Get(HeaderDelivery)
		gotType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := NewClient(2*time.Second, srv.Client().Transport)
	code, err := client.Deliver(context.Background(), srv.URL, secret, Delivery{
		ID: deliveryID, Event: "link.created", Payload: payload,
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", code, http.StatusAccepted)
	}

	if gotBody != string(payload) {
		t.Errorf("body = %q, want the payload byte for byte: %q", gotBody, payload)
	}
	if gotEvent != "link.created" {
		t.Errorf("%s = %q, want the event name", HeaderEvent, gotEvent)
	}
	if gotDelivery != deliveryID.String() {
		t.Errorf("%s = %q, want the delivery id — it is the receiver's idempotency key",
			HeaderDelivery, gotDelivery)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}

	// The receiver's half, written the way the docs describe it.
	version, digest, found := strings.Cut(gotSignature, "=")
	if !found || version != SignatureVersion {
		t.Fatalf("%s = %q, want %q= followed by a hex digest",
			HeaderSignature, gotSignature, SignatureVersion)
	}
	// The key is the secret string as displayed — hex — not the raw bytes.
	mac := hmac.New(sha256.New, []byte(hex.EncodeToString(secret)))
	mac.Write([]byte(gotTimestamp))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(digest), []byte(want)) {
		t.Fatalf("a receiver following docs/usage.md computed %s and the header "+
			"carried %s. The signature scheme is a published interface.", want, digest)
	}

	// The timestamp is a plain unix second count and is recent, so a receiver
	// can use it for replay protection.
	ts, perr := strconv.ParseInt(gotTimestamp, 10, 64)
	if perr != nil {
		t.Fatalf("%s = %q, want unix seconds: %v", HeaderTimestamp, gotTimestamp, perr)
	}
	if age := time.Since(time.Unix(ts, 0)); age > time.Minute || age < -time.Minute {
		t.Errorf("%s is %s away from now; a receiver rejecting stale deliveries "+
			"would reject every one of them", HeaderTimestamp, age)
	}
}

// TestSignChangesWithEveryInput is the cheap half: nothing that a receiver
// distinguishes may collide.
func TestSignChangesWithEveryInput(t *testing.T) {
	base := Sign([]byte("k"), 1000, []byte(`{"a":1}`))
	cases := map[string]string{
		"a different secret":  Sign([]byte("j"), 1000, []byte(`{"a":1}`)),
		"a different time":    Sign([]byte("k"), 1001, []byte(`{"a":1}`)),
		"a different payload": Sign([]byte("k"), 1000, []byte(`{"a":2}`)),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s produced the same signature; a receiver cannot tell them apart", name)
		}
	}
	if len(base) != sha256.Size*2 {
		t.Errorf("signature is %d characters, want %d hex characters of SHA-256",
			len(base), sha256.Size*2)
	}
}

// TestTransportErrorKeepsTheURLOutOfTheMessage is the F34-shaped half.
//
// A `*url.Error` prints as `Post "https://host/path": ...`, which puts the
// registered URL into every string that reaches a log line. The delivery row
// belongs to the workspace that registered the URL, so this is defence in depth
// rather than the whole defence — but repeating a shape that is already an open
// finding against the reputation feed would be strange.
func TestTransportErrorKeepsTheURLOutOfTheMessage(t *testing.T) {
	// A port nothing is listening on, on a public-looking address the guard
	// allows through, so the failure is a genuine transport failure.
	const target = "https://8.8.8.8:1/hooks/secret-path-nobody-should-see"
	client := NewClient(200*time.Millisecond, nil)
	_, err := client.Deliver(context.Background(), target, []byte("secret"), Delivery{
		ID: uuid.Must(uuid.NewV7()), Event: "link.created", Payload: []byte(`{}`),
	})
	if err == nil {
		t.Skip("the unreachable target answered; nothing to assert")
	}
	if strings.Contains(err.Error(), "secret-path-nobody-should-see") {
		t.Fatalf("the transport error carries the registered URL: %v", err)
	}
}

// TestBackoffDoublesAndCaps pins the retry schedule the docs state.
func TestBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 16 * time.Minute, 30 * time.Minute,
	}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	// Past the cap, and below the floor.
	if got := Backoff(99); got != BackoffMax {
		t.Errorf("Backoff(99) = %s, want the cap %s", got, BackoffMax)
	}
	if got := Backoff(0); got != BackoffBase {
		t.Errorf("Backoff(0) = %s, want the base %s", got, BackoffBase)
	}

	// The documented span: MaxAttempts deliveries land inside about an hour,
	// which is the sentence docs/usage.md and .env.example both make.
	var total time.Duration
	for i := 1; i < MaxAttempts; i++ {
		total += Backoff(i)
	}
	if total < 55*time.Minute || total > 65*time.Minute {
		t.Errorf("%d attempts span %s; the docs say about an hour", MaxAttempts, total)
	}
}

// TestStatusClassStaysBounded is the cardinality rule, asserted rather than
// trusted (M13).
func TestStatusClassStaysBounded(t *testing.T) {
	seen := map[string]struct{}{}
	for code := 0; code <= 599; code++ {
		seen[statusClass(code)] = struct{}{}
	}
	if len(seen) != 5 {
		t.Fatalf("statusClass produces %d distinct labels across every code, want 5 "+
			"(none, 2xx, 3xx, 4xx, 5xx). A label per code would be a metric users "+
			"can grow.", len(seen))
	}
}

// dialLoopback exists so the guard test above is not the only thing proving the
// transport is the guarded one: it confirms a plain dialer reaches the same
// address the guarded one refuses.
func dialLoopback(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("the test's own listener at %s is unreachable: %v", address, err)
	}
	_ = conn.Close()
}

func TestTheGuardedTransportIsWhatRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The listener is genuinely reachable from this process.
	dialLoopback(t, strings.TrimPrefix(srv.URL, "http://"))

	// And the guarded client will not open it.
	client := NewClient(time.Second, nil)
	if _, err := client.Deliver(context.Background(), srv.URL, []byte("s"), Delivery{
		ID: uuid.Must(uuid.NewV7()), Event: "link.created", Payload: []byte(`{}`),
	}); !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("Deliver = %v, want ErrPrivateAddress against a listener this "+
			"process can otherwise reach", err)
	}
}
