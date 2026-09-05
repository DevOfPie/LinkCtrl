package addon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// --- the boundary is structural, not a rule anybody has to remember ----------

// **Every logger this subsystem hands out neutralizes, and there is no second way
// to make one.** That is D286's claim and this is it as assertions.
//
// The rule it replaces was *neutralize module text where you log it*. It was written
// down three times and enumerated wrongly twice. The second enumeration named three
// sites and missed two: `store.MigrateAddon` logs a migration filename on the
// **success** path, in another package, and the filename pattern admits every code
// point but a newline; and the per-request instantiation failure carries a wazero
// trace built out of the module's own name section. Neither is reachable from a list
// of call sites in `internal/addon`, because one of them is not in `internal/addon`
// and the other did not look like a log site at all.
//
// So the two things asserted here are the two the property rests on: the logger Open
// holds is neutralizing, and nothing in the package constructs a logger that is not.
func TestEveryLoggerThisSubsystemHandsOutNeutralizes(t *testing.T) {
	dir := t.TempDir()
	code := fixture(t, "minimal")
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)
	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	// h.log is what openStorage hands to store.EnsureAddonSchema, store.MigrateAddon
	// and store.OpenAddonDB, and what every hostState derives its own two loggers
	// from. Wrapping it is therefore the whole of the wiring.
	if _, ok := h.log.Handler().(*neutralizingHandler); !ok {
		t.Fatalf("the host's logger is a %T, so anything given it can log what it likes", h.log.Handler())
	}
	st := newHostState(Manifest{Name: "x"}, Grants{}, nil, nil, h.log, nil, false)
	for name, l := range map[string]*slog.Logger{"addon": st.log, "host": st.hostLog} {
		if _, ok := l.Handler().(*neutralizingHandler); !ok {
			t.Errorf("hostState's %s logger is a %T", name, l.Handler())
		}
	}

	// Idempotent, which is what lets newHostState wrap defensively without the
	// escaping being applied twice to everything a test logs.
	once := neutralizingLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if twice := neutralizingLogger(once); twice != once {
		t.Error("wrapping an already-wrapped logger made a second wrapper, so every escape would be doubled")
	}

	// And the other half: no file in this package makes a logger of its own. A site
	// that did would be outside the boundary however carefully it was written, which
	// is precisely the failure mode two rounds of enumeration produced.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "logsafe.go" {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// Four spellings, not one (F311). The scan matched `slog.New(` alone, and a
		// logger comes by three other routes that are just as unwrapped:
		// `slog.Default()`, `slog.With(...)` on the package logger, and
		// `observability.LoggerFrom(ctx)` — which this package already imports, in
		// host.go. A boundary enforced against one construction spelling is enforced
		// against nothing in particular.
		for _, spelling := range []string{
			"slog.New(",
			"slog.Default(",
			"slog.With(",
			"observability.LoggerFrom(",
		} {
			if strings.Contains(string(body), spelling) {
				t.Errorf("%s comes by a logger through %s; every logger in this package "+
					"comes from neutralizingLogger, or the boundary has a hole in it",
					name, spelling)
			}
		}
	}

	// And the packages this one hands a logger to, which the scan above cannot see
	// at all because they are not in this directory (F311). `internal/store`'s
	// exported add-on functions each take a *slog.Logger, and one of them logs a
	// manifest-derived path on its success path — which is where F-1 was found. The
	// property holds today only because internal/addon/host.go is their sole
	// production caller, and nothing asserted that.
	//
	// M67 was the milestone predicted to add a second caller and it landed, so this
	// is checked rather than argued: every call site of those functions, across the
	// whole tree, must be inside this package. A caller elsewhere would hand them a
	// logger this package cannot un-neutralize, and the dependency runs the wrong
	// way for internal/store to protect itself.
	for _, fn := range []string{"EnsureAddonSchema", "MigrateAddon", "PurgeAddonSchema"} {
		out, err := exec.Command("git", "-C", "../..", "grep", "-l", "-F",
			"store."+fn+"(", "--", "*.go").Output()
		if err != nil && len(out) == 0 {
			t.Fatalf("asking git for callers of store.%s: %v", fn, err)
		}
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f == "" || strings.HasPrefix(f, "internal/addon/") {
				continue
			}
			// A test builds its own logger and reads its own output; it is not a
			// production path and neutralizing there would assert nothing. What this
			// is looking for is a second *serving* caller, which is what M67's runtime
			// install and removal made plausible.
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			t.Errorf("%s calls store.%s, which takes a *slog.Logger, and it is outside "+
				"internal/addon — so the logger it hands over has not been through "+
				"neutralizingLogger and nothing in internal/store can put it through one",
				f, fn)
		}
	}
}

