package auth

import (
	"encoding/base32"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rfc6238Secret is the twenty-byte ASCII seed RFC 6238's Appendix B uses:
// "12345678901234567890", base32-encoded as this package stores secrets.
//
// The published test vectors are the only external evidence this implementation
// is TOTP and not something that merely looks like it. Hand-rolling RFC 6238 is
// the milestone's dependency decision (D72's reasoning, m53.md's second bullet),
// and this is the price of it: the vectors, or the claim is untested.
var rfc6238Secret = base32.StdEncoding.WithPadding(base32.NoPadding).
	EncodeToString([]byte("12345678901234567890"))

// TestTOTPMatchesTheRFC6238Vectors.
//
// The SHA-1 rows of RFC 6238 Appendix B, truncated to six digits. The published
// table gives eight; the specification's own text says the code is the low
// `digits` decimal places, so the six-digit form is the last six characters of
// each — which is what an authenticator app shows and what this product compares.
func TestTOTPMatchesTheRFC6238Vectors(t *testing.T) {
	// Time, then the eight-digit vector, then what six digits of it is.
	cases := []struct {
		unix   int64
		eight  string
		want   string
		moment string
	}{
		{59, "94287082", "287082", "one step in"},
		{1111111109, "07081804", "081804", "the step boundary, below"},
		{1111111111, "14050471", "050471", "the step boundary, above"},
		{1234567890, "89005924", "005924", "a round number"},
		{2000000000, "69279037", "279037", "well past 2038 in signed 32-bit terms"},
		{20000000000, "65353130", "353130", "past the 32-bit counter entirely"},
	}
	for _, c := range cases {
		t.Run(c.moment, func(t *testing.T) {
			step := TOTPStep(time.Unix(c.unix, 0))
			got, err := TOTPCode(rfc6238Secret, step)
			if err != nil {
				t.Fatalf("compute code: %v", err)
			}
			if got != c.want {
				t.Errorf("code at %d = %s, want %s (RFC 6238 Appendix B gives %s at eight digits). "+
					"This is not TOTP, and every authenticator app in the world disagrees with it",
					c.unix, got, c.want, c.eight)
			}
			if !strings.HasSuffix(c.eight, c.want) {
				t.Fatalf("the fixture is wrong: %s is not the low six digits of %s", c.want, c.eight)
			}
		})
	}
}

// TestTOTPAcceptsExactlyOneStepEitherSide.
//
// m53.md asks for clock skew to be answered by accepting the adjacent windows and
// documenting the tolerance as a number, *and no more*. This is the *and no more*:
// two steps out is refused in both directions, so the ninety-second tolerance is a
// bound rather than a starting point.
func TestTOTPAcceptsExactlyOneStepEitherSide(t *testing.T) {
	// **The bound is written out rather than read from TOTPSkew**, and the first
	// version of this test did read it — which made every assertion below a
	// tautology: widening the constant widened the expectation with it, and
	// sabotaging TOTPSkew to 3 left the test green. The tolerance is a documented
	// number (ninety seconds), so the number is what is asserted.
	if TOTPSkew != 1 {
		t.Fatalf("TOTPSkew = %d, want 1. The documented tolerance is one step either "+
			"side — ninety seconds in total — and docs/SECURITY.md states it as that "+
			"number. Every extra step multiplies the guessing surface and extends how "+
			"long an observed code stays usable", TOTPSkew)
	}

	now := time.Unix(1_700_000_000, 0)
	current := TOTPStep(now)

	for _, offset := range []int64{-2, -1, 0, 1, 2} {
		code, err := TOTPCode(rfc6238Secret, current+offset)
		if err != nil {
			t.Fatal(err)
		}
		step, ok := TOTPVerify(rfc6238Secret, code, now)
		wantOK := offset >= -1 && offset <= 1
		if ok != wantOK {
			t.Errorf("a code %+d steps from now: accepted = %v, want %v. The tolerance is "+
				"one step either side, and no more", offset, ok, wantOK)
		}
		if ok && step != current+offset {
			t.Errorf("accepted a code %+d steps out and reported step %d, want %d. The "+
				"reported step is what the replay guard records; the wrong one either "+
				"lets the code work twice or refuses the next two windows",
				offset, step, current+offset)
		}
	}
}

// TestTOTPRefusesWhatIsNotACode.
//
// Six digits is checked before anything is computed, so a pasted password does not
// cost three HMACs. Nothing here is a secret — the digit count is in the URI the
// person scanned.
func TestTOTPRefusesWhatIsNotACode(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, code := range []string{"", "12345", "1234567", "abcdef", "correct horse battery"} {
		if _, ok := TOTPVerify(rfc6238Secret, code, now); ok {
			t.Errorf("%q was accepted as a code", code)
		}
	}
}

// TestTOTPToleratesHowASecretWasWrittenDown.
//
// Padding, lower case and the spaces an authenticator app puts in when it shows a
// secret for transcription. Refusing any of them would be this product being
// stricter than the format for no reason, on the one path where somebody is typing
// by hand.
func TestTOTPToleratesHowASecretWasWrittenDown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	code, err := TOTPCode(rfc6238Secret, TOTPStep(now))
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{
		rfc6238Secret,
		strings.ToLower(rfc6238Secret),
		rfc6238Secret + "======",
		rfc6238Secret[:8] + " " + rfc6238Secret[8:],
	} {
		if _, ok := TOTPVerify(variant, code, now); !ok {
			t.Errorf("the secret written as %q did not verify its own code", variant)
		}
	}
}

