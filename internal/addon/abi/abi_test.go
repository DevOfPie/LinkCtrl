package abi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestVersionStringMatchesItsComponents(t *testing.T) {
	// Two spellings of one number is how they come to differ, and the SDK carries
	// the string while CheckGeneration reasons about the integers.
	want := fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)
	if Version != want {
		t.Fatalf("Version is %q and its components spell %q", Version, want)
	}
}

func TestGenerationIsTheBreakingAxis(t *testing.T) {
	// The rule the deprecation policy states in prose, executed.
	for _, c := range []struct {
		major, minor, want int
	}{
		{0, 1, 1}, // 0.x: the minor breaks, which is SemVer's own rule for major zero
		{0, 7, 7}, //
		{1, 0, 1}, // from 1.0 the major breaks and the minor is additive
		{1, 4, 1}, //
		{2, 0, 2}, //
	} {
		if got := GenerationOf(c.major, c.minor); got != c.want {
			t.Errorf("GenerationOf(%d, %d) = %d, want %d", c.major, c.minor, got, c.want)
		}
	}
	if Generation != GenerationOf(VersionMajor, VersionMinor) {
		t.Errorf("Generation is %d and the rule says %d", Generation, GenerationOf(VersionMajor, VersionMinor))
	}
	if MinimumGeneration < 1 || MinimumGeneration > Generation {
		t.Errorf("MinimumGeneration is %d, which is outside 1..%d", MinimumGeneration, Generation)
	}
}

