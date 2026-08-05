package auth

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// testParams keep the tests fast. Real cost is 64 MiB; using it here would
// make the suite take minutes for no extra assurance, since the parameters are
// carried in the hash and exercised by TestVerifyUsesParamsFromTheHash.
var testParams = Params{MemoryKiB: 19 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func testHasher() *Hasher { return NewHasher(testParams) }

func TestHashAndVerifyRoundTrip(t *testing.T) {
	h := testHasher()
	const pw = "correct horse battery staple"

	encoded, err := h.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := h.Verify(pw, encoded); err != nil {
		t.Errorf("Verify with the correct password: %v", err)
	}
	if err := h.Verify("wrong password entirely", encoded); !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify with a wrong password = %v, want ErrMismatch", err)
	}
}

func TestHashIsSaltedPerCall(t *testing.T) {
	h := testHasher()
	const pw = "the same password twice"

	a, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("hashing the same password twice produced identical output; the salt is not random")
	}
	// Both must still verify.
	if err := h.Verify(pw, a); err != nil {
		t.Error(err)
	}
	if err := h.Verify(pw, b); err != nil {
		t.Error(err)
	}
}

func TestHashFormatIsPHC(t *testing.T) {
	h := testHasher()
	encoded, err := h.Hash("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("hash = %q, want a PHC argon2id string", encoded)
	}
	// The parameters must be embedded, which is what allows them to be raised
	// later without invalidating existing passwords.
	if !strings.Contains(encoded, "m=19456,t=1,p=1") {
		t.Errorf("hash = %q, want the cost parameters embedded", encoded)
	}
	if n := strings.Count(encoded, "$"); n != 5 {
		t.Errorf("hash has %d '$' separators, want 5", n)
	}
}

func TestPasswordLengthLimits(t *testing.T) {
	h := testHasher()

	if _, err := h.Hash("short"); err == nil {
		t.Error("Hash accepted a password below the minimum length")
	}
	if _, err := h.Hash(strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Error("Hash accepted a password above the maximum length; " +
			"unbounded input is a denial-of-service vector against a deliberately slow function")
	}
	if _, err := h.Hash(strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Errorf("Hash rejected a password at exactly the minimum length: %v", err)
	}
}

func TestVerifyRejectsOverlongInputWithoutHashing(t *testing.T) {
	h := testHasher()
	encoded, err := h.Hash("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	// Must not spend argon2 work on input it will reject anyway.
	start := time.Now()
	err = h.Verify(strings.Repeat("x", MaxPasswordLength*4), encoded)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify with overlong input = %v, want ErrMismatch", err)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("rejecting overlong input took %s; it should short-circuit before hashing", elapsed)
	}
}