// TestANewSecretIsWhatAnAuthenticatorExpects.
//
// Twenty bytes of entropy, base32 with no padding, thirty-two characters. Each is
// a thing an authenticator app assumes rather than negotiates, and a secret that
// breaks one of them produces an entry that silently generates codes this product
// refuses.
func TestANewSecretIsWhatAnAuthenticatorExpects(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := NewTOTPSecret()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, "=") {
			t.Fatalf("secret %q is padded; several apps refuse a padded one", s)
		}
		if s != strings.ToUpper(s) {
			t.Fatalf("secret %q is not upper case", s)
		}
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
		if err != nil {
			t.Fatalf("secret %q is not base32: %v", s, err)
		}
		if len(raw) != TOTPSecretBytes {
			t.Fatalf("secret decodes to %d bytes, want %d", len(raw), TOTPSecretBytes)
		}
		if seen[s] {
			t.Fatal("two generated secrets were the same, which means this is not reading crypto/rand")
		}
		seen[s] = true
	}
}

// TestTheEnrolmentURIIsWhatAnAppCanRead.
//
// The de-facto Key URI Format, which every authenticator app implements. The
// issuer appears twice on purpose — older apps read the label prefix and newer
// ones read the query parameter — and every parameter is named rather than left to
// a default, because an app that assumes different ones produces codes this
// product refuses with no way for anybody to see why.
func TestTheEnrolmentURIIsWhatAnAppCanRead(t *testing.T) {
	uri := TOTPURI("links.example.com", "someone@example.com", rfc6238Secret)

	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("uri does not start with the scheme an app registers: %q", uri)
	}
	label, query, ok := strings.Cut(strings.TrimPrefix(uri, "otpauth://totp/"), "?")
	if !ok {
		t.Fatalf("uri has no query: %q", uri)
	}
	if want := url.PathEscape("links.example.com") + ":" + url.PathEscape("someone@example.com"); label != want {
		t.Errorf("label = %q, want %q. An app that reads only the label files the "+
			"entry under this", label, want)
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("query does not parse: %v", err)
	}
	for key, want := range map[string]string{
		"secret":    rfc6238Secret,
		"issuer":    "links.example.com",
		"algorithm": "SHA1",
		"digits":    "6",
		"period":    "30",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestAnEscapedIssuerCannotBreakOutOfTheURI.
//
// The issuer is the instance's own host and the account is an address, so neither
// is attacker-controlled today. Escaping them anyway is what stops that from being
// a fact this function depends on — a later milestone that lets somebody name their
// own instance label should find this test rather than find out.
func TestAnEscapedIssuerCannotBreakOutOfTheURI(t *testing.T) {
	uri := TOTPURI("evil/../?x=1&issuer=Bank", "a@b.test", rfc6238Secret)
	label, query, _ := strings.Cut(strings.TrimPrefix(uri, "otpauth://totp/"), "?")
	// `?` and `#` are what end a path, and either one unescaped would let the
	// label smuggle in a query. `&` is deliberately not checked: inside the path
	// it is an ordinary character and cannot start a parameter, which is why
	// url.PathEscape leaves it alone and why demanding otherwise would be testing
	// the standard library's taste rather than this function's correctness.
	if strings.ContainsAny(label, "?#") {
		t.Errorf("the label carries an unescaped path terminator: %q", label)
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("query does not parse: %v", err)
	}
	if len(q["issuer"]) != 1 {
		t.Errorf("issuer appears %d times in the query; an injected one would be the "+
			"second, and an app taking the last would file the entry under it", len(q["issuer"]))
	}
	if q.Get("secret") != rfc6238Secret {
		t.Errorf("the secret parameter was displaced: %q", q.Get("secret"))
	}
}

// --- the secret at rest ---------------------------------------------------------

func TestASealedSecretComesBackAndNothingElseDoes(t *testing.T) {
	c, err := NewMFACipher(strings.Repeat("k", 48))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal(rfc6238Secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, rfc6238Secret) {
		t.Fatal("the ciphertext contains the plaintext, which means nothing was encrypted")
	}
	got, err := c.Open(sealed)
	if err != nil || got != rfc6238Secret {
		t.Fatalf("Open(Seal(x)) = %q, %v; want the secret back", got, err)
	}

	// A different key is a decryption failure and not a different secret. This is
	// the operator-error path m53.md bounds: losing MFA_SECRET_KEY locks every
	// enrolled account out of the second factor and no further.
	other, err := NewMFACipher(strings.Repeat("j", 48))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a secret sealed under one key opened under another")
	}
}