func TestCheckGenerationRefusesBothDirections(t *testing.T) {
	if err := CheckGeneration(Generation); err != nil {
		t.Errorf("this host's own generation was refused: %v", err)
	}
	if err := CheckGeneration(MinimumGeneration); err != nil {
		t.Errorf("the oldest generation in the window was refused: %v", err)
	}
	if err := CheckGeneration(Generation + 1); !errors.Is(err, ErrTooNew) {
		t.Errorf("a newer generation gave %v, want ErrTooNew", err)
	}
	// The retired branch is not reachable through a manifest today: manifest
	// validation refuses abi_version below 1 and MinimumGeneration is 1, so the
	// only retired value is one no manifest can carry. It is asserted here because
	// the branch is what a closing window will use, and an untested branch is what
	// a closing window will find broken.
	if err := CheckGeneration(MinimumGeneration - 1); !errors.Is(err, ErrRetired) {
		t.Errorf("a retired generation gave %v, want ErrRetired", err)
	}
	// The refusal has to name the numbers, because the operator reading it has to
	// decide whether to upgrade LinkCtrl or rebuild the add-on.
	err := CheckGeneration(Generation + 1)
	for _, want := range []string{fmt.Sprint(Generation + 1), fmt.Sprint(Generation), Version} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

var abiNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestEveryFunctionIsWellFormed(t *testing.T) {
	seenName := map[string]bool{}
	seenGo := map[string]bool{}
	records := map[string]bool{}
	for _, r := range Records {
		records[r.Name] = true
	}

	for _, f := range Functions {
		if !abiNameRe.MatchString(f.Name) {
			t.Errorf("%q is not a usable wasm import name", f.Name)
		}
		if seenName[f.Name] {
			t.Errorf("%q is declared twice", f.Name)
		}
		seenName[f.Name] = true
		if f.Go == "" || seenGo[f.Go] {
			t.Errorf("%q has a missing or duplicate SDK identifier %q", f.Name, f.Go)
		}
		seenGo[f.Go] = true
		if f.Since == "" {
			t.Errorf("%q does not say which ABI version it appeared in", f.Name)
		}
		if !regexp.MustCompile(`^M\d+(\.\d+)?$`).MatchString(f.BackedBy) {
			// Every function names the milestone whose behaviour it is, including the
			// refused ones — that is what lets the mid-phase review read this set
			// against what was actually built, which is m61.md's own mitigation for
			// declared-but-refused rotting into permanently-refused.
			t.Errorf("%q does not name a milestone that backs it, got %q", f.Name, f.BackedBy)
		}
		if strings.TrimSpace(f.Doc) == "" {
			t.Errorf("%q has no documentation, and the SDK's doc comment is generated from it", f.Name)
		}
		if (f.Deprecated != "") != (f.RemovedNotBefore != "") {
			t.Errorf("%q is half-deprecated: a deprecation names the version it may be removed in", f.Name)
		}
		for _, r := range f.Carries {
			if !records[r] {
				t.Errorf("%q carries %q, which is not a record", f.Name, r)
			}
		}

		outs := 0
		for i, p := range f.Params {
			if p.Name == "" || !abiNameRe.MatchString(p.Name) {
				t.Errorf("%q has a parameter named %q", f.Name, p.Name)
			}
			if p.Kind.GoType() == "" {
				t.Errorf("%q's parameter %q has no kind", f.Name, p.Name)
			}
			if !p.Kind.Out() {
				continue
			}
			outs++
			if i != len(f.Params)-1 {
				t.Errorf("%q's out parameter %q is not last, and the convention says it is", f.Name, p.Name)
			}
		}
		if outs > 1 {
			t.Errorf("%q has %d out parameters, and the convention allows one", f.Name, outs)
		}
	}
}

func TestEveryRecordIsCarriedBySomeFunction(t *testing.T) {
	// A record nothing carries is documentation of a payload that does not cross
	// the boundary, and the privacy assertion below would then be checking a claim
	// about nothing.
	for _, r := range Records {
		carried := slices.ContainsFunc(Functions, func(f Function) bool {
			return slices.Contains(f.Carries, r.Name)
		})
		if !carried {
			t.Errorf("record %q is carried by no function", r.Name)
		}
	}
}

// recordInDoc matches the sentence a parameter carrying a record has to write:
// "as an HTTPRequest record". The doc string is what the assertion binds to
// rather than a second field on Param, because the doc is what the generator
// emits into the SDK and into the published table — so a parameter whose
// documented shape and whose declared record disagree is exactly the case this
// catches.
var recordInDoc = regexp.MustCompile(`as an? ([A-Z][A-Za-z0-9]*) record`)

// TestEveryJSONParameterNamesWhatItCarries is the direction the tests above do
// not walk. TestEveryRecordIsCarriedBySomeFunction goes Records → Functions;
// this goes Functions → Records, and without it a parameter may say "as a JSON
// object" and leave what the object contains to whichever milestone implements
// the function. The privacy and credential assertions further down walk Records
// and parameter *names*, so an unenumerated payload is outside both of them by
// construction — no name to blocklist and no fields to check.
//
// It matters most on an out parameter, which is the only direction in which the
// *host* is the party handing something over. session_mint's was documented as
// "what the host minted, as a JSON object": an implementation answering with a
// token, a session id or a cookie inside it would have left every test here
// green, while m65.md cites this file for "never sees a token, a cookie, or the
// session row; asserted by the ABI's surface".
//
// Two ways to satisfy it, and both are declarations: name a record, or mark the
// parameter GuestShaped because the host does not author the JSON.
func TestEveryJSONParameterNamesWhatItCarries(t *testing.T) {
	records := map[string]bool{}
	for _, r := range Records {
		records[r.Name] = true
	}

	named, shaped := 0, 0
	for _, f := range Functions {
		carried := map[string]bool{}
		for _, p := range f.Params {
			m := recordInDoc.FindStringSubmatch(p.Doc)
			switch {
			case m != nil && p.GuestShaped:
				t.Errorf("%s's %q names the record %s and is also marked GuestShaped: a payload "+
					"is one this ABI describes or one it does not, never both", f.Name, p.Name, m[1])
			case m != nil:
				named++
				carried[m[1]] = true
				if !records[m[1]] {
					t.Errorf("%s's %q crosses as a %s and no such record is declared", f.Name, p.Name, m[1])
					continue
				}
				if !slices.Contains(f.Carries, m[1]) {
					t.Errorf("%s carries %s through %q and does not list it in Carries, so nothing "+
						"that walks Carries reaches it", f.Name, m[1], p.Name)
				}
			case strings.Contains(p.Doc, "JSON"):
				shaped++
				if !p.GuestShaped {
					t.Errorf("%s's %q crosses as JSON and says nothing about what is in it: name "+
						"the record it carries, or mark it GuestShaped because the host does not "+
						"author the shape", f.Name, p.Name)
				}
			}
		}
		// The other half of the same pair: Carries is what the record-level
		// assertions reach through, and a name in it that no parameter carries is a
		// record this function does not actually pass.
		for _, r := range f.Carries {
			if !carried[r] {
				t.Errorf("%s lists %s in Carries and no parameter of it is documented as carrying "+
					"one", f.Name, r)
			}
		}
	}

	if named == 0 || shaped == 0 {
		t.Errorf("this test walked %d record-carrying and %d host-unshaped JSON parameters; "+
			"with either at zero it is asserting about nothing", named, shaped)
	}
}

func TestStatusesAreNegativeAndDistinct(t *testing.T) {
	seen := map[Status]string{}
	for _, s := range Statuses {
		if s.Status >= 0 {
			t.Errorf("%s is %d: a status has to be negative, because a success is a length", s.Go, s.Status)
		}
		if other, dup := seen[s.Status]; dup {
			t.Errorf("%s and %s are both %d", s.Go, other, s.Status)
		}
		seen[s.Status] = s.Go
		if !strings.HasPrefix(s.Go, "Err") {
			t.Errorf("%s does not read as an error value in the SDK", s.Go)
		}
	}
	if _, ok := seen[StatusNotAvailable]; !ok {
		// The distinguishable refusal m61.md requires. Without it in this list the
		// SDK has no error value for it and a consumer cannot branch on it.
		t.Error("StatusNotAvailable is not in Statuses, so the SDK exports no error for the refusal")
	}
}

// --- the privacy stance, as a property of the surface -----------------------

// addressish catches the shapes AddressBearing's literal list would miss.
var addressish = regexp.MustCompile(`(^|_)(ip|ips|addr|address|cidr|subnet|host_?ip)(_|$)`)

// TestNoHostFunctionCarriesAClientAddress is m61.md's privacy bullet, and it is
// the fifth inherited-rule collision answered at the boundary rather than by
// auditing somebody else's DDL.
//
// The stance is that no client address is stored anywhere. An add-on with storage
// that is *handed* an address would store it, and nothing in this repository can
// review the add-on's code. So the answer is that the ABI never hands one over:
// an add-on cannot store what it is never handed, and this test is what makes
// that a property of the surface rather than a promise about vigilance.
func TestNoHostFunctionCarriesAClientAddress(t *testing.T) {
	for _, f := range Functions {
		for _, p := range f.Params {
			assertNotAnAddress(t, f.Name+" parameter", p.Name)
		}
	}
	for _, r := range Records {
		for _, f := range r.Fields {
			assertNotAnAddress(t, r.Name+" field", f.Name)
		}
	}
}

func assertNotAnAddress(t *testing.T, where, name string) {
	t.Helper()
	if slices.Contains(AddressBearing, name) {
		t.Errorf("%s %q is a client address: no host function hands an add-on one", where, name)
		return
	}
	if addressish.MatchString(name) {
		t.Errorf("%s %q looks like an address, and the stance is that none crosses this boundary; "+
			"if it truly is not one, the name is still wrong", where, name)
	}
}

// TestARedirectRecordCarriesNoMoreThanClickEventsMay is the other half, and it is
// the falsifiable one: m61.md says a redirect observation carries "at most what
// click_events may carry, prefix-derived and country-level", so the bound is the
// table's own column list, read out of the migration rather than copied here.
func TestARedirectRecordCarriesNoMoreThanClickEventsMay(t *testing.T) {
	columns := clickEventColumns(t)
	for _, forbidden := range []string{"region", "city"} {
		if !slices.Contains(columns, forbidden) {
			t.Fatalf("this test assumes click_events has a %q column and it does not; "+
				"the exclusion below has stopped meaning anything", forbidden)
		}
	}

	clickDerived := 0
	for _, r := range Records {
		if !r.ClickDerived {
			continue
		}
		clickDerived++
		for _, f := range r.Fields {
			if !slices.Contains(columns, f.Name) {
				t.Errorf("%s.%s is not a click_events column, so it is not something a "+
					"redirect observation may carry: %v", r.Name, f.Name, columns)
			}
			// region and city are columns and are still refused. The privacy stance
			// says they resolve transiently and are never stored — handing them to an
			// add-on that owns tables is exactly how they would come to be stored, in
			// a schema this repository does not migrate.
			if f.Name == "region" || f.Name == "city" {
				t.Errorf("%s.%s: the stance is country-level; region and city resolve "+
					"transiently and are never stored, and an add-on with storage is "+
					"where they would stop being transient", r.Name, f.Name)
			}
		}
	}
	if clickDerived == 0 {
		t.Error("no record is marked ClickDerived, so this test asserted nothing; " +
			"the redirect observation record is the one that should be")
	}
}

// clickEventColumns reads the schema instead of trusting a list. Both the
// CREATE TABLE and any later ADD COLUMN, because a column added by a later
// migration widens what an add-on may legitimately be handed and a bound that
// lags the schema fails in the safe direction but for the wrong reason.
func clickEventColumns(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the migrations are not where this test expects them: %v", err)
	}

	create := regexp.MustCompile(`(?s)CREATE TABLE click_events \((.*?)\n\)`)
	column := regexp.MustCompile(`^\s+([a-z][a-z0-9_]*)\s+[a-z]`)
	added := regexp.MustCompile(`ALTER TABLE click_events\s+ADD COLUMN\s+([a-z][a-z0-9_]*)`)

	var columns []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: this repository's own migrations
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if m := create.FindStringSubmatch(text); m != nil {
			for _, line := range strings.Split(m[1], "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") {
					continue
				}
				if c := column.FindStringSubmatch(line); c != nil {
					columns = append(columns, c[1])
				}
			}
		}
		for _, m := range added.FindAllStringSubmatch(text, -1) {
			columns = append(columns, m[1])
		}
	}
	if len(columns) < 10 {
		t.Fatalf("only %d click_events columns were found (%v); the parse has broken and "+
			"this test would pass by finding nothing to compare against", len(columns), columns)
	}
	return columns
}