// The handler itself, over every shape a record carries. A value class this misses
// is a value class a module's text can ride out on, so the table is over slog's
// kinds rather than over the attributes this package happens to write today.
//
// **Recorded rather than rendered**, and that is not a stylistic choice: slog's text
// handler quotes with strconv, which escapes a `Cf` code point on its own — so a test
// that reads a rendered line cannot tell this handler working from the handler
// underneath tidying up after it, and passes with the neutralization removed. Which
// is the failure this whole milestone is about, so it is not one to reproduce in its
// own test. What is asserted here is what the inner handler is *handed*.
func TestTheNeutralizingHandlerCoversMessageAndEveryAttribute(t *testing.T) {
	// A zero-width space and a right-to-left override: graphic-by-category and
	// invisible, so a search for them is a search for the escaping failing. Plus a
	// newline, which is what closes one record and opens a forged one.
	const hostileText = "before\u200b\u202eafter\nlevel=ERROR"

	for _, tc := range []struct {
		what string
		log  func(l *slog.Logger)
	}{
		{"the message", func(l *slog.Logger) { l.Info(hostileText) }},
		{"a string attribute", func(l *slog.Logger) { l.Info("m", slog.String("k", hostileText)) }},
		{"an attribute key", func(l *slog.Logger) { l.Info("m", slog.String(hostileText, "v")) }},
		{"an error attribute", func(l *slog.Logger) { l.Info("m", slog.Any("error", errors.New(hostileText))) }},
		{"a wrapped error", func(l *slog.Logger) {
			l.Info("m", slog.Any("error", fmt.Errorf("context: %w", errors.New(hostileText))))
		}},
		{"a Stringer", func(l *slog.Logger) { l.Info("m", slog.Any("v", stringerText(hostileText))) }},
		{"a string slice", func(l *slog.Logger) { l.Info("m", slog.Any("v", []string{hostileText})) }},
		{"a group", func(l *slog.Logger) {
			l.Info("m", slog.Group("g", slog.String("k", hostileText)))
		}},
		{"a With attribute", func(l *slog.Logger) { l.With(slog.String("k", hostileText)).Info("m") }},
		{"a group name", func(l *slog.Logger) { l.WithGroup(hostileText).Info("m", slog.String("k", "v")) }},
		{"a LogValuer", func(l *slog.Logger) { l.Info("m", slog.Any("v", valuerText(hostileText))) }},
		// The default limb, which is what stops the sentence at the top of logsafe.go
		// needing an "except": a value the handler underneath would format itself.
		{"anything else", func(l *slog.Logger) {
			l.Info("m", slog.Any("v", struct{ Field string }{hostileText}))
		}},
	} {
		rec := newRecorder()
		tc.log(neutralizingLogger(slog.New(rec)))
		texts := rec.texts()
		if len(texts) == 0 {
			t.Fatalf("%s reached no handler at all", tc.what)
		}
		joined := strings.Join(texts, "\x00")
		for _, bad := range []rune{'\u200b', '\u202e', '\n'} {
			if strings.ContainsRune(joined, bad) {
				t.Errorf("%s handed U+%04X to the handler underneath as itself: %q", tc.what, bad, texts)
			}
		}
		// The escape spellings, named without a backslash so this reads the same
		// whatever quotes it later.
		for _, want := range []string{"u200b", "u202e", `\n`} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s did not arrive escaped; want %s in %q", tc.what, want, texts)
			}
		}
	}
}

type stringerText string

func (s stringerText) String() string { return string(s) }

type valuerText string

func (v valuerText) LogValue() slog.Value { return slog.StringValue(string(v)) }

// recorder is a slog.Handler that keeps every string it is handed — the message,
// every key, every value, every group name — instead of rendering them. It is what
// makes the test above about this boundary rather than about strconv.
type recorder struct {
	shared *recorded
	pre    []slog.Attr
	groups []string
}

type recorded struct {
	mu    sync.Mutex
	texts []string
}

func newRecorder() *recorder { return &recorder{shared: &recorded{}} }

func (r *recorder) texts() []string {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	return append([]string(nil), r.shared.texts...)
}

func (r *recorder) add(s string) {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	r.shared.texts = append(r.shared.texts, s)
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.add(rec.Message)
	for _, g := range r.groups {
		r.add(g)
	}
	for _, a := range r.pre {
		r.attr(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		r.attr(a)
		return true
	})
	return nil
}