// TestVerifyUsesParamsFromTheHash is the property that makes raising the cost
// safe. A hash created under weaker settings must keep verifying after the
// policy is raised, or every existing user is locked out by a config change.
func TestVerifyUsesParamsFromTheHash(t *testing.T) {
	weak := NewHasher(Params{MemoryKiB: 19 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	strong := NewHasher(Params{MemoryKiB: 32 * 1024, Iterations: 2, Parallelism: 2, SaltLength: 16, KeyLength: 32})

	const pw = "a password hashed under the old policy"
	old, err := weak.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}

	if err := strong.Verify(pw, old); err != nil {
		t.Fatalf("a hash made with weaker parameters stopped verifying after the policy was raised: %v", err)
	}
	if !strong.NeedsRehash(old) {
		t.Error("NeedsRehash = false for a hash below current policy; it would never be upgraded")
	}
	if weak.NeedsRehash(old) {
		t.Error("NeedsRehash = true for a hash at current policy; it would be rewritten on every login")
	}

	// After rehashing it should be at the new policy.
	upgraded, err := strong.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if strong.NeedsRehash(upgraded) {
		t.Error("a freshly created hash already reports NeedsRehash")
	}
}

func TestDecodeRejectsMalformedHashes(t *testing.T) {
	h := testHasher()
	bad := map[string]string{
		"empty":               "",
		"not phc":             "hunter2",
		"too few fields":      "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"wrong algorithm":     "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"bcrypt":              "$2y$10$abcdefghijklmnopqrstuv",
		"unsupported version": "$argon2id$v=16$m=65536,t=3,p=2$c2FsdA$a2V5",
		"bad params":          "$argon2id$v=19$m=x,t=3,p=2$c2FsdA$a2V5",
		"zero memory":         "$argon2id$v=19$m=0,t=3,p=2$c2FsdA$a2V5",
		"bad base64 salt":     "$argon2id$v=19$m=65536,t=3,p=2$!!!!$a2V5",
		"empty key":           "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
	}

	for name, encoded := range bad {
		t.Run(name, func(t *testing.T) {
			err := h.Verify("any password at all", encoded)
			if err == nil {
				t.Fatal("Verify succeeded against a malformed hash")
			}
			// A corrupt row must be distinguishable from a wrong password, or
			// the real problem is reported to the user as a login failure and
			// never investigated.
			if errors.Is(err, ErrMismatch) {
				t.Errorf("malformed hash reported as ErrMismatch, hiding the real fault: %v", err)
			}
			// And it must be treated as needing replacement.
			if !h.NeedsRehash(encoded) {
				t.Error("NeedsRehash = false for an unparseable hash")
			}
		})
	}
}

// TestDummyVerifyCostsTheSameAsARealOne guards the account-enumeration
// defence. If "no such user" returns far faster than a real verification, the
// difference is a reliable oracle for which emails are registered.
func TestDummyVerifyCostsTheSameAsARealOne(t *testing.T) {
	h := testHasher()
	const pw = "a password of realistic length"
	encoded, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}

	// Warm up, so the first allocation does not skew the comparison.
	h.DummyVerify(pw)
	_ = h.Verify(pw, encoded)

	const runs = 5
	var real, dummy time.Duration
	for i := 0; i < runs; i++ {
		start := time.Now()
		_ = h.Verify("the wrong password here", encoded)
		real += time.Since(start)

		start = time.Now()
		h.DummyVerify("the wrong password here")
		dummy += time.Since(start)
	}
	real /= runs
	dummy /= runs

	// Generous bound: this is a timing test on a shared CI machine, and the
	// failure it must catch is an order-of-magnitude gap, not a few percent.
	ratio := float64(dummy) / float64(real)
	if ratio < 0.25 || ratio > 4.0 {
		t.Errorf("dummy verification took %s vs %s for a real one (ratio %.2f); "+
			"the gap is a usable account-enumeration oracle", dummy, real, ratio)
	}
}

func TestHasherIsSafeForConcurrentUse(t *testing.T) {
	h := testHasher()
	const pw = "concurrent password work"
	encoded, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Verify(pw, encoded); err != nil {
				t.Errorf("concurrent Verify: %v", err)
			}
			if _, err := h.Hash(pw); err != nil {
				t.Errorf("concurrent Hash: %v", err)
			}
		}()
	}
	wg.Wait()
}

func FuzzDecode(f *testing.F) {
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5")
	f.Add("")
	f.Add("$$$$$")
	f.Add("$argon2id$v=19$m=1,t=1,p=1$AA$AA")

	h := testHasher()
	f.Fuzz(func(t *testing.T, encoded string) {
		// Must never panic, whatever is in the column.
		_ = h.Verify("some password", encoded)
		_ = h.NeedsRehash(encoded)
	})
}

// --- sessions ---------------------------------------------------------------

func TestSessionTokenIsRandomAndHashed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		token, hash, err := NewSessionToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("duplicate session token %q", token)
		}
		seen[token] = true

		if len(hash) != 32 {
			t.Fatalf("hash length %d, want 32", len(hash))
		}
		// The stored form must not be the token itself, or a database leak
		// hands over live sessions.
		if string(hash) == token {
			t.Fatal("stored hash equals the raw token")
		}
		if got := HashSessionToken(token); string(got) != string(hash) {
			t.Fatal("HashSessionToken is not deterministic")
		}
	}
}

