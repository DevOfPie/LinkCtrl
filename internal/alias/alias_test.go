package alias

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestAlphabetIsPowerOfTwo(t *testing.T) {
	// Random reduces a uniform byte modulo len(Alphabet). That is only unbiased
	// when len(Alphabet) divides 256. If someone adds or removes a character,
	// generated codes silently stop being uniform — so fail loudly here.
	n := len(Alphabet)
	if n == 0 || 256%n != 0 {
		t.Fatalf("len(Alphabet) = %d, which does not divide 256; generation would be biased", n)
	}
	if n != 32 {
		t.Errorf("len(Alphabet) = %d, want 32 (update the docs if this is intentional)", n)
	}
}

func TestAlphabetExcludesConfusableCharacters(t *testing.T) {
	for _, c := range []string{"i", "l", "o"} {
		if strings.Contains(Alphabet, c) {
			t.Errorf("alphabet contains %q, which is confusable with a digit", c)
		}
	}
	// Uppercase must be absent: aliases are lowercase-canonical.
	if Alphabet != strings.ToLower(Alphabet) {
		t.Error("alphabet contains uppercase characters")
	}
	seen := map[rune]bool{}
	for _, r := range Alphabet {
		if seen[r] {
			t.Errorf("alphabet contains duplicate %q", r)
		}
		seen[r] = true
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		// zero Reason means "expect success"
		reason Reason
	}{
		{name: "simple", input: "abc", want: "abc"},
		{name: "typical generated", input: "k3m9x2p", want: "k3m9x2p"},
		{name: "digits allowed", input: "123", want: "123"},
		{name: "hyphen inside", input: "my-link", want: "my-link"},
		{name: "underscore inside", input: "my_link", want: "my_link"},
		{name: "max length", input: strings.Repeat("a", MaxLength), want: strings.Repeat("a", MaxLength)},
		{name: "min length", input: "abc", want: "abc"},

		{name: "uppercase folded", input: "GitHub", want: "github"},
		{name: "mixed case folded", input: "MyLink", want: "mylink"},
		{name: "surrounding space trimmed", input: "  abc  ", want: "abc"},

		{name: "empty", input: "", reason: ReasonEmpty},
		{name: "whitespace only", input: "   ", reason: ReasonEmpty},
		{name: "too short", input: "ab", reason: ReasonTooShort},
		{name: "too long", input: strings.Repeat("a", MaxLength+1), reason: ReasonTooLong},

		{name: "dot rejected", input: "logo.png", reason: ReasonInvalidChars},
		{name: "slash rejected", input: "a/b", reason: ReasonInvalidChars},
		{name: "space inside", input: "my link", reason: ReasonInvalidChars},
		{name: "unicode", input: "café", reason: ReasonInvalidChars},
		{name: "emoji", input: "aa🎉", reason: ReasonInvalidChars},
		{name: "short and invalid reports chars", input: "é", reason: ReasonInvalidChars},
		{name: "percent", input: "a%20b", reason: ReasonInvalidChars},
		{name: "null byte", input: "ab\x00c", reason: ReasonInvalidChars},

		{name: "leading hyphen", input: "-abc", reason: ReasonEdgeSeparator},
		{name: "trailing hyphen", input: "abc-", reason: ReasonEdgeSeparator},
		{name: "leading underscore", input: "_abc", reason: ReasonEdgeSeparator},
		{name: "trailing underscore", input: "abc_", reason: ReasonEdgeSeparator},

		{name: "reserved route", input: "api", reason: ReasonReserved},
		{name: "reserved route cased", input: "API", reason: ReasonReserved},
		{name: "reserved dashboard", input: "dashboard", reason: ReasonReserved},
		{name: "reserved admin", input: "admin", reason: ReasonReserved},

		{name: "profane whole word", input: "shit", reason: ReasonProfane},
		{name: "profane leetspeak", input: "sh1t", reason: ReasonProfane},
		{name: "profane token", input: "my-shit-link", reason: ReasonProfane},
		{name: "profane substring", input: "somenigger1", reason: ReasonProfane},
		{name: "profane separator evasion", input: "n-i-g-g-e-r", reason: ReasonProfane},

		// Terms demoted from substring to whole-token matching must still be
		// rejected when they stand alone. Without these, the fix for the
		// "therapist" false positive would look like a way to disable the term.
		{name: "demoted term alone", input: "rapist", reason: ReasonProfane},
		{name: "demoted term as token", input: "the-coon-link", reason: ReasonProfane},
		{name: "demoted term leetspeak", input: "r4pist", reason: ReasonProfane},
		{name: "demoted retard alone", input: "retard", reason: ReasonProfane},
		{name: "demoted spastic alone", input: "spastic", reason: ReasonProfane},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(tc.input)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Validate(%q) = error %v, want success", tc.input, err)
				}
				if got != tc.want {
					t.Errorf("Validate(%q) = %q, want %q", tc.input, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%q) = %q, want rejection with reason %q", tc.input, got, tc.reason)
			}
			var ve *Error
			if !errors.As(err, &ve) {
				t.Fatalf("Validate(%q) returned %T, want *alias.Error", tc.input, err)
			}
			if ve.Reason != tc.reason {
				t.Errorf("Validate(%q) reason = %q, want %q (message: %s)",
					tc.input, ve.Reason, tc.reason, ve.Message)
			}
		})
	}
}