func (r *recorder) attr(a slog.Attr) {
	r.add(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		for _, g := range v.Group() {
			r.attr(g)
		}
	case slog.KindAny:
		switch t := v.Any().(type) {
		case error:
			r.add(t.Error())
		case fmt.Stringer:
			r.add(t.String())
		case []string:
			for _, one := range t {
				r.add(one)
			}
		default:
			r.add(fmt.Sprint(t))
		}
	default:
		r.add(v.String())
	}
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recorder{
		shared: r.shared,
		pre:    append(append([]slog.Attr{}, r.pre...), attrs...),
		groups: r.groups,
	}
}

func (r *recorder) WithGroup(name string) slog.Handler {
	return &recorder{
		shared: r.shared,
		pre:    r.pre,
		groups: append(append([]string{}, r.groups...), name),
	}
}

// The other direction: a value that says its text is already neutralized is folded
// and bounded, never escaped a second time. A double escape is not a hole — it is a
// line that reads worse — but the whole reason the marker exists is that a
// `required` add-on's failure has to be readable in two places at once.
func TestAlreadyNeutralizedTextIsFoldedRatherThanEscapedTwice(t *testing.T) {
	// One backslash from the module. Escaped once it is two; escaped twice it is four,
	// which is how this test tells the two apart.
	inner := errors.New(`a\b`)
	safe := neutralize(inner)
	if got := safe.Error(); got != `a\\b` {
		t.Fatalf("neutralize produced %q, want %q", got, `a\\b`)
	}
	if _, ok := safe.(logSafe); !ok {
		t.Fatal("neutralize's result does not carry the marker, so the handler would escape it again")
	}

	sink := &logSink{}
	neutralizingLogger(slog.New(slog.NewTextHandler(sink, nil))).
		Info("m", slog.Any("error", safe))
	got := sink.String()
	if !strings.Contains(got, `a\\b`) {
		t.Errorf("an already-neutralized error did not reach the log escaped once: %q", got)
	}
	if strings.Contains(got, `a\\\\b`) {
		t.Errorf("an already-neutralized error was escaped a second time: %q", got)
	}

	// A LoadError carries the same marker for the same reason: it is printed fatally
	// by cmd/linkctrl and logged here.
	if _, ok := any(&LoadError{}).(logSafe); !ok {
		t.Error("a LoadError does not carry the marker, so logging one escapes its sentence twice")
	}

	// And the fold: the host's own newlines become one record, marked with the lone
	// backslash a module cannot spell.
	if got := foldToLogLine("one\ntwo"); got != `one\ntwo` {
		t.Errorf("folding produced %q, want %q", got, `one\ntwo`)
	}
}

// F-1, at the shape of the site that had it. store.MigrateAddon logs
// `slog.String("name", r.Source.Path)` when a migration **applies** — the success
// path, which is the one that fires when nothing is wrong — and the filename is the
// module's: migrationFileRe admits every code point but a newline. The site is in
// another package and cannot import this one, so what closes it is the logger it is
// handed, which is the host's own.
//
// Driven at that logger's shape rather than through store, because a database is
// what separates this from the integration test that drives the real function
// (TestAMigrationFilenameReachesAnOperatorNeutralized in test/integration). The
// wiring is asserted separately and in the same test: h.log — the logger openStorage
// passes on — is the neutralizing one.
func TestTheLoggerHandedToTheStoreNeutralizesAMigrationFilename(t *testing.T) {
	dir := t.TempDir()
	code := fixture(t, "minimal")
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)
	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	// The host's own logger, with what it wraps swapped for a recorder: what is being
	// asserted is what internal/store's line hands to the handler underneath, and a
	// rendered line would show strconv's quoting as readily as this boundary's work.
	rec := newRecorder()
	const hostile = "0001_\u202eeverything is fine\u200bSECRET=hunter2.sql"
	slog.New(&neutralizingHandler{inner: rec}).Info("add-on migration applied",
		slog.String("addon", "clickstats"),
		slog.String("name", hostile),
		slog.Int64("version", 1))
	got := strings.Join(rec.texts(), "\x00")
	if _, wrapped := h.log.Handler().(*neutralizingHandler); !wrapped {
		t.Fatal("the logger openStorage hands to internal/store is not the neutralizing one, " +
			"so what this asserts is not what that package writes through")
	}
	if strings.ContainsRune(got, '\u202e') || strings.ContainsRune(got, '\u200b') {
		t.Errorf("a migration filename reached an operator with its invisible code points intact: %q", got)
	}
	for _, want := range []string{"u202e", "u200b"} {
		if !strings.Contains(got, want) {
			t.Errorf("the filename did not arrive escaped; want %s in %q", want, got)
		}
	}
}

