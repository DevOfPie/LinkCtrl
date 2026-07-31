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
func TestThemeSwitcherIsAPlainFormWithAllThreeChoices(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data, ok := pageData(t)["dashboard"].(map[string]any)
	if !ok {
		t.Fatal("the dashboard page data is not a map")
	}
	data["Theme"] = ""
	data["Path"] = "/dashboard"

	rec := httptest.NewRecorder()
	if err := r.Render(rec, http.StatusOK, "dashboard", data); err != nil {
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
	// The path comes back on the form so the POST can return where it started,
	// including from the login page.
	if !strings.Contains(body, `name="next" value="/dashboard"`) {
		t.Error("the appearance form does not carry the page it was posted from")
	}
}