func TestValidateDoesNotFlagInnocentWords(t *testing.T) {
	// The word/substring split in profanity.txt exists precisely so these pass.
	// If someone moves a short term into the substring list, this catches it.
	innocent := []string{
		"class", "classic", "assets", "assessment", "passage", "bass", "mass",
		"grass", "compass", "embassy", "assign", "assistant", "cassette",
		"analysis", "canal", "scunthorpe", "penistone", "shitake-mushroom-x",
		"cockpit", "cocktail", "hancock", "peacock", "shuttlecock",
		"dickens", "dickinson", "sussex", "middlesex", "essex",
		"matsushita", "hello-world", "my-cool-link",

		// Each of these caught a real substring-list bug during development.
		// They stay here so that demoting a term back to substring matching
		// fails the build rather than silently rejecting valid aliases.
		"therapist", "therapists", // "rapist"
		"raccoon", "cocoon", "tycoon", // "coon"
		"retardant", "fire-retardant", "flame-retardant", // "retard"
		"spasticity", // "spastic"
	}
	for _, s := range innocent {
		t.Run(s, func(t *testing.T) {
			// "shitake-mushroom-x" tokenizes to shitake/mushroom/x, none of
			// which is a listed word, so it must pass.
			if _, err := Validate(s); err != nil {
				var ve *Error
				if errors.As(err, &ve) && (ve.Reason == ReasonProfane) {
					t.Errorf("Validate(%q) wrongly rejected as profane", s)
				}
			}
		})
	}
}

func TestValidateIsIdempotent(t *testing.T) {
	inputs := []string{"abc", "GitHub", "  Mixed-Case_99  ", "k3m9x2p", strings.Repeat("z", MaxLength)}
	for _, in := range inputs {
		first, err := Validate(in)
		if err != nil {
			t.Fatalf("Validate(%q): %v", in, err)
		}
		second, err := Validate(first)
		if err != nil {
			t.Fatalf("Validate(Validate(%q)) = error %v", in, err)
		}
		if first != second {
			t.Errorf("Validate not idempotent for %q: %q then %q", in, first, second)
		}
	}
}

func TestRandomAlwaysValidates(t *testing.T) {
	// Random re-checks its own output because leetspeak normalization maps
	// 0->o and 1->i, so a chance code really can normalize into a listed word.
	// Ten thousand iterations is enough to catch a regression that removes the
	// re-check while keeping the test fast.
	const iterations = 10_000
	for i := 0; i < iterations; i++ {
		code, err := Random(DefaultLength)
		if err != nil {
			t.Fatalf("Random: %v", err)
		}
		if len(code) != DefaultLength {
			t.Fatalf("Random returned %q of length %d, want %d", code, len(code), DefaultLength)
		}
		canonical, err := Validate(code)
		if err != nil {
			t.Fatalf("Random produced %q which fails Validate: %v", code, err)
		}
		if canonical != code {
			t.Fatalf("Random produced non-canonical %q (canonical %q)", code, canonical)
		}
	}
}