// F-2, as the property rather than as the site. Route neutralizes at its exit, so
// which of its failures carried the module's text stopped being a question anybody
// has to answer correctly — the instantiation failure it missed is reachable per
// request and not at load, because hostState is registered before InstantiateModule
// and carries the request, so a guest can read that it is answering one and trap
// only then.
func TestEveryErrorOutOfRouteIsNeutralizedAtTheExit(t *testing.T) {
	dir := t.TempDir()
	pages := fixture(t, "pages")
	m := manifestFor("pages", ClassRequired, pages)
	m.Permissions = []string{PermissionRoutes, PermissionSessionContext}
	install(t, dir, m, pages)
	minimal := fixture(t, "minimal")
	mm := manifestFor("minimal", ClassRequired, minimal)
	mm.Permissions = []string{PermissionRoutes}
	install(t, dir, mm, minimal)
	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what   string
		addon  string
		path   string
		sentry error
	}{
		{"an unknown add-on", "absent", "/", ErrNoRoute},
		{"a module exporting no handler", "minimal", "/", ErrNoHandler},
		{"a handler that wrote nothing", "pages", "/nothing", ErrNoResponse},
		{"a handler that refused", "pages", "/refuse", ErrGuestFailed},
	} {
		_, err := h.Route(t.Context(), tc.addon, RequestIn{Method: "GET", Path: tc.path})
		if err == nil {
			t.Errorf("%s answered no error", tc.what)
			continue
		}
		if _, ok := err.(logSafe); !ok { //nolint:errorlint // the exit's own wrapper, by type
			t.Errorf("%s answered a %T, which did not come through Route's neutralizing exit", tc.what, err)
		}
		// Unwrap survives, which is what internal/httpx decides the visitor's page on.
		if !errors.Is(err, tc.sentry) {
			t.Errorf("%s answered %v, and errors.Is no longer reaches its sentinel", tc.what, err)
		}
		// One line. internal/httpx logs this through the *request's* logger, which is
		// not one this package wrapped, so the folding neutralizingHandler would do is
		// not applied out there — and a record boundary is what a newline crosses.
		if strings.ContainsRune(err.Error(), '\n') {
			t.Errorf("%s answered an error carrying a newline, and the logger that prints "+
				"it out of this package does not fold one", tc.what)
		}
	}

	// A nil host answers the same way, so the wrapper is not skipped on the cheap path.
	var none *Host
	if _, err := none.Route(t.Context(), "x", RequestIn{}); !errors.Is(err, ErrNoRoute) {
		t.Errorf("a nil host answered %v, want ErrNoRoute", err)
	}
}

// --- an operator's manifest error list is not a log line ---------------------

// F-5. `neutralize` was `sanitizeLogMessage` under another name, so it brought the
// log line's 4 KiB cap onto the path that prints why a `required` add-on refused to
// load — and Manifest.Validate aggregates every problem with errors.Join precisely
// so that somebody publishing an add-on for the first time sees the whole list.
// Past 4 KiB the list was cut with no sign that it had been, and the newlines
// errors.Join puts between the problems were escaped like a module's own, so what
// was left arrived run-on.
//
// Both are the same mistake — neutralization and length-bounding fused into one
// function — and the split is D286.
func TestAnOperatorsManifestErrorListIsNeitherCutNorRunOn(t *testing.T) {
	// Enough refused settings that the aggregate is comfortably past the log line's
	// bound. Each name is refused for the same reason and each carries its own text.
	m := Manifest{
		SchemaVersion: SchemaVersion,
		Name:          "listy",
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        "listy.wasm",
		SHA256:        strings.Repeat("a", 64),
		FailureClass:  ClassDegrade,
	}
	const problems = 120
	for i := range problems {
		m.Settings = append(m.Settings, Setting{
			Name: fmt.Sprintf("NOT A SETTING NAME %03d \u200b\u202e", i),
			Type: SettingText,
		})
	}
	// One problem that is on its own longer than a log line, because the aggregate
	// being long is not the same claim: neutralizing branch by branch would still cut
	// a single long one if the bound had merely moved rather than gone.
	long := "LONG " + strings.Repeat("x", 6*1024) + " END"
	m.Settings = append(m.Settings, Setting{Name: long, Type: SettingText})
	err := m.Validate()
	if err == nil {
		t.Fatal("a manifest with 120 malformed setting names validated")
	}
	safe := neutralize(err)
	text := safe.Error()

	if len(text) <= maxLogMessage {
		t.Fatalf("the aggregate is %d bytes, which is under the log line's bound — "+
			"this test cannot show the cap was removed", len(text))
	}
	if strings.Contains(text, logTruncated) {
		t.Error("the manifest error list carries the log line's truncation mark, so it was cut")
	}
	if lines := strings.Count(text, "\n") + 1; lines != problems+1 {
		t.Errorf("the list arrived as %d lines and Validate reported %d problems", lines, problems+1)
	}
	// The long one, whole: its own length is what a per-branch bound would have cut.
	if !strings.Contains(text, long) {
		t.Error("a single problem longer than a log line was cut, so the bound only moved")
	}
	// Every problem is still there, first and last, so *the whole list* is a claim
	// about content and not only about length.
	for _, want := range []string{"NOT A SETTING NAME 000", fmt.Sprintf("NOT A SETTING NAME %03d", problems-1)} {
		if !strings.Contains(text, want) {
			t.Errorf("the list does not carry %q", want)
		}
	}
	// Neutralized all the same: the escaping is what the cap was fused to, and only
	// the cap was removed.
	if strings.ContainsRune(text, '\u200b') || strings.ContainsRune(text, '\u202e') {
		t.Error("a setting name reached an operator with its invisible code points intact")
	}
	// %q wrote `\u200b` and the escaping doubled its backslash, so the operator sees
	// `\\u200b`: escaped twice over, which is the safe direction and is what M60's own
	// quoting already did to a printable name.
	if !strings.Contains(text, `\\u200b`) {
		t.Error("the refused names did not arrive escaped")
	}

	// And the same error, logged: one record, bounded, marked as cut. The two
	// destinations want different things and that is the whole of the fix.
	sink := &logSink{}
	neutralizingLogger(slog.New(slog.NewTextHandler(sink, nil))).
		Error("add-on failed to load", slog.Any("error", safe))
	line := sink.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Errorf("the same error produced %d log records", n)
	}
	if !strings.Contains(line, "(truncated)") {
		t.Errorf("the log record was not bounded: %q", line[:min(len(line), 200)])
	}
	if len(line) > 2*maxLogMessage {
		t.Errorf("the log record is %d bytes and the line bound is %d", len(line), maxLogMessage)
	}
}