func TestEverySealIsDifferentForTheSameSecret(t *testing.T) {
	c, err := NewMFACipher(strings.Repeat("k", 48))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Seal(rfc6238Secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Seal(rfc6238Secret)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two seals of one secret produced identical bytes. The nonce is not " +
			"random, and the column would then tell a reader which accounts share a secret")
	}
}

// TestATamperedSecretIsRefusedRatherThanDecoded.
//
// GCM is authenticated, which is the whole reason it was chosen over a bare
// stream: a flipped bit in the column is a refusal, not a secret an attacker
// chose. Every malformed shape answers the same error, because they are one
// operator problem.
func TestATamperedSecretIsRefusedRatherThanDecoded(t *testing.T) {
	c, err := NewMFACipher(strings.Repeat("k", 48))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal(rfc6238Secret)
	if err != nil {
		t.Fatal(err)
	}

	flipped := []byte(sealed)
	flipped[len(flipped)-1] ^= 0x01
	for _, bad := range []struct{ name, value string }{
		{"a flipped byte", string(flipped)},
		{"an unknown scheme", "9." + strings.TrimPrefix(sealed, "1.")},
		{"no scheme at all", strings.TrimPrefix(sealed, "1.")},
		{"not base64", "1.!!!!"},
		{"shorter than a nonce", "1.AAAA"},
		{"empty", ""},
	} {
		if _, err := c.Open(bad.value); err == nil {
			t.Errorf("%s opened successfully", bad.name)
		}
	}
}

// TestTheMFAKeyFloorIsTheOneConfigEnforces.
//
// internal/config writes `32` out rather than importing this constant, because
// that package reads the environment for every other package and depends on none
// of them. This is what holds the two together — the number is in two files and
// they are checked against each other here.
func TestTheMFAKeyFloorIsTheOneConfigEnforces(t *testing.T) {
	if MFAKeyMinBytes != 32 {
		t.Fatalf("MFAKeyMinBytes = %d; internal/config's validation writes 32 out and "+
			"would now accept a key this package calls too short", MFAKeyMinBytes)
	}
	if _, err := NewMFACipher(strings.Repeat("k", MFAKeyMinBytes-1)); err == nil {
		t.Error("a key one byte under the floor was accepted")
	}
	if _, err := NewMFACipher(""); err == nil {
		t.Error("an empty key was accepted; unset must be a refusal, not a working cipher")
	}
}

// --- recovery codes ---------------------------------------------------------------

func TestARecoveryCodeIsReadableAndUnbiased(t *testing.T) {
	if len(recoveryCodeChars) != 32 {
		t.Fatalf("the alphabet has %d symbols, not 32. `v & 31` is only uniform over "+
			"exactly thirty-two, and any other size needs a rejection loop this "+
			"generator does not have", len(recoveryCodeChars))
	}
	for _, ambiguous := range []rune{'i', 'l', 'o', 'u'} {
		if strings.ContainsRune(recoveryCodeChars, ambiguous) {
			t.Errorf("the alphabet contains %q, which somebody transcribing from "+
				"paper gets wrong", ambiguous)
		}
	}

	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		code, err := newRecoveryCode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[code] {
			t.Fatal("two generated codes were the same")
		}
		seen[code] = true
		if want := recoveryCodeLength + 1; len(code) != want {
			t.Fatalf("code %q is %d characters, want %d including the separator",
				code, len(code), want)
		}
		body := strings.ReplaceAll(code, "-", "")
		for _, r := range body {
			if !strings.ContainsRune(recoveryCodeChars, r) {
				t.Fatalf("code %q contains %q, which is not in the alphabet", code, r)
			}
		}
	}
}