func TestCookieNameUsesHostPrefixOnlyWhenSecure(t *testing.T) {
	// The __Host- prefix is rejected by browsers over plain HTTP, so using it
	// in local development would silently break login rather than fail loudly.
	if got := CookieName(true); got != "__Host-linkctrl_session" {
		t.Errorf("secure cookie name = %q, want the __Host- prefix", got)
	}
	if got := CookieName(false); strings.HasPrefix(got, "__Host-") {
		t.Errorf("insecure cookie name = %q, must not use the __Host- prefix", got)
	}
}

func TestSessionTTLEnforcesBothDeadlines(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ttl := SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour}

	tests := []struct {
		name    string
		session Session
		wantErr bool
	}{
		{
			name:    "fresh",
			session: Session{LastSeenAt: now.Add(-time.Minute), ExpiresAt: now.Add(29 * 24 * time.Hour)},
		},
		{
			name:    "past absolute deadline",
			session: Session{LastSeenAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second)},
			wantErr: true,
		},
		{
			name:    "idle too long but within absolute",
			session: Session{LastSeenAt: now.Add(-8 * 24 * time.Hour), ExpiresAt: now.Add(20 * 24 * time.Hour)},
			wantErr: true,
		},
		{
			name:    "idle exactly at the limit",
			session: Session{LastSeenAt: now.Add(-7 * 24 * time.Hour), ExpiresAt: now.Add(20 * 24 * time.Hour)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ttl.Valid(tc.session, now)
			if tc.wantErr && err == nil {
				t.Error("expected the session to be rejected")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected the session to be accepted, got %v", err)
			}
		})
	}
}

func TestAnonymizeIP(t *testing.T) {
	tests := []struct{ in, want string }{
		{"203.0.113.42", "203.0.113.0/24"},
		{"10.1.2.3", "10.1.2.0/24"},
		{"2001:db8:1234:5678::1", "2001:db8:1234::/48"},
		// An IPv4-mapped IPv6 address must fold to IPv4 first. Masking it to
		// /48 as though it were IPv6 would preserve the whole IPv4 address,
		// which is precisely the data this function exists to discard.
		{"::ffff:203.0.113.42", "203.0.113.0/24"},
		{"127.0.0.1", "127.0.0.0/24"},
		// 6to4: the client's IPv4 address sits in bytes 2-5, inside the /48 an
		// IPv6 address is masked to, so masking as IPv6 preserved all four
		// octets of it (F59).
		{"2002:cb00:712a::1", "203.0.113.0/24"},
		// Every other v4-in-v6 scheme embeds in the low bits a /48 discards, so
		// each of these is masked as ordinary IPv6 and must stay that way — a
		// fold applied too widely would turn a real IPv6 client into somebody
		// else's IPv4 address.
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:0:4136::/48"}, // Teredo
		{"64:ff9b::cb00:712a", "64:ff9b::/48"},                       // NAT64
		{"2001:db8::5efe:cb00:712a", "2001:db8::/48"},                // ISATAP
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := AnonymizeIP(addr); got != tc.want {
				t.Errorf("AnonymizeIP(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	if got := AnonymizeIP(netip.Addr{}); got != "" {
		t.Errorf("AnonymizeIP(invalid) = %q, want empty", got)
	}
}

func TestLockoutPolicy(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	p := LockoutPolicy{Threshold: 5, Window: 15 * time.Minute}

	if until := p.LockedUntil(4, now.Add(-time.Minute), now); !until.IsZero() {
		t.Error("locked below the threshold")
	}
	if until := p.LockedUntil(5, now.Add(-time.Minute), now); until.IsZero() {
		t.Error("not locked at the threshold")
	}
	// The lockout must expire on its own. A lock that persists until an
	// administrator clears it turns a failed-login flood into a denial of
	// service against the account owner.
	if until := p.LockedUntil(5, now.Add(-16*time.Minute), now); !until.IsZero() {
		t.Error("still locked after the window elapsed")
	}
	if until := p.LockedUntil(99, now.Add(-time.Minute), now); until.IsZero() {
		t.Error("not locked well above the threshold")
	}
}
