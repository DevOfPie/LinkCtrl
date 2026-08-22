package audit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// TestAllActionsIsExhaustive is what makes the audit vocabulary countable.
//
// The list is enumerated outside the code — docs/SECURITY.md states a coverage
// count, and a reader checks that claim by counting. The number has been wrong
// twice: twelve until M32.5 while omitting destination.blocked, and eighteen
// until 0.2.0 while the list had grown past it. Both times a hand-maintained
// number sat beside a list nothing checked, and F18 named the mechanical cause —
// two of the actions were declared in another package entirely, so anything
// enumerating from here was short by two whatever care was taken.
//
// So this parses the source rather than trusting the slice. Adding a constant
// and forgetting AllActions is a failing build here, not a documentation defect
// found two milestones later.
func TestAllActionsIsExhaustive(t *testing.T) {
	src, err := os.ReadFile("audit.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "audit.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	declared := map[string]string{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				declared[name.Name] = v
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("parsed no Action constants; this test is not reading what it thinks")
	}

	listed := AllActions()
	for name, value := range declared {
		if !slices.Contains(listed, value) {
			t.Errorf("%s (%q) is declared and missing from AllActions. Anything that "+
				"counts this vocabulary — docs/SECURITY.md included — is now wrong by "+
				"at least one, which is how that number has been wrong twice already",
				name, value)
		}
	}
	for _, value := range listed {
		if !slices.Contains(slicesValues(declared), value) {
			t.Errorf("AllActions lists %q, which no constant in this file declares", value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("AllActions has %d entries against %d declared constants; a duplicate "+
			"in the list would pass every check above and still make the count wrong",
			len(listed), len(declared))
	}
}

func slicesValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// --- the number outside the code ------------------------------------------------

// countedActions is how the documents that state this number actually write it.
//
// Three files state it and they do not agree on the spelling: README.md writes it
// as a word at the start of a sentence, docs/SECURITY.md as a word mid-sentence,
// docs/data-model.md in digits. So the check is against the spelling each file
// uses, and a count with no entry here fails loudly rather than asserting nothing.
var countedActions = map[int][]string{
	38: {"Thirty-eight", "thirty-eight", "38"},
	39: {"Thirty-nine", "thirty-nine", "39"},
	40: {"Forty", "forty", "40"},
	41: {"Forty-one", "forty-one", "41"},
	42: {"Forty-two", "forty-two", "42"},
}

// anchoredCount is one sentence that states the size of the audit vocabulary,
// with the count written as `%s`.
type anchoredCount struct {
	file, sentence string
	// spelling indexes countedActions: 0 sentence-initial word, 1 mid-sentence
	// word, 2 digits.
	spelling int
}

// theCountIsStatedHere is every sentence in the product's documentation that says
// how large the audit vocabulary is and is tied to it.
//
// **README.md was in this list and has left it** (D313). It was anchored here with
// a comment conceding the collision in advance — *a phase that lands actions and
// folds them into README at the tag finds this red until it does* — and the first
// phase to land one resolved the red by editing README mid-phase, which is exactly
// what D104 exists to prevent: README describes the *released* product, so a count
// including an unreleased action is false for a reader of the tag. The concession
// was the design problem rather than a caveat about it. What the count is tied to
// instead is [frozenUntilTheTag], and the obligation to fold it is written into
// m70.md rather than left to whoever cuts the tag.
var theCountIsStatedHere = []anchoredCount{
	{file: "../../docs/SECURITY.md", sentence: "**Coverage is %s actions**, which is every administrative change", spelling: 1},
	{file: "../../docs/data-model.md", sentence: "%s actions, enumerated by `audit.AllActions`", spelling: 2},
}

// frozenUntilTheTag is a sentence that states this vocabulary's size and is
// deliberately **not** tied to it.
//
// One entry, and it is README's. D313's answer, and its cost is a hole this file
// names rather than absorbs: between now and the release, README's count can be
// stale and nothing here goes red — one of D311's mechanisms carrying a documented
// blind spot on purpose, in the phase whose recurring defect is counts nothing
// checks.
//
// What it still buys is not nothing. The phrase is checked in both directions like
// every other exemption, so the sentence cannot quietly disappear, and the moment
// M70's documentation pass folds the count the entry goes red and has to be
// re-read — which is the one place D104 says README changes. The obligation to do
// the fold is a bullet in docs/build-notes/phase-details/m70.md, with a definition
// of done, rather than a note somebody has to remember.
var frozenUntilTheTag = map[string][]string{
	"../../README.md": {"**Thirty-nine actions are recorded**"},
}

// notThisCount is every other numeric "N actions" in the swept documents, with a
// phrase saying what it is about.
//
// Checked in both directions, which is the half that keeps it honest: an
// occurrence in neither list fails, so a fourth file that starts stating the
// vocabulary's size is caught rather than silently exempt, and a phrase the prose
// no longer contains fails too, so an exemption cannot outlive what it excused.
var notThisCount = map[string][]string{
	"../../README.md":     {"and three actions: notify the owners"},
	"../../docs/usage.md": {"Three actions. A rule may hold at most three"},
	"../../docs/cli.md":   {"eight actions and two people"},
	// The release history, swept for the first time. Each of these is a fact
	// about a release or about a different vocabulary; none is the size of this
	// one, and each is one line rather than the file being waved through.
	"../../CHANGELOG.md": {
		"switching is still one action",
		"the three membership actions and the two instance-level ones",
		"Three actions: an in-app notification",
		"A rule may hold at most three actions",
		"an audit trail spanning eight actions and two people",
	},
	"../../docs/SECURITY.md": {
		"3 actions a rule",
		// Subsets of the vocabulary rather than its size, and both were invisible
		// until the sweep learned to read past a word between the number and the
		// noun: `membership` in one, `of the` in the other. The first phrase
		// appears twice in the file and excuses both occurrences, which is what
		// [encloses] checking every occurrence rather than the first is for.
		"the three membership actions and the two instance-level ones",
		"two of the actions — dispute.allowed and dispute.upheld",
	},
	"../../docs/operations.md": {
		// Not a count of anything: `one action` here is the tail of "at least one
		// action failed", describing an automation firing that partly succeeded.
		"when at least one action failed",
		// The same firing described in the alert table one page down, and an
		// automation rule's actions are not this vocabulary either.
		"at least one of its actions did not complete",
	},
}

// actionNoun is the word this sweep counts. Occurrences are found by looking for
// the noun and reading backwards from it — see [countsOf].
var actionNoun = regexp.MustCompile(`(?i)^actions?$`)

// countAt is one numeric count found in a document: the number, and where the
// whole of "<number> … <noun>" sits in the flattened text.
type countAt struct {
	n          int
	start, end int
}

// countsOf finds every "<number> … <noun>" in flat, with at most countGap words
// between the number and the noun.
//
// **Written as a backwards scan rather than as one regular expression.** The
// pattern it replaces required the number to sit immediately in front of the noun,
// so `Twelve audit actions are supported` — an ordinary adjective away from the
// shape the sweep claims to find — was invisible to it, and so was every other
// sentence that named what kind of actions it was counting.
//
// The obvious widening, an optional `\w+\s+` before the noun, is worse than the
// gap it closes: the regexp engine matches leftmost-first, so on *"and three
// actions"* it captures `and`, reads it as not-a-number, consumes the occurrence,
// and the count behind it is never examined. Reading backwards from the noun loses
// no position — each occurrence of the noun is considered exactly once.
//
// The walk stops at a clause boundary so a number in one sentence is not read as a
// count in the next, and `|` is one of those: these documents are largely Markdown
// tables and the cell before is not the same clause.
//
// This is internal/addon/abi's scan, written out a second time rather than shared.
// Two packages have the same problem and the shared version of it is a mechanism
// of its own, which is the work D311 gives its own milestone.
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
		// that excuses it.
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
// `(40`, `actions,` and `actions.` are read as what they say. The inner hyphen
// stays: `thirty-eight` is one word and one number.
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

// numberWords is how English writes a small number, which is how every one of
// these documents writes one.
//
// Bounded at ninety-nine deliberately: a vocabulary of a hundred actions is a
// different document, and a test that quietly kept working through that is a test
// nobody re-read at the point it mattered.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
	"nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// quantity reads the word in front of "actions" as a number, or says it is not one.
//
// *the actions*, *its actions*, *an action* and *membership actions* are all
// matched by the pattern and none of them is a count, so the sweep asks this
// before it asks anything else. `hundred`, `many` and `several` are deliberately
// not numbers here: none of them is a claim a reader can check against a list.
func quantity(word string) (int, bool) {
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

// TestTheDocumentedActionCountIsTheOneAllActionsHolds.
//
// The test above makes the vocabulary countable. This one makes the count that
// was written down the same number.
//
// It has been wrong three times: twelve until M32.5, eighteen until 0.2.0, and
// thirty-nine in the milestone that made it forty — the last one in two of the
// three files that state it, because the third was edited and the other two were
// not. Every previous fix was to the number. This is a fix to the mechanism, and
// it is the same mechanism internal/store applies to the tables an account
// deletion has to reach.
func TestTheDocumentedActionCountIsTheOneAllActionsHolds(t *testing.T) {
	n := len(AllActions())
	spellings, ok := countedActions[n]
	if !ok {
		t.Fatalf("the vocabulary is %d actions and countedActions has no spelling for it. "+
			"Add one, and edit the sentences below to match; a test that silently "+
			"accepted an unspelled number would be the hand-maintained count again", n)
	}

	for _, a := range theCountIsStatedHere {
		src, err := os.ReadFile(a.file)
		if err != nil {
			t.Fatal(err)
		}
		want := flatten(fmt.Sprintf(a.sentence, spellings[a.spelling]))
		if !strings.Contains(flatten(string(src)), want) {
			t.Errorf("%s does not say %q. The vocabulary is %d actions and this sentence "+
				"has stopped saying so", a.file, want, n)
		}
	}

	// **Both lists are checked against the sweep before the sweep runs.** An
	// exemption naming a file this test does not read excuses nothing, and it is
	// silent in the direction that matters: the loop below used to iterate the
	// swept documents and index `notThisCount` by each one, so an excused file
	// that was renamed or deleted took its exemption out of the check without
	// anything going red — and if the file came back, or a new file took its
	// name, the entry was there again and excusing whatever now matched.
	swept := sweptDocuments(t)
	for _, file := range slices.Sorted(maps.Keys(notThisCount)) {
		if !slices.Contains(swept, file) {
			t.Errorf("notThisCount excuses %q and the sweep does not read that file. An "+
				"exemption outside the sweep is a line nothing checks; delete it, or the "+
				"file moved and the entry moves with it", file)
		}
	}
	for _, a := range theCountIsStatedHere {
		if !slices.Contains(swept, a.file) {
			t.Errorf("theCountIsStatedHere anchors a sentence in %q and the sweep does not "+
				"read that file, so every other count in it is unexamined", a.file)
		}
	}
	for _, file := range slices.Sorted(maps.Keys(frozenUntilTheTag)) {
		if !slices.Contains(swept, file) {
			t.Errorf("frozenUntilTheTag holds a sentence in %q and the sweep does not read "+
				"that file. A count untied from the vocabulary and unread by the sweep is "+
				"tied to nothing at all", file)
		}
	}

	// And nothing else in these documents states a number of actions without
	// having been looked at.
	for _, file := range swept {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		flat := flatten(string(src))
		for _, at := range countsOf(flat, actionNoun, quantity) {
			if coveredBy(flat, at.start, at.end, file, spellings) {
				continue
			}
			t.Errorf("%s says %q and nothing says whether that is the audit vocabulary. "+
				"Anchor the sentence in theCountIsStatedHere, or name it in notThisCount "+
				"with a phrase saying what it counts", file, flat[at.start:at.end])
		}
		for _, phrase := range frozenUntilTheTag[file] {
			if !strings.Contains(flat, flatten(phrase)) {
				t.Errorf("%s no longer contains %q. That sentence is untied from the "+
					"vocabulary on purpose (D313) and this is the one thing still holding "+
					"it: if M70 folded the count, move the spelling here with it; if the "+
					"sentence went, delete the entry", file, phrase)
			}
			if len(countsOf(flatten(phrase), actionNoun, quantity)) == 0 {
				t.Errorf("%s freezes %q and that phrase contains no count of actions at "+
					"all, so it holds nothing", file, phrase)
			}
		}
		for _, phrase := range notThisCount[file] {
			if !strings.Contains(flat, flatten(phrase)) {
				t.Errorf("%s no longer contains %q, so an exemption is excusing nothing. "+
					"An exemption that outlives its sentence is how the next occurrence "+
					"gets quietly excused", file, phrase)
			}
			if len(countsOf(flatten(phrase), actionNoun, quantity)) == 0 {
				t.Errorf("%s excuses %q and that phrase contains no count of actions at "+
					"all, so it excuses nothing and hides nothing", file, phrase)
			}
		}
	}
}

// coveredBy reports whether the occurrence lying at [start, end) in flat has been
// accounted for.
//
// **Position, not text.** It used to ask whether an exempting phrase *contained*
// the occurrence's characters, which made one exemption cover every future
// occurrence of its own substring anywhere in the file: `"eight actions and two
// people"` in docs/cli.md excused the string `eight actions`, so a second, entirely
// unrelated sentence saying *eight actions* was silently excused too — and if the
// vocabulary ever reached eight, the sentence stating it was excused as well. What
// is asked now is whether *this* occurrence sits inside an occurrence of the
// phrase, which is what the exemption was written about.
func coveredBy(flat string, start, end int, file string, spellings []string) bool {
	for _, phrase := range notThisCount[file] {
		if encloses(flat, flatten(phrase), start, end) {
			return true
		}
	}
	for _, phrase := range frozenUntilTheTag[file] {
		if encloses(flat, flatten(phrase), start, end) {
			return true
		}
	}
	for _, a := range theCountIsStatedHere {
		if a.file != file {
			continue
		}
		if encloses(flat, flatten(fmt.Sprintf(a.sentence, spellings[a.spelling])), start, end) {
			return true
		}
	}
	return false
}

// encloses reports whether some occurrence of phrase in flat contains [start, end).
//
// Every occurrence, not the first: an exempting phrase that appears twice excuses
// the count inside each of them and nothing between them.
func encloses(flat, phrase string, start, end int) bool {
	if phrase == "" {
		return false
	}
	for off := 0; off <= len(flat)-len(phrase); {
		i := strings.Index(flat[off:], phrase)
		if i < 0 {
			return false
		}
		i += off
		if i <= start && end <= i+len(phrase) {
			return true
		}
		off = i + 1
	}
	return false
}

// sweptDocuments is every Markdown file this claim is about, plus the one Go
// comment that is documentation.
//
// **It walks the tree and names what it skips.** It used to glob `docs/*.md` and
// add README.md and Plan.md, with a comment saying every exclusion was named and
// six listed — and that glob excluded nine more files nobody had thought about:
// `ci/proposed/README.md`, the three `tools/*/README.md`, and five
// `.claude/commands/*.md`. Measured: `Twelve actions are supported here.` appended
// to `tools/agent-browser/README.md` passed green. Nothing in this repository said
// those files were out; a glob had simply never reached them, and *the exclusions
// are named* was a claim about a list rather than about the walk.
//
// This is the shape internal/addon/abi's function sweep already had, and it is the
// one that survives somebody adding a directory. The skip list is the whole of the
// exclusion, and each entry is a decision:
//
//   - **docs/build-notes** is the record. Its entries quote what a number was when
//     the entry was written, and an append-only file whose past has to be rewritten
//     when a constant moves is not append-only.
//   - **.claude** is the build harness's own tree, and it holds whole worktrees:
//     checkouts of this repository at other commits, whose documents are other
//     commits' claims and not this one's.
//   - **node_modules, bin, dist, tmp** are not written here. `tools/*/node_modules`
//     alone carries three copies of a vendored file stating a count of actions.
//   - **docs/adr and docs/dev-notes** were excluded by the old glob being one level
//     deep and are now swept, which cost no exemption: neither states a count of
//     actions. Being in the walk is what makes that a fact rather than an
//     assumption.
//   - **CLAUDE.md** is swept too, for the same reason — it is the build harness's
//     contract and states no count of actions, and a file stating none costs
//     nothing to read.
//
// **CHANGELOG.md is swept and was not**, and that reverses the argument this
// comment used to make about it. The release-history reasoning is real — *Twelve
// actions* in an entry for 0.1.0 stays a fact about 0.1.0 — but it is an argument
// about individual sentences and it was being spent as a blanket. The function
// sweep in internal/addon/abi has read CHANGELOG since D303 and names three of its
// sentences by phrase, checked both ways, so a *new* entry stating a function count
// goes red there; there was no reason for this vocabulary to be the exception.
//
// It cost five exemptions, which is the argument rather than against it: five
// sentences in CHANGELOG.md state a number of actions and every one of them was
// unexamined. Two are about automation rules, one about a demo fixture, one about
// a workspace switch, and one — *the three membership actions* — is a subset of
// this very vocabulary, sitting in a file nothing swept. The next entry stating
// the vocabulary's size now goes red rather than joining them.
//
// **Plan.md is swept and was not**, for the argument internal/addon/abi's sweep
// makes about the same file: the relevant property is not who reads a file but
// whether its past has to be rewritten when a constant moves. Plan.md's rows are
// edited as milestones discharge them, they are written in the present tense about
// the shipped product, and step 1 of the build loop reads one to decide what gets
// built next. A stale count there is read by the process that schedules work.
//
// **sdk/doc.go is swept**, because a publisher's manual that happens to be a Go
// comment is documentation. It names no audit action, and having the walk say so
// is worth more than a paragraph claiming it.
func sweptDocuments(t *testing.T) []string {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, ".claude": true, "node_modules": true, "bin": true,
		"dist": true, "tmp": true, "build-notes": true,
	}
	root := repoRoot(t)
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
			// Relative to this package, which is what every list in this file
			// spells its files as and what the failures print.
			out = append(out, filepath.ToSlash(filepath.Join("..", "..", rel)))
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

// repoRoot locates the tree this sweep walks, and fails rather than walking
// whatever happens to be two directories up.
func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/audit -> repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate the repository root from %s: %v", root, err)
	}
	return root
}

// flatten removes what wrapping and emphasis did to a sentence, so where a line
// happened to break and where somebody put a bold marker are not part of the
// claim.
//
// **The emphasis half was missing and the comment claimed it anyway**, which is
// the worst arrangement of the two: `**40** actions` — the ordinary way this
// repository writes a number it wants a reader to see — did not match
// `countPattern` at all, so the sweep looked straight past the most likely
// spelling of the thing it exists to find. Applied to the anchored sentences and
// the exemption phrases as well as to the documents, so both sides of every
// comparison are the same text.
var emphasis = strings.NewReplacer("**", "", "*", "", "`", "")

func flatten(s string) string {
	return strings.Join(strings.Fields(emphasis.Replace(s)), " ")
}
