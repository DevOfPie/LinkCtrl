package ui

// Mail bodies live here, with the pages, and follow the same conventions:
// embedded rather than read from disk, parsed once at boot so a broken template
// fails startup instead of the first send, and stdlib-only. Delivery is
// internal/mail's job; this file only turns data into words.
//
// Plain text, and only plain text. A multipart message with an HTML part is
// where mail acquires its whole hostile-input surface — remote images that
// report when a message was opened, anchor text that disagrees with its href,
// and a rendering engine per client to be wrong about. A text/plain body has no
// active content to make inert in the first place, which is the cheapest
// possible way to satisfy the requirement.
//
// What is left is injection, and it is handled by construction: every value a
// template interpolates goes through inert() first, so a template author cannot
// forget to escape one. text/template does no escaping of its own — unlike the
// html/template used for pages — so relying on the author would mean relying on
// them to remember, once, in every mail this product ever sends.

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"text/template"
)

// textTemplate names text/template's Template where the Renderer struct is
// declared. That file imports html/template as `template`, and a struct cannot
// be split across files, so the alias is what lets the two live together
// without either import being renamed.
type textTemplate = template.Template

// MailValueLimit is the longest an interpolated value may be, in runes.
//
// Truncation rather than refusal: a display name of ten thousand characters is
// somebody probing, and a mail that arrives with the name cut short is a better
// outcome than one that never arrives at all. Long enough that no legitimate
// name, address or URL this product generates comes close.
const MailValueLimit = 512

// subjectPrefix is the header every mail template must open with.
//
// The template file is written to look like the message it produces — a subject
// line, a blank line, then the body — so a reader can tell what lands in
// someone's inbox without knowing anything about this package.
const subjectPrefix = "Subject:"

// loadMail parses every mail template and checks its shape.
//
// Called from New, so a template missing its subject line takes the process
// down at boot rather than surfacing as a send that fails weeks later, when the
// audit-growth threshold is finally crossed.
func (r *Renderer) loadMail() error {
	names, err := fs.Glob(files, "templates/mail/*.txt")
	if err != nil {
		return fmt.Errorf("ui: list mail templates: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("ui: no mail templates were embedded")
	}

	r.mail = make(map[string]*textTemplate, len(names))
	for _, p := range names {
		src, err := fs.ReadFile(files, p)
		if err != nil {
			return fmt.Errorf("ui: read %s: %w", p, err)
		}
		if !strings.HasPrefix(string(src), subjectPrefix+" ") {
			return fmt.Errorf("ui: %s must begin with %q and a blank line after it",
				p, subjectPrefix+" ")
		}
		// Option("missingkey=error"): a mail naming a value its caller did not
		// supply must fail rather than send "<no value>" to a stranger.
		t, err := template.New(path.Base(p)).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return fmt.Errorf("ui: parse %s: %w", p, err)
		}
		name := strings.TrimSuffix(path.Base(p), ".txt")
		r.mail[name] = t
		r.mailNames = append(r.mailNames, name)
	}
	sort.Strings(r.mailNames)
	return nil
}

// MailTemplates lists the parsed mail templates, for the test that renders
// every one of them.
func (r *Renderer) MailTemplates() []string {
	return append([]string(nil), r.mailNames...)
}

// RenderMail turns one template and its data into a subject and a plain-text
// body.
//
// The data is map[string]string rather than a struct or an `any`, and that is
// the load-bearing choice: it lets this function neutralize every value on the
// way in. A struct would put the responsibility back on whoever wrote the
// template.
func (r *Renderer) RenderMail(name string, data map[string]string) (subject, body string, err error) {
	t, ok := r.mail[name]
	if !ok {
		return "", "", fmt.Errorf("ui: no mail template named %q", name)
	}

	safe := make(map[string]string, len(data))
	for k, v := range data {
		safe[k] = inert(v)
	}

	var buf strings.Builder
	if err := t.Execute(&buf, safe); err != nil {
		return "", "", fmt.Errorf("ui: render mail %s: %w", name, err)
	}

	subject, body, ok = strings.Cut(buf.String(), "\n")
	if !ok {
		return "", "", fmt.Errorf("ui: mail %s rendered no body", name)
	}
	subject = strings.TrimSpace(strings.TrimPrefix(subject, subjectPrefix))
	if subject == "" {
		return "", "", fmt.Errorf("ui: mail %s rendered an empty subject", name)
	}
	// The blank line after the subject is part of the file's shape, not part of
	// the body.
	body = strings.TrimLeft(body, "\n")
	if strings.TrimSpace(body) == "" {
		return "", "", fmt.Errorf("ui: mail %s rendered an empty body", name)
	}
	return subject, body, nil
}

// inert makes one untrusted value safe to interpolate into a message.
//
// Three things are removed, and each is a way text alone can still be an attack:
//
//   - C0 controls and DEL. CR and LF are the header-injection vector — a display
//     name of "Bob\r\nBcc: everyone@example.com" is the whole exploit — and the
//     rest are invisible characters in something a person is asked to trust.
//   - Bidirectional formatting characters. An override can make
//     "moc.elpmaxe@bob" render as an address it is not, which is a spoof that
//     survives every amount of escaping because nothing is being escaped.
//   - Anything past MailValueLimit runes.
//
// Removed rather than escaped: there is no encoding layer in a plain-text body
// for an escape to be undone by, so the only honest answer is for the character
// not to be there.
func inert(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	truncated := false
	for _, ch := range s {
		switch {
		case ch < 0x20 || ch == 0x7f:
			// Dropped. Includes CR, LF and NUL.
			continue
		case ch >= 0x200e && ch <= 0x200f, // LRM, RLM
			ch >= 0x202a && ch <= 0x202e, // LRE .. RLO
			ch >= 0x2066 && ch <= 0x2069: // LRI .. PDI
			continue
		}
		if n >= MailValueLimit {
			truncated = true
			break
		}
		b.WriteRune(ch)
		n++
	}
	if truncated {
		// Said out loud. A value that silently stops mid-word reads as a bug in
		// the mail rather than as a value someone made too long.
		return b.String() + "… (truncated)"
	}
	return b.String()
}