// TestTheJoinDiscriminationHoldsForATwoVerbWrap pins the one invariant standing
// between a shape heuristic and a forged log record (F314).
//
// [neutralizedErrText] tells an [errors.Join] from `fmt.Errorf`'s two-`%w` form by
// comparing the error's own text against its branches joined by **newlines**. That
// is a heuristic, and it is correct only because `fmt.Errorf` separates with what
// the format string says — here `": "` — and never with a newline. If it ever
// answered *join* for a two-verb wrap, the separator it writes is a raw newline
// into a log record, which is the record boundary this whole subsystem defends.
//
// Nothing drove it before: the handler tests use a single `%w`, and the
// ErrGuestFailed cases they reach are single-verb sites. `internal/addon/http.go`
// has three two-verb sites, and this is the shape they produce.
//
// The remedy this row asked for is a test rather than a change, because the
// behaviour is right. What is asserted is that it stays right.
func TestTheJoinDiscriminationHoldsForATwoVerbWrap(t *testing.T) {
	inner := errors.New("instantiate: out of memory")
	wrapped := fmt.Errorf("%w: instantiate: %w", ErrGuestFailed, inner)

	// The premise: a two-%w error unwraps to a slice, exactly like errors.Join, so
	// the type assertion cannot tell them apart and the text comparison is the
	// whole of the discrimination.
	if _, ok := any(wrapped).(interface{ Unwrap() []error }); !ok {
		t.Fatal("a two-%w error no longer unwraps to a slice; this test's premise is " +
			"gone and neutralizedErrText's heuristic needs re-reading, not this test")
	}

	got := neutralizedErrText(wrapped)
	if strings.Contains(got, "\n") {
		t.Errorf("neutralizedErrText wrote a raw newline into %q. It took the join "+
			"branch for a two-%%w wrap, and a newline is a record boundary: the next "+
			"line of an operator's log is now whatever the module put after it", got)
	}
	if got != wrapped.Error() {
		t.Errorf("neutralizedErrText returned %q for a two-%%w wrap and the error reads "+
			"%q; the leaf path is the correct one here and it should pass the text "+
			"through moduleText unchanged", got, wrapped.Error())
	}

	// And the other side of the discrimination, so this test fails if the branch it
	// is guarding stops being reachable at all: a real Join must still take the
	// join path, which is what puts newlines in deliberately.
	joined := neutralizedErrText(errors.Join(errors.New("one"), errors.New("two")))
	if !strings.Contains(joined, "\n") {
		t.Errorf("errors.Join no longer takes the join branch (%q), so the heuristic "+
			"this test guards has stopped discriminating anything", joined)
	}
}