// cookieField matches any field or parameter name that carries cookies, in
// either direction and whatever it is spelled.
var cookieField = regexp.MustCompile(`(^|_)cookies?(_|$)`)

// TestNoHostFunctionHandsOverACookieOfTheHosts is D232 as a property of the
// surface, and it is the same argument the address test above makes applied to
// the credential: sessions here are server-side and opaque, so the Cookie header
// *is* the session and an add-on handed it verbatim is an add-on that can act as
// whoever is signed in.
//
// Two shipped assertions rest on this and neither is a promise about add-on
// code — m64.md's "it cannot read the cookie" and m65.md's "never sees a token,
// a cookie, or the session row; asserted by the ABI's surface". This test is
// what that citation points at.
func TestNoHostFunctionHandsOverACookieOfTheHosts(t *testing.T) {
	for _, f := range Functions {
		for _, p := range f.Params {
			assertNotACredential(t, f.Name+" parameter", p.Name)
		}
	}

	prefixed := 0
	for _, r := range Records {
		carries := ""
		for _, f := range r.Fields {
			assertNotACredential(t, r.Name+" field", f.Name)
			if f.Name == "cookies" {
				carries = f.Type
			}
		}
		if !r.PrefixedCookies {
			if carries != "" {
				t.Errorf("record %q carries cookies inbound and is not marked PrefixedCookies, "+
					"so nothing asserts which cookies it carries", r.Name)
			}
			continue
		}
		prefixed++
		// Marked and empty-handed is the failure the flag invites: a claim about a
		// field the record stopped having.
		if carries == "" {
			t.Errorf("record %q is marked PrefixedCookies and carries no `cookies` field", r.Name)
		} else if carries != "object" {
			t.Errorf("record %q's `cookies` field is %q: a prefix-filtered set is an object "+
				"keyed by name, and a string is how the whole header would cross", r.Name, carries)
		}
	}
	if prefixed == 0 {
		t.Error("no record is marked PrefixedCookies, so this test asserted nothing about " +
			"cookies; the record carrying a request to an add-on's routes is the one that should be")
	}
}

func assertNotACredential(t *testing.T, where, name string) {
	t.Helper()
	if slices.Contains(CredentialBearing, name) {
		t.Errorf("%s %q is a credential of the host's: no host function hands an add-on one", where, name)
		return
	}
	if !cookieField.MatchString(name) {
		return
	}
	if _, allowed := CookieFields[name]; !allowed {
		t.Errorf("%s %q carries cookies under a name this ABI has no property for; the two "+
			"that are allowed are in CookieFields, each with what makes it safe", where, name)
	}
}
