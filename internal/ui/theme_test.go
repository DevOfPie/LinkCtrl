package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// rawPalette matches a Tailwind colour utility that names a palette shade
// instead of a theme token.
//
// The families matter as much as the colours. `fill-` and `stroke-` are in the
// list because the SVG charts are drawn with them, and a sweep that only
// covered backgrounds and text would leave the one part of the UI nobody
// notices is unreadable until they switch themes.
var rawPalette = regexp.MustCompile(
	`\b(bg|text|border|ring|fill|stroke|divide|placeholder|outline|accent|decoration|shadow|from|to|via)-` +
		`(slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|` +
		`indigo|violet|purple|fuchsia|pink|rose|white|black)(-\d{2,3})?(/\d{1,3})?\b`)

// TestTemplatesUseThemeTokensOnly is what holds every later UI milestone to both
// themes.
//
// A raw palette utility is not wrong in light — it is wrong in dark, silently,
// and only for the people who use dark. Nobody building a feature in the light
// theme has a reason to notice, so review is the wrong instrument: this fails
// the build instead.
//
// The scan is over the embedded templates, which is what actually ships, rather
// than over the source directory.
func TestTemplatesUseThemeTokensOnly(t *testing.T) {
	var offenders []string

	err := fs.WalkDir(files, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range rawPalette.FindAllString(line, -1) {
				offenders = append(offenders,
					path+":"+itoa(i+1)+"  "+m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("templates name palette shades instead of theme tokens, so these "+
			"are wrong in the dark theme:\n  %s\n\nUse a semantic token from "+
			"internal/ui/static/css/input.css — surface, raised, sunken, ink, muted, "+
			"subtle, line, accent, ok, warn, danger — or add one there, with its "+
			"contrast figures, if none of them fits.",
			strings.Join(offenders, "\n  "))
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- the cascade ------------------------------------------------------------

// tokenDecl matches a custom-property *declaration* of a theme token, which is
// the colon: `var(--t-surface)` is a reference and must not count, or every
// generated utility in the stylesheet would look like a token block.
var tokenDecl = regexp.MustCompile(`(--t-[a-z0-9-]+)\s*:`)

// cssBlock is one rule in the built stylesheet that declares theme tokens,
// together with the at-rules it is nested inside.
type cssBlock struct {
	chain  []string // outermost prelude first, then the selector
	tokens []string
}

// layer is the cascade context the block lands in: the layer it is inside, or
// the empty string when it is unlayered. This is the thing that has to match
// across blocks, and it is not the same question as specificity.
func (b cssBlock) layer() string {
	for _, p := range b.chain {
		if strings.HasPrefix(p, "@layer") {
			return p
		}
	}
	return ""
}

func (b cssBlock) where() string { return strings.Join(b.chain, " > ") }

// TestThemeTokensShareOneCascadeLayer is the test this milestone shipped
// without, and the reason it shipped a theme no browser applied.
//
// Cascade layers outrank specificity: a normal declaration outside every layer
// beats one inside a layer whatever the selectors say. So light tokens in an
// unlayered `:root` and dark tokens inside `@layer base` is not a near miss —
// it is a stylesheet in which no dark rule can ever win, on either path, and
// every other check passes over it. The templates still name tokens. The
// attribute is still on `<html>`. The contrast arithmetic is still correct
// about values nothing reaches.
//
// So this asserts the relationship itself, over the built app.css rather than
// the source, because the build is what a browser is handed.
func TestThemeTokensShareOneCascadeLayer(t *testing.T) {
	css := builtStylesheet(t)
	blocks := themeTokenBlocks(css)

	if len(blocks) < 2 {
		t.Fatalf("found %d theme-token blocks in %s; expected a light one and at "+
			"least one dark one, so either the parse is wrong or the tokens are gone",
			len(blocks), StylesheetPath)
	}

	// One cascade context, or nothing below matters.
	want := blocks[0].layer()
	for _, b := range blocks[1:] {
		if b.layer() != want {
			t.Errorf("theme tokens are declared in two different cascade contexts:\n"+
				"  %s  in %s\n  %s  in %s\n\n"+
				"Cascade layers beat specificity, so whichever of these is unlayered "+
				"wins unconditionally and the other theme is dead. Put every --t-* "+
				"block in the same context in internal/ui/static/css/input.css.",
				blocks[0].where(), describeLayer(want),
				b.where(), describeLayer(b.layer()))
		}
	}

	// Which block is which. Light is the one that names neither dark path.
	var light *cssBlock
	var dark []cssBlock
	for i, b := range blocks {
		if strings.Contains(b.where(), "dark") {
			dark = append(dark, blocks[i])
		} else if light == nil {
			light = &blocks[i]
		} else {
			t.Errorf("two blocks declare theme tokens without naming a dark path: "+
				"%s and %s", light.where(), b.where())
		}
	}
	if light == nil {
		t.Fatal("no light theme-token block found in the built stylesheet")
	}
	if len(dark) == 0 {
		t.Fatal("no dark theme-token block found in the built stylesheet")
	}

	// Source order is the other half of the cascade. Equal layer, and the dark
	// selectors are more specific than bare `:root`, so the only remaining way
	// to invert the themes is to emit light last.
	lightAt := indexOfBlock(blocks, *light)
	for _, d := range dark {
		if indexOfBlock(blocks, d) < lightAt {
			t.Errorf("%s is emitted before the light block %s; the dark values would "+
				"be overwritten by the light ones at equal specificity",
				d.where(), light.where())
		}
	}

	// A token added to one block and not the others is this defect's next
	// version: it fails silently, in one theme, for the people using it.
	for _, d := range dark {
		for _, name := range light.tokens {
			if !contains(d.tokens, name) {
				t.Errorf("%s is declared for the light theme but not in %s",
					name, d.where())
			}
		}
		for _, name := range d.tokens {
			if !contains(light.tokens, name) {
				t.Errorf("%s is declared in %s but not for the light theme",
					name, d.where())
			}
		}
	}
}

func describeLayer(l string) string {
	if l == "" {
		return "no layer (unlayered)"
	}
	return l
}

func indexOfBlock(blocks []cssBlock, b cssBlock) int {
	for i := range blocks {
		if blocks[i].where() == b.where() {
			return i
		}
	}
	return -1
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// builtStylesheet returns the generated app.css, in source order.
//
// Fatal rather than skipped when it is absent. A skip here would be the same
// mistake in a different costume: a green run over a stylesheet nobody built.
func builtStylesheet(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(files, "static/"+StylesheetPath)
	if err != nil {
		t.Fatalf("cannot read the built stylesheet (%v); run `make css` and rebuild", err)
	}
	return string(b)
}

// themeTokenBlocks finds every rule in the stylesheet that declares a --t-*
// token, in source order, each carrying the at-rules it is nested inside.
func themeTokenBlocks(css string) []cssBlock {
	var out []cssBlock
	collectTokenBlocks(css, nil, &out)
	return out
}

func collectTokenBlocks(css string, chain []string, out *[]cssBlock) {
	for _, c := range topLevelConstructs(css) {
		if !c.braced {
			continue
		}
		inner := append(append([]string(nil), chain...), c.prelude)
		if strings.Contains(c.body, "{") {
			collectTokenBlocks(c.body, inner, out)
			continue
		}
		var tokens []string
		for _, m := range tokenDecl.FindAllStringSubmatch(c.body, -1) {
			tokens = append(tokens, m[1])
		}
		if len(tokens) > 0 {
			*out = append(*out, cssBlock{chain: inner, tokens: tokens})
		}
	}
}

type construct struct {
	prelude string
	body    string
	braced  bool
}

// topLevelConstructs splits a stretch of CSS into its top-level rules and
// at-rules. Quoted strings are skipped so a brace inside a content value cannot
// desynchronise the depth count.
func topLevelConstructs(css string) []construct {
	var out []construct
	depth, start := 0, 0
	prelude := ""
	for i := 0; i < len(css); i++ {
		switch c := css[i]; c {
		case '"', '\'':
			for i++; i < len(css); i++ {
				if css[i] == '\\' {
					i++
					continue
				}
				if css[i] == c {
					break
				}
			}
		case '{':
			if depth == 0 {
				prelude = strings.TrimSpace(css[start:i])
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				out = append(out, construct{
					prelude: prelude,
					body:    css[strings.Index(css[start:i], "{")+start+1 : i],
					braced:  true,
				})
				start = i + 1
			}
		case ';':
			if depth == 0 {
				out = append(out, construct{prelude: strings.TrimSpace(css[start:i])})
				start = i + 1
			}
		}
	}
	return out
}

// The three cookie states, rendered.
//
// The attribute is the whole mechanism: it is why the first response is already
// in the right theme and why there is no correcting script. If it stops being
// emitted, the page still renders and still looks right to whoever is on the
// system default — which is why this is asserted rather than eyeballed.
func TestThemeAttributeRendersForEveryCookieState(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		theme string
		want  string
		// absent is what must NOT appear, so "system" cannot pass by rendering
		// an empty attribute.
		absent string
	}{
		{"no cookie follows the system", "", "<html lang=\"en\" class=\"h-full\">", "data-theme"},
		{"explicit light", "light", `data-theme="light"`, ""},
		{"explicit dark", "dark", `data-theme="dark"`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, ok := pageData(t)["dashboard"].(map[string]any)
			if !ok {
				t.Fatal("the dashboard page data is not a map")
			}
			data["Theme"] = tt.theme
			data["Path"] = "/dashboard"

			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, "dashboard", data); err != nil {
				t.Fatalf("render: %v", err)
			}
			body := rec.Body.String()

			if !strings.Contains(body, tt.want) {
				t.Errorf("rendered <html> does not contain %q", tt.want)
			}
			if tt.absent != "" && strings.Contains(body, tt.absent) {
				t.Errorf("rendered page contains %q with no stored preference; "+
					"with nothing chosen the page must carry no attribute at all "+
					"so prefers-color-scheme decides", tt.absent)
			}
		})
	}
}

// The switcher has to work without JavaScript, and has to offer the way back to
// "system" — a fresh visitor is on it, so a toggle that only swapped light and
// dark would make it unreachable the moment somebody touched the control.
//
// Run against both render sites, from the same partial: the signed-in one on
// account settings and the signed-out one on the sign-in page. The second is
// the one that would break quietly, since a control that reached for account
// context would render there with nothing in it.
func TestThemeSwitcherIsAPlainFormWithAllThreeChoices(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	for _, page := range appearanceControlPages {
		t.Run(page, func(t *testing.T) {
			data, ok := pageData(t)[page].(map[string]any)
			if !ok {
				t.Fatalf("the %s page data is not a map", page)
			}
			data["Theme"] = ""
			data["Path"] = "/" + page

			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, data); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()

			if !strings.Contains(body, `action="/theme"`) || !strings.Contains(body, `method="post"`) {
				t.Error("the appearance control is not a plain form post")
			}
			for _, v := range []string{`value="system"`, `value="light"`, `value="dark"`} {
				if !strings.Contains(body, v) {
					t.Errorf("the appearance control offers no %s", v)
				}
			}
			// The path comes back on the form so the POST can return where it
			// started, including from the login page.
			if !strings.Contains(body, `name="next" value="/`+page+`"`) {
				t.Error("the appearance form does not carry the page it was posted from")
			}
		})
	}
}

// appearanceControlPages names every page that renders the control, and by
// omission every page that must not.
//
// account is where somebody goes looking for a preference; the footer, where
// this shipped, is the one region of a page a person scanning for a setting
// does not read. login is not a duplicate of it: the preference is per-browser
// by design, so it has to be settable before there is an account, and account
// settings need a session.
var appearanceControlPages = []string{"account", "login"}

// TestExactlyOneAppearanceControlPerPage holds the move honest in both
// directions.
//
// The control was rendered by the layout, which every page renders. Adding a
// site inside a page without removing that one gives the account page two
// controls that can disagree about which option looks selected, and leaving the
// layout site in place is the cheapest possible way to "move" something. So the
// count is asserted exactly, on every page, rather than merely "the account page
// has one".
func TestExactlyOneAppearanceControlPerPage(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	want := make(map[string]int, len(appearanceControlPages))
	for _, p := range appearanceControlPages {
		want[p] = 1
	}

	// Each page in the state it is actually served in: login, setup and error
	// carry no identity, the rest carry one. That is the signed-out half of the
	// sweep, and it is the half that matters — the sign-in control renders with
	// no session at all.
	data := pageData(t)

	for _, page := range r.Pages() {
		t.Run(page, func(t *testing.T) {
			d, ok := data[page]
			if !ok {
				t.Fatalf("no test data for page %q", page)
			}
			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, d); err != nil {
				t.Fatalf("render %s: %v", page, err)
			}
			got := strings.Count(rec.Body.String(), `action="/theme"`)
			if got != want[page] {
				t.Errorf("%s renders %d appearance controls, want %d; the control "+
					"belongs to %v and to no other page, and no page may render two",
					page, got, want[page], appearanceControlPages)
			}
		})
	}
}