func TestRandomUsesOnlyAlphabet(t *testing.T) {
	for i := 0; i < 1000; i++ {
		code, err := Random(DefaultLength)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range code {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("Random produced %q containing %q, which is not in the alphabet", code, r)
			}
		}
	}
}

func TestRandomDistributionIsUniform(t *testing.T) {
	// A chi-squared goodness-of-fit check. This is the test that catches modulo
	// bias if the alphabet length is ever changed to something that does not
	// divide 256 — the kind of change that looks harmless in review.
	const (
		samples = 200_000
		length  = DefaultLength
	)
	counts := make(map[rune]int, len(Alphabet))
	total := 0
	for i := 0; i < samples/length; i++ {
		code, err := Random(length)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range code {
			counts[r]++
			total++
		}
	}

	k := float64(len(Alphabet))
	expected := float64(total) / k
	chiSq := 0.0
	for _, r := range Alphabet {
		d := float64(counts[r]) - expected
		chiSq += d * d / expected
	}

	// 31 degrees of freedom. The 99.9th percentile is about 61.1; a uniform
	// generator exceeds it roughly once in a thousand runs. Use a generous
	// bound so this does not flake, while still failing decisively on real
	// bias (a 31-character alphabet reduced mod 256 lands far above this).
	const critical = 90.0
	if chiSq > critical {
		t.Errorf("chi-squared = %.2f exceeds %.2f with %d samples; generation looks biased",
			chiSq, critical, total)
		for _, r := range Alphabet {
			t.Logf("  %q: %d (expected %.0f)", r, counts[r], expected)
		}
	}
	if math.IsNaN(chiSq) {
		t.Fatal("chi-squared is NaN")
	}
}

func TestRandomRejectsBadLength(t *testing.T) {
	for _, n := range []int{-1, 0, MinLength - 1, MaxLength + 1} {
		if _, err := Random(n); err == nil {
			t.Errorf("Random(%d) succeeded, want error", n)
		}
	}
}

func TestGenerateSkipsTakenAliases(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	calls := 0

	// Report the first two candidates as taken, then accept.
	taken := func(_ context.Context, candidate string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		seen[candidate] = true
		return calls <= 2, nil
	}

	got, err := Generate(context.Background(), taken)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 3 {
		t.Errorf("checked availability %d times, want 3", calls)
	}
	if _, err := Validate(got); err != nil {
		t.Errorf("Generate returned %q which fails Validate: %v", got, err)
	}
}

func TestGenerateEscalatesLength(t *testing.T) {
	// Everything at the default length is taken; Generate must lengthen rather
	// than give up or spin.
	taken := func(_ context.Context, candidate string) (bool, error) {
		return len(candidate) == DefaultLength, nil
	}
	got, err := Generate(context.Background(), taken)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(got) != DefaultLength+1 {
		t.Errorf("Generate returned length %d, want %d after escalation", len(got), DefaultLength+1)
	}
}

func TestGenerateGivesUpEventually(t *testing.T) {
	everythingTaken := func(_ context.Context, _ string) (bool, error) { return true, nil }
	if _, err := Generate(context.Background(), everythingTaken); err == nil {
		t.Error("Generate succeeded with every alias taken, want error")
	}
}

func TestGeneratePropagatesLookupError(t *testing.T) {
	sentinel := errors.New("database is down")
	failing := func(_ context.Context, _ string) (bool, error) { return false, sentinel }
	_, err := Generate(context.Background(), failing)
	if !errors.Is(err, sentinel) {
		t.Errorf("Generate error = %v, want it to wrap %v", err, sentinel)
	}
	// A lookup failure must not be mistaken for "taken" and silently retried.
}

func TestGenerateHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Generate(ctx, func(context.Context, string) (bool, error) { return true, nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Generate error = %v, want context.Canceled", err)
	}
}

