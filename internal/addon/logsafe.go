package addon

import (
	"context"
	"fmt"
	"log/slog"
)

// This file is the one boundary between text a module had a hand in and an
// operator's log.
//
// # Why it is a handler and not a rule
//
// It was a rule, twice, and the rule was wrong both times. M62 neutralized the
// `log` function and wrote down that a module could not put a chosen byte in front
// of a reader; the manifest-validation path was found reaching a log unescaped and
// three sites were fixed and enumerated in five documents; and the enumeration was
// still wrong — a migration filename reached an operator on `store.MigrateAddon`'s
// **success** path, and a per-request instantiation failure carried a wazero trace
// built from names the module's own name section supplies. Each round the fix was
// correct and the *list* was not, because a list of call sites is a claim about
// code that has not been written yet.
//
// So the property moved: **nothing this subsystem logs reaches a handler without
// passing through here**, whoever wrote the call and whichever package it lives in.
// Open wraps the logger it was given before anything else sees it, and every logger
// downstream — the per-add-on one hostState builds, the one openStorage hands to
// internal/store — is derived from that one, so a log call added tomorrow in a file
// nobody has thought of is neutralized without its author knowing this file exists.
// A new site can no longer be missed, because there is no site to miss. D286.
//
// # Why the module's text can be escaped exactly once
//
// The escaping doubles a backslash (D244), so applying it twice is lossy and a
// reader meets `\\\\u200b` where a module wrote a zero-width space. Two things
// keep it to once. Every call site here logs the **raw** value and lets this file
// escape it — no site pre-escapes what it is about to log. And an error whose text
// must already be neutralized because it is *also* printed outside a log —
// [moduleErr] from neutralize, and [LoadError] — says so by implementing logSafe,
// and this file then only folds and bounds it.
//
// A type that carries neutralized text and forgets the marker is escaped twice: a
// line that reads worse, never a line that carries what it should not. That is the
// direction to be wrong in, and it is why the marker means *already safe* rather
// than *needs work*.

// logSafe is implemented by a value whose text has been through escapeModuleText
// already. See the note above on which direction forgetting it fails in.
type logSafe interface{ neutralized() }

// neutralizingLogger returns a logger whose every record is neutralized on its way
// to the handler underneath. Idempotent: wrapping a logger that is already wrapped
// returns it unchanged, so a caller does not have to know whether the one it holds
// came from Open or from a test.
func neutralizingLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	h := l.Handler()
	if _, done := h.(*neutralizingHandler); done {
		return l
	}
	return slog.New(&neutralizingHandler{inner: h})
}

// neutralizingHandler is that wrapper.
type neutralizingHandler struct{ inner slog.Handler }

func (h *neutralizingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle neutralizes the message and every attribute, then hands the record on.
//
// A new record is built rather than the old one edited: slog.Record's attributes
// are not addressable from outside the package, and a record is cloned on every
// fan-out anyway.
func (h *neutralizingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, sanitizeLogMessage(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(safeAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs neutralizes the attributes here rather than at Handle, because the
// handler underneath keeps them and emits them itself: hostState's `addon` and
// `source` pair arrives this way and the add-on's name is the publisher's text like
// any other.
func (h *neutralizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		safe = append(safe, safeAttr(a))
	}
	return &neutralizingHandler{inner: h.inner.WithAttrs(safe)}
}

func (h *neutralizingHandler) WithGroup(name string) slog.Handler {
	return &neutralizingHandler{inner: h.inner.WithGroup(sanitizeLogMessage(name))}
}

func safeAttr(a slog.Attr) slog.Attr {
	return slog.Attr{Key: sanitizeLogMessage(a.Key), Value: safeValue(a.Value)}
}

// safeValue neutralizes everything a log line can be read off, and leaves alone only
// what has no spelling a module chooses: a number, a boolean, a time and a duration
// keep their own types on the way to a JSON handler. Everything else — a string, a
// group, the text of an error or a Stringer, the elements of a string slice, and any
// other value the handler underneath would have formatted itself — is rendered here
// and escaped.
//
// Resolved first, so a slog.LogValuer cannot deliver its text past this.
func safeValue(v slog.Value) slog.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(sanitizeLogMessage(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		safe := make([]slog.Attr, 0, len(attrs))
		for _, a := range attrs {
			safe = append(safe, safeAttr(a))
		}
		return slog.GroupValue(safe...)
	case slog.KindAny:
		switch t := v.Any().(type) {
		case error:
			return slog.StringValue(logLineFor(t))
		case fmt.Stringer:
			return slog.StringValue(sanitizeLogMessage(t.String()))
		case []string:
			safe := make([]string, len(t))
			for i, s := range t {
				safe[i] = sanitizeLogMessage(s)
			}
			return slog.AnyValue(safe)
		default:
			// Anything else the handler underneath would format itself, which is where
			// text would otherwise get past this. Rendered here and escaped instead:
			// the cost is that a struct reaches a JSON handler as a string rather than
			// as an object, and this subsystem logs no such value — while the benefit
			// is that the sentence at the top of this file has no "except" in it.
			return slog.StringValue(sanitizeLogMessage(fmt.Sprint(t)))
		}
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64,
		slog.KindTime, slog.KindUint64, slog.KindLogValuer:
		// Nothing a module chooses the spelling of. Left with their own types, so a
		// JSON handler still writes a number as a number — and KindLogValuer cannot
		// arrive here at all, Resolve having already turned one into what it resolves
		// to.
	}
	return v
}

// logLineFor is one error, as one log record's worth of text.
func logLineFor(err error) string {
	if _, safe := err.(logSafe); safe {
		return foldToLogLine(err.Error())
	}
	return sanitizeLogMessage(err.Error())
}
