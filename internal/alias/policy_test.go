package alias

import "testing"

// The zero Policy has to be the safe one, because it is what the package-level
// Validate uses and what a struct literal in a test or a service produces.
func TestZeroPolicyKeepsTheBuiltInProtections(t *testing.T) {
	var p Policy

	if _, err := p.Validate("api"); err == nil {
		t.Error("zero policy accepted a reserved route name")
	}
	if !p.IsReserved("dashboard") {
		t.Error("zero policy does not consult the built-in reserved list")
	}

	// Whatever the profanity list contains, the zero policy must apply it. Taking
	// a term from the list itself keeps this test honest without hardcoding one.
	if profane := someProfaneTerm(t); profane != "" {
		if _, err := p.Validate(profane); err == nil {
			t.Errorf("zero policy accepted %q; the filter defaults to off", profane)
		}
	}
}

func TestReservedExtraAddsToTheBuiltInList(t *testing.T) {
	p := Policy{ReservedExtra: []string{"marketing", "Careers", "  status  "}}

	for _, s := range []string{"marketing", "careers", "status"} {
		if !p.IsReserved(s) {
			t.Errorf("IsReserved(%q) = false; operator additions should apply, "+
				"canonicalized and trimmed", s)
		}
		if _, err := p.Validate(s); err == nil {
			t.Errorf("Validate(%q) accepted an operator-reserved alias", s)
		}
	}

	// Additions extend the list; they never shrink it. Every route the router
	// registers is on the built-in list, and an alias shadowing one of those
	// would take a working page out of service.
	if !p.IsReserved("api") {
		t.Error("operator additions replaced the built-in list instead of extending it")
	}
	if p.IsReserved("something-else-entirely") {
		t.Error("IsReserved returned true for an unlisted alias")
	}
}

func TestReservedExtraIsCaseInsensitiveOnInput(t *testing.T) {
	p := Policy{ReservedExtra: []string{"internal"}}
	for _, s := range []string{"INTERNAL", "Internal", "internal"} {
		if !p.IsReserved(s) {
			t.Errorf("IsReserved(%q) = false, want true", s)
		}
	}
}

func TestProfanityFilterCanBeDisabled(t *testing.T) {
	profane := someProfaneTerm(t)
	if profane == "" {
		t.Skip("profanity list has no term usable as an alias")
	}

	on := Policy{}
	if _, err := on.Validate(profane); err == nil {
		t.Fatalf("Validate(%q) succeeded with the filter on", profane)
	}

	off := Policy{ProfanityDisabled: true}
	got, err := off.Validate(profane)
	if err != nil {
		t.Fatalf("Validate(%q) with the filter off: %v", profane, err)
	}
	if got != Canonical(profane) {
		t.Errorf("Validate returned %q, want the canonical form %q", got, Canonical(profane))
	}
}

// Disabling profanity filtering must not disable anything else. The two lists
// exist for different reasons: one is taste, the other is routing correctness.
func TestDisablingProfanityKeepsReservedWords(t *testing.T) {
	p := Policy{ProfanityDisabled: true, ReservedExtra: []string{"internal"}}

	if _, err := p.Validate("api"); err == nil {
		t.Error("disabling the profanity filter also allowed a reserved route")
	}
	if _, err := p.Validate("internal"); err == nil {
		t.Error("disabling the profanity filter also allowed an operator reservation")
	}
	// And the character rules are untouched.
	if _, err := p.Validate("has.a.dot"); err == nil {
		t.Error("disabling the profanity filter also allowed a dot in an alias")
	}
}

// WellFormed is shape, not policy: it answers "could this be in the database",
// which is what lets the redirect path refuse most junk without a lookup.
func TestWellFormed(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc", true},
		{"launch", true},
		{"a-b_c9", true},

		{"", false},
		{"ab", false},                     // shorter than MinLength
		{string(make([]byte, 65)), false}, // longer than MaxLength

		// The paths that make this worth having. Every browser asks for the first
		// two, and they land on the redirect tree because they are not routes.
		{"favicon.ico", false},
		{"robots.txt", false},
		{"apple-touch-icon-precomposed.png", false},
		{"wp-login.php", false},

		{"has space", false},
		{"CAPS", false}, // canonical form is lowercase; callers fold first
		{"héllo", false},
		{"-leading", false},
		{"trailing_", false},
	}

	for _, tc := range cases {
		if got := WellFormed(tc.in); got != tc.want {
			t.Errorf("WellFormed(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Anything that validates must be well formed, or the redirect path would refuse
// aliases the service allowed someone to create.
func TestEveryValidAliasIsWellFormed(t *testing.T) {
	for _, s := range []string{"abc", "launch", "a-b_c9", "x_9", "many-hyphens-here"} {
		canonical, err := Validate(s)
		if err != nil {
			t.Fatalf("Validate(%q): %v", s, err)
		}
		if !WellFormed(canonical) {
			t.Errorf("Validate accepted %q but WellFormed rejects it; the redirect "+
				"path would 404 a link that exists", canonical)
		}
	}
}

// someProfaneTerm finds a term from the embedded list that is otherwise a valid
// alias, so tests exercise the filter rather than a length or character rule.
func someProfaneTerm(t *testing.T) string {
	t.Helper()
	for term := range profaneWords {
		if len(term) < MinLength || len(term) > MaxLength {
			continue
		}
		if !isAllowedASCII(term) {
			continue
		}
		if IsReserved(term) {
			continue
		}
		return term
	}
	return ""
}