func TestGenerateProducesDistinctAliasesConcurrently(t *testing.T) {
	// Approximates the service-layer collision test: many creators at once,
	// each taking whatever it is handed. Distinctness here comes from entropy,
	// not from coordination; the database unique index is the real guarantee.
	const workers = 50

	var mu sync.Mutex
	claimed := map[string]bool{}
	taken := func(_ context.Context, candidate string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if claimed[candidate] {
			return true, nil
		}
		claimed[candidate] = true
		return false, nil
	}

	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Generate(context.Background(), taken)
		}(i)
	}
	wg.Wait()

	unique := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if unique[results[i]] {
			t.Errorf("duplicate alias %q returned to two workers", results[i])
		}
		unique[results[i]] = true
	}
	if len(unique) != workers {
		t.Errorf("got %d distinct aliases, want %d", len(unique), workers)
	}
}

func TestIsReservedIsCaseInsensitive(t *testing.T) {
	for _, s := range []string{"api", "API", "Api", " api "} {
		if !IsReserved(s) {
			t.Errorf("IsReserved(%q) = false, want true", s)
		}
	}
	if IsReserved("definitely-not-reserved-xyz") {
		t.Error("IsReserved returned true for an unlisted alias")
	}
}

func TestReservedListIsNonEmptyAndCanonical(t *testing.T) {
	list := Reserved()
	if len(list) < 100 {
		t.Errorf("reserved list has %d entries, which looks truncated", len(list))
	}
	for _, entry := range list {
		if entry != strings.ToLower(entry) {
			t.Errorf("reserved entry %q is not lowercase", entry)
		}
		if strings.TrimSpace(entry) != entry {
			t.Errorf("reserved entry %q has surrounding whitespace", entry)
		}
	}
}

// FuzzValidate asserts the two invariants that must hold for every possible
// input: Validate never panics, and anything it accepts is canonical, in
// range, and made only of permitted characters. Those are exactly the
// assumptions the unique index and the cache key depend on.
func FuzzValidate(f *testing.F) {
	seeds := []string{
		"", " ", "a", "ab", "abc", "API", "my-link", "_x_", "logo.png",
		"café", "🎉🎉🎉", "sh1t", strings.Repeat("a", MaxLength+1),
		"a\x00b", "a\nb", "--", "__", "0", "00000000",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		got, err := Validate(input)
		if err != nil {
			if got != "" {
				t.Errorf("Validate(%q) returned %q alongside error %v; want empty on failure", input, got, err)
			}
			var ve *Error
			if !errors.As(err, &ve) {
				t.Errorf("Validate(%q) returned %T, want *alias.Error", input, err)
			}
			return
		}

		if got != Canonical(got) {
			t.Errorf("Validate(%q) = %q, which is not canonical", input, got)
		}
		if n := len([]rune(got)); n < MinLength || n > MaxLength {
			t.Errorf("Validate(%q) = %q of length %d, outside [%d,%d]", input, got, n, MinLength, MaxLength)
		}
		if !isAllowedASCII(got) {
			t.Errorf("Validate(%q) = %q containing disallowed characters", input, got)
		}
		if isSeparator(got[0]) || isSeparator(got[len(got)-1]) {
			t.Errorf("Validate(%q) = %q with an edge separator", input, got)
		}
		if IsReserved(got) {
			t.Errorf("Validate(%q) = %q which is reserved", input, got)
		}
		if IsProfane(got) {
			t.Errorf("Validate(%q) = %q which is profane", input, got)
		}

		// Accepting a value implies accepting it again, unchanged.
		again, err := Validate(got)
		if err != nil || again != got {
			t.Errorf("Validate not idempotent: %q -> %q -> (%q, %v)", input, got, again, err)
		}
	})
}

func BenchmarkValidate(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Validate("my-marketing-link-2026"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRandom(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Random(DefaultLength); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleValidate() {
	canonical, err := Validate("  My-Link  ")
	fmt.Println(canonical, err)
	// Output: my-link <nil>
}
