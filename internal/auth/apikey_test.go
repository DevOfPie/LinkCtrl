package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testPepper = []byte("a-test-pepper-of-at-least-32-bytes!!")

func TestNewAPIKeyTokenShape(t *testing.T) {
	token, prefix, secret, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}

	if !strings.HasPrefix(token, apiKeyTag) {
		t.Errorf("token %q does not start with %q", token, apiKeyTag)
	}
	if got := len(token); got != apiKeyTokenLength {
		t.Errorf("token length = %d, want %d", got, apiKeyTokenLength)
	}
	if token != prefix+"_"+secret {
		t.Errorf("token %q is not prefix+_+secret", token)
	}
	if got := len(prefix); got != APIKeyPrefixLength {
		t.Errorf("prefix length = %d, want %d", got, APIKeyPrefixLength)
	}

	// The whole point of the prefix being public is that it can be shown,
	// logged and stored. It must therefore reveal nothing about the secret.
	if strings.Contains(prefix, secret) || strings.Contains(secret, prefix) {
		t.Error("prefix and secret overlap")
	}

	// The token must survive being pasted anywhere without quoting.
	if strings.ContainsAny(token, " \t\r\n\"'`$\\;&|") {
		t.Errorf("token %q contains a character that needs escaping", token)
	}
}

func TestNewAPIKeyTokenIsRandom(t *testing.T) {
	const n = 100
	prefixes := make(map[string]struct{}, n)
	secrets := make(map[string]struct{}, n)

	for range n {
		_, prefix, secret, err := newAPIKeyToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := prefixes[prefix]; dup {
			t.Fatalf("prefix %q generated twice in %d attempts", prefix, n)
		}
		if _, dup := secrets[secret]; dup {
			t.Fatalf("secret generated twice in %d attempts", n)
		}
		prefixes[prefix] = struct{}{}
		secrets[secret] = struct{}{}
	}
}

func TestParseAPIKeyRoundTrip(t *testing.T) {
	token, prefix, secret, err := newAPIKeyToken()
	if err != nil {
		t.Fatal(err)
	}

	gotPrefix, gotSecret, err := ParseAPIKey(token)
	if err != nil {
		t.Fatalf("ParseAPIKey(%q): %v", token, err)
	}
	if gotPrefix != prefix || gotSecret != secret {
		t.Errorf("ParseAPIKey = (%q, %q), want (%q, %q)", gotPrefix, gotSecret, prefix, secret)
	}
}

func TestParseAPIKeyRejectsMalformed(t *testing.T) {
	valid, prefix, secret, err := newAPIKeyToken()
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":              "",
		"no tag":             prefix[len(apiKeyTag):] + "_" + secret,
		"wrong tag":          "sk_live_" + prefix[len(apiKeyTag):] + "_" + secret,
		"test tag":           "lk_test_" + prefix[len(apiKeyTag):] + "_" + secret,
		"truncated secret":   valid[:len(valid)-1],
		"extra character":    valid + "x",
		"separator replaced": valid[:APIKeyPrefixLength] + "-" + secret,
		"prefix only":        prefix,
		"session token":      "wSjMCE9SdMe2z5tvVoJc0kX9VUZzB7SqvTLQD6q9Twk",
		"whitespace":         " " + valid[1:],
	}

	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseAPIKey(token); !errors.Is(err, ErrAPIKeyInvalid) {
				t.Errorf("ParseAPIKey(%q) = %v, want ErrAPIKeyInvalid", token, err)
			}
		})
	}
}

func TestAPIKeyHashProperties(t *testing.T) {
	const (
		prefix = "lk_live_abcdefgh"
		secret = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	)

	base := APIKeyHash(testPepper, prefix, secret)
	if len(base) != 32 {
		t.Fatalf("hash length = %d, want 32", len(base))
	}
	if string(APIKeyHash(testPepper, prefix, secret)) != string(base) {
		t.Error("hashing is not deterministic")
	}

	// Without the pepper a stolen database dump would be enough to verify keys
	// offline, which is the entire reason it exists.
	other := APIKeyHash([]byte("a-different-pepper-of-32-plus-bytes!"), prefix, secret)
	if string(other) == string(base) {
		t.Error("hash does not depend on the pepper")
	}

	if string(APIKeyHash(testPepper, prefix, secret[:len(secret)-1]+"H")) == string(base) {
		t.Error("hash does not depend on the secret")
	}

	// The prefix is in the message, so a hash copied into another key's row no
	// longer verifies.
	if string(APIKeyHash(testPepper, "lk_live_zzzzzzzz", secret)) == string(base) {
		t.Error("hash is not bound to its prefix")
	}
}