// TestARecoveryCodeMatchesHoweverItIsTyped.
//
// Hashing goes through one normalizer on both sides, so a code typed from paper in
// upper case with the hyphen left out is the same code. Two functions that agreed
// would be one refactor away from not agreeing, and the failure would be somebody
// locked out holding a code that works.
func TestARecoveryCodeMatchesHoweverItIsTyped(t *testing.T) {
	code, err := newRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	canonical := normalizeRecoveryCode(code)
	for _, typed := range []string{
		code,
		strings.ToUpper(code),
		strings.ReplaceAll(code, "-", ""),
		strings.ReplaceAll(code, "-", " "),
		" " + code + " ",
	} {
		if got := normalizeRecoveryCode(strings.TrimSpace(typed)); got != canonical {
			t.Errorf("%q normalizes to %q, want %q", typed, got, canonical)
		}
	}
}

// --- the two claims m53.md makes about this milestone's footprint ------------------

// TestNoModuleDependencyJoinedTheSetForTheSecondFactor is m53.md's dependency
// bullet, checked where the claim lives.
//
// **The idiom is D72's, which internal/qr's png_test.go established**: the list is
// written out rather than counted, because a count would pass for a swap. RFC 6238
// is HMAC over a time counter and AES-GCM is in the standard library, so a second
// factor that added a module would be paying a supply-chain cost on the login path
// for arithmetic.
func TestNoModuleDependencyJoinedTheSetForTheSecondFactor(t *testing.T) {
	// As of M53, and identical to the list internal/qr holds at M49. A milestone
	// that adds a direct dependency changes both lists deliberately and says why
	// in decisions.md.
	want := map[string]bool{
		"github.com/boombuler/barcode":            true,
		"github.com/caarlos0/env/v11":             true,
		"github.com/getkin/kin-openapi":           true,
		"github.com/google/uuid":                  true,
		"github.com/jackc/pgx/v5":                 true,
		"github.com/joho/godotenv":                true,
		"github.com/oschwald/maxminddb-golang/v2": true,
		"github.com/pressly/goose/v3":             true,
		"github.com/prometheus/client_golang":     true,
		"github.com/redis/go-redis/v9":            true,
		"golang.org/x/crypto":                     true,
		"golang.org/x/net":                        true,
		"golang.org/x/sync":                       true,
		"gopkg.in/yaml.v3":                        true,
	}
	for _, path := range mfaDirectRequires(t) {
		if !want[path] {
			t.Errorf("go.mod requires %s directly, and M53's claim is that a second "+
				"factor joins no module to the dependency set. If this is a deliberate "+
				"addition from a later milestone, add it to the list here with its "+
				"reason in decisions.md", path)
		}
		delete(want, path)
	}
	for path := range want {
		t.Errorf("%s is in this test's list and no longer in go.mod. A dependency "+
			"leaving is fine; a list that no longer describes the file is not", path)
	}

	// And the algorithm this package actually uses is the standard library's, so
	// the assertion above is about the right thing.
	src, err := os.ReadFile("totp.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{`"crypto/hmac"`, `"crypto/sha1"`, `"encoding/base32"`} {
		if !strings.Contains(string(src), pkg) {
			t.Errorf("totp.go does not import %s; the require block above is then "+
				"proving nothing about where the algorithm came from", pkg)
		}
	}
}

// mfaDirectRequires is every non-indirect module path in go.mod.
//
// Parsed by hand rather than through golang.org/x/mod, which would be a module
// dependency added by the test that asserts no module dependency was added. A
// second copy of internal/qr's parser, deliberately: importing a test helper
// across packages is not a thing Go does, and the alternative is a shared
// non-test package existing only for this.
func mfaDirectRequires(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case !inBlock, line == "", strings.HasPrefix(line, "//"):
			continue
		case strings.Contains(line, "// indirect"):
			continue
		}
		if path, _, ok := strings.Cut(line, " "); ok {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no direct requirements were parsed out of go.mod; the parser is " +
			"reading nothing and would pass for any dependency at all")
	}
	return paths
}
