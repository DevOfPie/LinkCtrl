package abi

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// TestTheDocumentedLiveCountIsTheOneThisListHolds.
//
// `docs/SECURITY.md`'s add-on ABI row tells an operator how much of this contract
// is behaviour and how much is a signature that refuses — *N of those functions do
// anything today* — and it tells them how many cost no permission at all.
// `Plan.md` states the same three numbers from the other end, as the limitation
// row an ordering decision is read out of. All of them come off this slice, and
// none of them had anything checking it: SECURITY.md's live count said **eight**
// through the milestone that made it twelve, and Plan.md said **three of eleven
// refuse** and **two cost nothing** when the answers were two of fourteen and
// four.
//
// **Plan.md is in this list, and that is a judgement rather than an oversight
// elsewhere.** internal/audit's sweep excludes `docs/build-notes` because an entry
// quotes what a number was when it was written; Plan.md is not that file. Its
// limitation rows are edited as milestones discharge them, they are written in the
// present tense about the shipped product, and step 1 of the phase loop reads one
// of them to validate a milestone — so a stale count there is read by the process
// that decides what gets built next, which is a consequence somebody meets. It is
// held here rather than swept, because the sweep is a glob over what an operator
// reads and Plan.md is not that either.
//
// **It is no longer only two files, and that is the second correction.** The claim
// this test's name makes is about *the documented count*, and it was checking five
// sentences while `docs/configuration.md` and `docs/addon-abi.md` stated the
// ungated count in untied prose — the second of those outside the generated
// markers, since the generator rewrites the table region and nothing else. A fifth
// ungated function would have reddened SECURITY.md twice and Plan.md three times
// and left both of those reading *four*. They are anchored here now, and
// [TestEveryDocumentedFunctionCountIsTied] is what makes *every site* a checked
// claim rather than a claim about how carefully somebody swept — because this is
// the third time a count in this repository has been asserted complete without
// being one.
func TestTheDocumentedLiveCountIsTheOneThisListHolds(t *testing.T) {
	var live, ungated, refusing int
	for _, f := range Functions {
		if f.Live {
			live++
		} else {
			refusing++
		}
		if f.Requires == "" {
			ungated++
		}
	}
	for _, tc := range anchoredFunctionCounts(t, live, ungated, refusing) {
		src, err := os.ReadFile(filepath.Join(repoRootOf(t), filepath.FromSlash(tc.file)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(flattenCounts(string(src)), tc.want) {
			t.Errorf("%s does not say %q. %s is %d and the sentence stating it has "+
				"stopped saying so", tc.file, tc.want, tc.what, tc.count)
		}
	}
}

// anchoredCount is one sentence in the documentation that states one of these
// counts, with the count already filled in and flattened.
type anchoredCount struct {
	what, file, want string
	count            int
}

// anchoredFunctionCounts is every sentence in the product's documentation that
// says how many functions this ABI has, how many are live, or how many cost no
// permission.
//
// One list, read by two tests, and that is the point: the sweep asks whether every
// occurrence in every document is accounted for, and an occurrence inside one of
// these sentences is accounted for **by that sentence** rather than by a second
// list repeating it. Two lists would drift, and the drift would be an anchored
// sentence quietly exempting itself.
//
// Paths are relative to the repository root, because the sweep walks from there
// and a comparison against `../../../docs/…` would never match.
func anchoredFunctionCounts(t *testing.T, live, ungated, refusing int) []anchoredCount {
	t.Helper()
	total := len(Functions)
	var out []anchoredCount
	for _, tc := range []struct {
		what, file, sentence string
		counts               []int
		// spelling indexes functionCountWords: 0 is the sentence-initial word and 1
		// the mid-sentence one, because these files do not write it the same way.
		spelling int
	}{
		{"the live count", "docs/SECURITY.md",
			"%s of those functions do anything today", []int{live}, 0},
		{"the ungated count", "docs/SECURITY.md",
			"%s functions cost nothing and are ungated deliberately", []int{ungated}, 0},
		{"the refusing count", "Plan.md",
			"and %s of its %s functions still refuse", []int{refusing, total}, 1},
		{"the live count", "Plan.md",
			"so %s are live", []int{live}, 1},
		{"the ungated count", "Plan.md",
			"The %s functions that cost nothing are not %s the host trusts",
			[]int{ungated, ungated}, 1},
		// The two the sweep found and this list did not have. Neither is
		// decorative: configuration.md's is what an operator reads to decide which
		// grants a manifest needs, and addon-abi.md's is what a publisher reads for
		// the same thing from the other side — and the second sits **outside** the
		// generated markers, because the generator rewrites the table region and
		// nothing else, so nothing regenerates it when the list changes.
		{"the ungated count", "docs/configuration.md",
			"%s functions cost nothing and need no declaration", []int{ungated}, 0},
		{"the ungated count", "docs/addon-abi.md",
			"%s functions cost nothing", []int{ungated}, 0},
		// **CHANGELOG's `[Unreleased]` states these counts live, and they were
		// exempted by name.** The exemption list's rationale said *the release
		// history quotes what a number was in the release it describes* — true of
		// internal/audit's five, every one of which sits in `[0.3.0]` or `[0.2.0]`,
		// and false of all four here, which are in `[Unreleased]` and were written
		// by the diff that exempted them. `[Unreleased]` is not history: it is the
		// next release being drafted, so a count in it is a live claim and moves
		// with the list. Exempt by name, these two would have gone stale the moment
		// M66 made `redirect_event_read` live — silently, by construction, while
		// the same numbers in SECURITY.md and Plan.md went red.
		{"the ungated count", "CHANGELOG.md",
			"%s functions cost nothing and are ungated deliberately", []int{ungated}, 0},
		{"the live count", "CHANGELOG.md",
			"%s functions work; the rest are declared and refuse", []int{live}, 0},
	} {
		words := make([]any, 0, len(tc.counts))
		for _, n := range tc.counts {
			spellings, ok := functionCountWords[n]
			if !ok {
				t.Fatalf("%s in %s is %d and there is no spelling for it here. Add one, and "+
					"edit the sentence that states it; a test that accepted an unspelled "+
					"number would be the hand-maintained count again", tc.what, tc.file, n)
			}
			words = append(words, spellings[tc.spelling])
		}
		out = append(out, anchoredCount{
			what:  tc.what,
			file:  tc.file,
			want:  flattenCounts(fmt.Sprintf(tc.sentence, words...)),
			count: tc.counts[0],
		})
	}
	return out
}

// functionCountWords is how docs/SECURITY.md and Plan.md write these numbers: as a
// word, capitalized where it opens a sentence and lower case where it does not.
var functionCountWords = map[int][]string{
	// One arrived at M66, when the refusing count reached it: `template_render` is
	// the last function this ABI declares and this host does not implement.
	1: {"One", "one"},
	2: {"Two", "two"}, 3: {"Three", "three"}, 4: {"Four", "four"},
	5: {"Five", "five"}, 6: {"Six", "six"}, 7: {"Seven", "seven"},
	8: {"Eight", "eight"}, 9: {"Nine", "nine"}, 10: {"Ten", "ten"},
	11: {"Eleven", "eleven"}, 12: {"Twelve", "twelve"},
	13: {"Thirteen", "thirteen"}, 14: {"Fourteen", "fourteen"},
	15: {"Fifteen", "fifteen"}, 16: {"Sixteen", "sixteen"},
	17: {"Seventeen", "seventeen"},
}

// --- the same counts, everywhere they are written ---------------------------

// flattenCounts makes a sentence in a document comparable to one written here:
// where a line wrapped, what emphasis was put on a number, and whether a name was
// in code ticks are none of them the claim.
//
// `// ` goes too, so a Go doc comment flattens the way a Markdown paragraph does
// and sdk/doc.go is read as prose rather than as source.
var countEmphasis = strings.NewReplacer("**", "", "*", "", "`", "", "// ", "")

func flattenCounts(text string) string {
	return strings.Join(strings.Fields(countEmphasis.Replace(text)), " ")
}

// functionNoun is the word this sweep counts. Occurrences are found by looking
// for the noun and then reading backwards, which is [countsOf]'s whole reason for
// existing — see the comment there.
var functionNoun = regexp.MustCompile(`(?i)^functions?$`)

// countAt is one numeric count found in a document: the number, and where the
// whole of "<number> … <noun>" sits in the flattened text.
type countAt struct {
	n          int
	start, end int
}

// countsOf finds every "<number> … <noun>" in flat, with at most countGap words
// between the number and the noun.
//
// **Written as a backwards scan rather than as one regular expression**, and that
// is the repair for a gap the pattern used to have. It matched a number
// immediately in front of the noun, plus the one *"N of those functions"* spelling
// that was hand-written into it, so `Fourteen host functions`, `The ABI has 14
// host functions` and `Twelve audit actions are supported` were all invisible —
// none of which is spelling the count another way, which is the blind spot that
// was written down. They are the shape the sweep claims to find with an ordinary
// adjective in it.
//
// The obvious widening — an optional `\w+\s+` before the noun — is worse than
// the gap it closes, because the regexp engine matches leftmost-first: on *"and
// three functions"* it captures `and`, reads it as not-a-number, and consumes the
// occurrence, so the count behind it is never examined at all. Reading backwards
// from the noun has no such position to lose: each occurrence of the noun is
// considered once, and the nearest number in front of it is the count.
//
// The walk stops at a clause boundary, so a number in one sentence cannot be read
// as a count in the next, and `|` is one of those: these documents are largely
// Markdown tables, and the cell before is not the same clause.
const countGap = 2

func countsOf(flat string, noun *regexp.Regexp, number func(string) (int, bool)) []countAt {
	words := wordSpan.FindAllStringIndex(flat, -1)
	var out []countAt
	for i, at := range words {
		if !noun.MatchString(bareWord(flat[at[0]:at[1]])) {
			continue
		}
		// The span reported is the bare words and not the punctuation they carry,
		// so an occurrence at the end of a sentence still sits inside the phrase
		// that excuses it — `two functions.` was not inside "…the two functions".
		_, nounEnd := bareSpan(flat, at[0], at[1])
		for back := 1; back <= countGap+1 && i-back >= 0; back++ {
			prev := words[i-back]
			word := flat[prev[0]:prev[1]]
			if n, ok := number(bareWord(word)); ok {
				numStart, _ := bareSpan(flat, prev[0], prev[1])
				out = append(out, countAt{n: n, start: numStart, end: nounEnd})
				break
			}
			if endsAClause(word) {
				break
			}
		}
	}
	return out
}

// wordSpan is one whitespace-delimited word of flattened text.
var wordSpan = regexp.MustCompile(`\S+`)

// bareWord strips the punctuation a word carries into the text around it, so
// `(14`, `functions,` and `functions.` are read as what they say. The inner
// hyphen stays: `thirty-eight` is one word and one number.
func bareWord(w string) string {
	return strings.Trim(w, "(),;:.!?\"'“”‘’|[]{}<>—–-")
}

// bareSpan is [start, end) narrowed to what bareWord keeps.
func bareSpan(flat string, start, end int) (int, int) {
	for start < end && bareWord(flat[start:start+1]) == "" {
		start++
	}
	for end > start && bareWord(flat[end-1:end]) == "" {
		end--
	}
	return start, end
}

// endsAClause reports whether a word closes the clause it is in, which is where
// the backwards walk stops.
func endsAClause(w string) bool {
	return strings.ContainsAny(w[len(w)-1:], ".;:!?|")
}

// notThisFunctionCount is every other numeric "N functions" in the documentation,
// with a phrase saying what it counts instead.
//
// Checked in both directions, which is what keeps it from becoming a list of
// things nobody re-reads: an occurrence in neither list fails, and a phrase the
// prose no longer contains fails too, so an exemption cannot outlive the sentence
// it was written for.
var notThisFunctionCount = map[string][]string{
	// **Neither of these is the release history.** Both sit in `[Unreleased]`, and
	// what makes them exemptions is that neither counts the list: one is how many
	// functions a version bump *added*, the other names the two storage calls. The
	// two sentences that did state the live counts moved to the anchors above.
	"CHANGELOG.md": {
		"moves to 0.1.1 for three new functions",
		// The redirect-inline subset's own pair — redirect_decision_read and
		// redirect_answer_write — and not a count of the list.
		"the two functions the class exists for",
		// A pair, and one the sweep could not see until it learned to read past a
		// word: `two host functions` names storage_query and storage_exec.
		"and two host functions to read and write it",
		// M68.5's entry, and it counts what the release *added* rather than the
		// list — one function and one permission. The same shape as the 0.1.1 line
		// above it.
		"through one new host function and one new permission",
	},
	// Pairs, not counts of the list: the two storage functions, the two the
	// session boundary is split into, the two ungated sources.
	"docs/addon-abi.md": {
		"which of the two functions you called is a fact",
		"the repair is underneath those calls rather than in the two functions below",
		"So the two functions have opposite requirements",
		"Two functions, one token",
		// The redirect-inline subset's own pair — redirect_decision_read and
		// redirect_answer_write — and not a count of the list. It replaced *one
		// function it must export* at M66, which stopped being true when the
		// redirect classes gave a module two more exports; the other direction that
		// exemption named is now a table rather than a sentence, so there is no
		// count in it to excuse.
		"and the two functions above",
		// The egress limb's opening sentence (M68.5). It names `network_fetch` and
		// says what is singular about it — that it is the only one that leaves this
		// machine — rather than counting the list, which at that point is seventeen.
		"network_fetch is the one function that leaves this machine",
	},
	// The host functions M66's timing fixture calls, which is what the deadline was
	// measured against and is not a count of the published list.
	"docs/slo.md": {
		"probe six host functions",
	},
	"sdk/doc.go": {
		"The fix is underneath crypto/rand and time.Now, not in the two functions",
	},
	// What M61 shipped, which is a fact about M61 and not about the list today.
	"Plan.md": {
		"publishes the whole contract and implements three functions of it",
		// The same pair as CHANGELOG's, and invisible for the same reason.
		"added the two storage functions",
	},
	// Four sentences using *one function* as an ordinary quantity — a compile
	// overshoot, a distance in an argument, a shared answer, a shared callee. None
	// is a count of this list, and all four were invisible while [spelledNumber]
	// asked whether a word was a number the *anchors* could have spelled.
	"docs/SECURITY.md": {
		// The redirect-inline subset's own pair — redirect_decision_read and
		// redirect_answer_write — and not a count of the list.
		"the two functions the class exists for",
		"overshoots by however long that one function takes",
		"left standing one function away from where the argument against it was made",
		"The two questions are answered by one function",
		"The mint is a third caller of one function",
	},
	// A row in a file-by-file table: one function per claim in checks.mjs.
	"tools/render-verify/README.md": {
		"one function per claim",
	},
}

// TestEveryDocumentedFunctionCountIsTied.
//
// The test above anchors seven sentences. This one is what makes *seven* a
// checked number rather than a claim about how carefully somebody looked — and it
// exists because the alternative has now failed twice in this milestone alone: a
// tie asserted as complete, and two sites outside it both times.
//
// A new sentence quoting one of these counts fails here until whoever wrote it
// decides which it is: a statement of the count, which gets an anchor above, or a
// use of the same word about something else, which gets a line in
// [notThisFunctionCount] saying what it is about. Both are one line and both are
// visible.
//
// What it reads is every Markdown file in the tree plus sdk/doc.go, which is a
// publisher's manual that happens to be a Go comment. The exclusions are named in
// [documentationForCounts].
//
// **It sees a stale count, and that is the direction drift arrives from.** It used
// to build the set of numbers it cared about out of today's correct values and
// skip every occurrence outside it, so `Fourteen functions are declared` failed and
// `Eight functions are live today` — the same claim, wrong — passed, which is an
// assertion that cannot fail in the direction that matters. Every number in front
// of the noun is now examined, whatever it says. The cost is that a sentence
// counting something else has to be named in [notThisFunctionCount] even when its
// number could never be one of these, and that cost is the point: a line saying
// what a number counts is what the value gate was standing in for.
//
// **And a count with a word in the middle is one.** `Fourteen host functions`, `The
// ABI has 14 host functions` — see [countsOf], which is where the scan that finds
// them lives. That was not "spelling the count another way"; it was the shape this
// sweep claims to find with an adjective in it, and it turned up twice in this
// repository's own prose the moment it was looked for.
//
// The blind spot that remains is narrower and is still worth naming: the sweep
// finds a number in front of the noun *functions*, so a sentence that never writes
// the noun — "the ABI has 14 entries", "all but two refuse" — passes. That is a
// bound on the claim rather than a hole in it, and it is written down so the next
// person to widen the claim knows where it stops.
func TestEveryDocumentedFunctionCountIsTied(t *testing.T) {
	var live, ungated, refusing int
	for _, f := range Functions {
		if f.Live {
			live++
		} else {
			refusing++
		}
		if f.Requires == "" {
			ungated++
		}
	}
	// Everything the test above anchors, filled in and flattened, so an occurrence
	// inside an anchored sentence is accounted for by that sentence rather than by
	// a second list saying the same thing.
	anchors := anchoredFunctionCounts(t, live, ungated, refusing)
	anchored := map[string][]string{}
	for _, tc := range anchors {
		anchored[tc.file] = append(anchored[tc.file], tc.want)
	}
	// **Both lists are checked against the sweep before the sweep runs**, which is
	// the shape internal/audit's sibling already had. An anchor or an exemption
	// naming a file the walk does not read is a line nothing checks — and it is
	// silent in the direction that matters, because a file that was renamed took
	// its entry out of the check without anything going red.
	swept := documentationForCounts(t)
	for _, rel := range slices.Sorted(maps.Keys(notThisFunctionCount)) {
		if !slices.Contains(swept, rel) {
			t.Errorf("notThisFunctionCount excuses %q and the sweep does not read that "+
				"file. Delete the entry, or the file moved and the entry moves with it", rel)
		}
	}
	for _, a := range anchors {
		if !slices.Contains(swept, a.file) {
			t.Errorf("anchoredFunctionCounts anchors a sentence in %q and the sweep does "+
				"not read that file, so every other count in it is unexamined", a.file)
		}
	}

	// reached[file+"\x00"+want] is set when the walk actually found the count
	// inside that anchored sentence. See the guard below for why counting
	// occurrences instead was near-vacuous.
	reached := map[string]bool{}
	for _, rel := range swept {
		src, err := os.ReadFile(filepath.Join(repoRootOf(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		flat := flattenCounts(string(src))
		for _, at := range countsOf(flat, functionNoun, spelledNumber) {
			if want, ok := enclosingPhrase(flat, at.start, at.end, anchored[rel]); ok {
				reached[rel+"\x00"+want] = true
				continue
			}
			if _, ok := enclosingPhrase(flat, at.start, at.end,
				flattenedPhrases(notThisFunctionCount[rel])); ok {
				continue
			}
			t.Errorf("%s says %q and nothing says whether that is a count of this ABI's "+
				"functions:\n  …%s…\nAnchor the sentence in "+
				"TestTheDocumentedLiveCountIsTheOneThisListHolds, or name it in "+
				"notThisFunctionCount with a phrase saying what it counts.",
				rel, flat[at.start:at.end], excerptAt(flat, at.start, at.end))
		}
		// An exemption that outlives its sentence stops excusing anything and
		// starts hiding that nobody has re-read what it was written for.
		for _, phrase := range notThisFunctionCount[rel] {
			if !strings.Contains(flat, flattenCounts(phrase)) {
				t.Errorf("%s no longer contains %q, so an exemption is excusing nothing",
					rel, phrase)
			}
			// The sibling sweep's other half, missing here: an exemption whose phrase
			// holds no count of functions at all excuses nothing and hides nothing,
			// and passes silently for as long as nobody re-reads it.
			if len(countsOf(flattenCounts(phrase), functionNoun, spelledNumber)) == 0 {
				t.Errorf("%s excuses %q and that phrase contains no count of functions at "+
					"all, so it excuses nothing and hides nothing", rel, phrase)
			}
		}
	}
	// **A walk that reached nothing would pass every assertion above by reaching
	// none of them**, which is the one failure this test cannot report as one.
	//
	// It used to ask whether the tree-wide occurrence *total* was at least
	// `len(anchored)` — a count of anchored **files**, four of them, against a
	// number in the dozens. Near-vacuous: the walk could have skipped every
	// anchored document and still cleared it on the exempted pairs alone. What is
	// asked now is per anchor, and it is the question that was meant: did the scan
	// find the count inside the sentence this file says states it.
	//
	// Anchors whose sentence holds no countable occurrence are excluded rather than
	// waved through — `so eight are live` states the live count and never writes the
	// noun, which is [TestEveryDocumentedFunctionCountIsTied]'s own documented bound
	// showing up on this side of it. The sentence still has to exist; that is the
	// test above.
	for _, a := range anchors {
		if len(countsOf(a.want, functionNoun, spelledNumber)) == 0 {
			continue
		}
		if !reached[a.file+"\x00"+a.want] {
			t.Errorf("the sweep never found %q in %s, and the sentence is there. The walk "+
				"is not reaching this document, or countsOf can no longer read the shape "+
				"the anchor is written in — either way every other count in the file is "+
				"unexamined", a.want, a.file)
		}
	}
}

// numberWords is how English writes a small number, which is how these documents
// write one.
//
// Bounded at ninety-nine deliberately: an ABI of a hundred functions is a
// different document, and a test that quietly kept working through that is a test
// nobody re-read at the point it mattered.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
	"nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// spelledNumber reads the word in front of "functions" as a number, or says it is
// not one. *the functions*, *every function* and *a function* are all matched by
// the pattern and none of them is a count.
//
// **It does not consult [functionCountWords], and that is the repair.** It used to:
// a word was a number only if some anchored sentence could have spelled it, which
// is a value gate wearing a vocabulary's clothes. Measured on the tree it left
// green, appended to docs/operations.md: `Seventeen functions are declared.`,
// `Twenty functions are declared.` and `Only one function is live.` all passed,
// because 1, 17 and 20 were outside the 2–16 the anchors happened to need — while
// [TestEveryDocumentedFunctionCountIsTied]'s own comment claimed *every number in
// front of the noun is now examined, whatever it says*. The sibling sweep in
// internal/audit reads 1–99 including the hyphenated compounds, so the two copies
// were not the same scan and the weaker one was the one whose decision text
// claimed the repair. `hundred`, `many` and `several` are deliberately not numbers
// here: none of them is a claim a reader can check against a list.
func spelledNumber(word string) (int, bool) {
	if n, err := strconv.Atoi(word); err == nil {
		return n, true
	}
	lower := strings.ToLower(word)
	if n, ok := numberWords[lower]; ok {
		return n, true
	}
	tens, units, hyphenated := strings.Cut(lower, "-")
	if !hyphenated {
		return 0, false
	}
	t, tok := numberWords[tens]
	u, uok := numberWords[units]
	if !tok || !uok || t < 20 || t%10 != 0 || u > 9 {
		return 0, false
	}
	return t + u, true
}

// enclosingPhrase reports which of these phrases has an occurrence containing
// [start, end). Every occurrence, not the first: a phrase appearing twice excuses
// the count inside each of them and nothing between them.
//
// It answers *which* rather than *whether*, so the reach check below can say an
// anchored sentence was found rather than that some number somewhere was.
func enclosingPhrase(flat string, start, end int, phrases []string) (string, bool) {
	for _, p := range phrases {
		if p == "" {
			continue
		}
		for off := 0; off <= len(flat)-len(p); {
			i := strings.Index(flat[off:], p)
			if i < 0 {
				break
			}
			i += off
			if i <= start && end <= i+len(p) {
				return p, true
			}
			off = i + 1
		}
	}
	return "", false
}

func flattenedPhrases(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, flattenCounts(p))
	}
	return out
}

func excerptAt(flat string, start, end int) string {
	const around = 70
	from := max(start-around, 0)
	to := min(end+around, len(flat))
	return flat[from:to]
}

// documentationForCounts is every file this sweep reads, relative to the repo
// root, and the exclusions are written down because an unwritten exception is
// exactly what this test exists over.
//
//   - **docs/build-notes** is the record. Its entries quote what a number was when
//     the entry was written, and an append-only file whose past has to be
//     rewritten when a count moves is not append-only.
//   - **.claude and build output** are not the product's documentation: the first
//     is the build harness's own command files, the rest is gitignored and is not
//     in a clone.
//   - **Go source other than sdk/doc.go** states these numbers beside the list
//     that produces them, where the edit that moves one has the sentence about it
//     already open. That is not the failure this sweep is for.
//
// CHANGELOG.md is *not* excluded, and neither is it in internal/audit's action
// sweep any more — D314 made the two sweeps one shape and this comment used to
// claim the difference. Its entries state these counts and they are exempted by
// phrase, so an entry added for a future release meets this test rather than
// being excused in advance by a
// glob.
func documentationForCounts(t *testing.T) []string {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, ".claude": true, "node_modules": true, "bin": true,
		"dist": true, "tmp": true, "build-notes": true,
	}
	root := repoRootOf(t)
	inTheClone := tracked(t, root)
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !inTheClone[rel] {
			// Untracked or ignored: working state on one machine, absent from every
			// other. See [tracked].
			return nil
		}
		if strings.HasSuffix(rel, ".md") || rel == "sdk/doc.go" {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for documentation: %v", root, err)
	}
	if len(out) < 10 {
		t.Fatalf("swept %d documents; this test is not reading what it thinks", len(out))
	}
	return out
}

// tracked is every path in this repository's index, relative to the repo root and
// slash-separated.
//
// **A gate that reads a gitignored working file is a gate that fails differently
// on every machine.** Both sweeps walked the tree by suffix and by a directory
// skip list, with no tracked-status filter, so `.current-task.md` mentioning
// *forty actions* or `.queue.md` mentioning *fourteen functions* reddened
// `make check` on the machine that happened to hold one while CI stayed green —
// and the exemption route was closed, because the both-directions check would then
// demand an entry for a file that does not exist in a clone. Both of those files
// are gitignored by phase-loop.md's own design, precisely so they cannot affect a
// gate.
//
// Fatal rather than best-effort when git cannot answer: a sweep that silently fell
// back to walking everything would be the machine-dependent gate again, and this
// time with nothing saying so.
func tracked(t *testing.T, root string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("asking git which files %s tracks: %v. This sweep reads the "+
			"documentation a clone has, and it cannot tell that from a working file "+
			"without an answer here", root, err)
	}
	in := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			in[p] = true
		}
	}
	if len(in) == 0 {
		t.Fatalf("git tracks no files under %s; the sweep would read nothing", root)
	}
	return in
}

func repoRootOf(t *testing.T) string {
	t.Helper()
	// internal/addon/abi -> repo root.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate the repository root from %s: %v", root, err)
	}
	return root
}