// The NUL separator is what stops the two fields being run together: without
// it, a shifted boundary between prefix and secret would hash identically.
func TestAPIKeyHashSeparatesFields(t *testing.T) {
	a := APIKeyHash(testPepper, "lk_live_ab", "cdef")
	b := APIKeyHash(testPepper, "lk_live_a", "bcdef")
	if string(a) == string(b) {
		t.Error("moving the boundary between prefix and secret produced the same hash")
	}
}

func TestRestrictToIntersects(t *testing.T) {
	id := &Identity{permissions: map[string]struct{}{
		"links.read":   {},
		"links.create": {},
		"tags.read":    {},
	}}

	// A scope the owner does not hold must not appear, or minting a key would
	// be a way to grant yourself permissions.
	id.restrictTo([]string{"links.read", "links.delete", "org.delete"})

	if !id.Can("links.read") {
		t.Error("links.read was granted and held, but is not allowed")
	}
	for _, p := range []string{"links.create", "tags.read", "links.delete", "org.delete"} {
		if id.Can(p) {
			t.Errorf("%s should not be allowed", p)
		}
	}
}

func TestRestrictToEmptyScopesGrantsNothing(t *testing.T) {
	id := &Identity{permissions: map[string]struct{}{"links.read": {}}}
	id.restrictTo(nil)
	if id.Can("links.read") {
		t.Error("a key with no scopes must authorize nothing")
	}
	if len(id.Permissions()) != 0 {
		t.Errorf("permissions = %v, want none", id.Permissions())
	}
}

func TestNonDelegableScopesCoverKeyManagement(t *testing.T) {
	// If key management ever became delegable, a leaked key could mint its own
	// replacement and revocation would stop meaning anything.
	for _, scope := range []string{PermAPIKeysRead, PermAPIKeysWrite} {
		if !isNonDelegable(scope) {
			t.Errorf("%s must not be delegable to an API key", scope)
		}
	}
	if isNonDelegable("links.read") {
		t.Error("links.read must be delegable, or keys are useless")
	}
}

// audit.read is non-delegable for a different reason from the rest of the set,
// and the difference is the point of asserting it separately. It escalates
// nothing and reverses nothing; it is refused because the audit log is where a
// network prefix is tied to a named person.
//
// This map is also the only thing enforcing that — the endpoint authorizes on
// the permission like any other — so if this entry goes, audit.read becomes
// delegable with no other code change and no other test noticing.
func TestAuditReadIsNotDelegable(t *testing.T) {
	if !isNonDelegable("audit.read") {
		t.Error("audit.read must not be delegable to an API key: it is the one " +
			"permission that discloses which named person acted from which network, " +
			"and NonDelegableScopes is the only place that is enforced")
	}
}

func TestIsAPIKey(t *testing.T) {
	var nilIdentity *Identity
	if nilIdentity.IsAPIKey() {
		t.Error("a nil identity is not an API key")
	}
	if (&Identity{}).IsAPIKey() {
		t.Error("a session identity reported itself as an API key")
	}
}

func TestUsageTrackerCoalescesAndKeepsTheLatest(t *testing.T) {
	tr := newKeyUsageTracker(nil, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))

	id := uuid.Must(uuid.NewV7())
	first := time.Now()
	later := first.Add(time.Second)

	tr.touch(id, later)
	tr.touch(id, first) // out of order, must not move the timestamp backwards
	tr.touch(id, later)

	if len(tr.pending) != 1 {
		t.Fatalf("pending = %d entries, want 1; repeated use of one key must collapse to one write",
			len(tr.pending))
	}
	if got := tr.pending[id]; !got.Equal(later) {
		t.Errorf("pending timestamp = %s, want the latest (%s)", got, later)
	}
}

func TestUsageTrackerIsBounded(t *testing.T) {
	tr := newKeyUsageTracker(nil, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))

	now := time.Now()
	for range maxPendingKeyTouches + 10 {
		tr.touch(uuid.Must(uuid.NewV7()), now)
	}

	if len(tr.pending) > maxPendingKeyTouches {
		t.Errorf("pending grew to %d, past the %d cap", len(tr.pending), maxPendingKeyTouches)
	}
	if tr.dropped != 10 {
		t.Errorf("dropped = %d, want 10; drops must be counted rather than silent", tr.dropped)
	}
}

// close must stop the loop and be safe to call twice, because shutdown runs it
// from the server hook and a test or a second signal can run it again.
func TestUsageTrackerCloseIsIdempotent(t *testing.T) {
	tr := newKeyUsageTracker(nil, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tr.start()

	// Nothing pending, so flush returns before it would need the pool.
	if err := tr.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := tr.close(context.Background()); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestNewAPIKeyServiceRejectsWeakPepper(t *testing.T) {
	if _, err := NewAPIKeyService(nil, nil, APIKeyConfig{Pepper: []byte("short")}); err == nil {
		t.Error("a pepper below the floor was accepted; config validation could then be bypassed in code")
	}
}
