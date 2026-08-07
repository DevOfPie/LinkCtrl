// Package ui owns the dashboard's templates, static assets and rendering.
//
// It deliberately depends on nothing but the standard library. Handlers live in
// internal/httpx and pass in whatever data a page needs, so this package can be
// tested by rendering a page and reading the HTML rather than by standing up
// services — and so a template change cannot quietly acquire a dependency on a
// service type.
//
// Everything is embedded, because the deployment unit is one binary. There is
// no filesystem path to get wrong, no volume to forget to mount, and no way for
// the running instance to disagree with the templates it was built from.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// The generated stylesheet is not committed — it is built from these very
// templates — so `all:static` embeds the directory rather than naming the file.
// A build without `make css` therefore succeeds and the missing stylesheet is
// reported at boot by MissingAssets, instead of failing compilation with an
// error about an embed pattern that says nothing about what to do.
//
//go:embed all:templates all:static
var files embed.FS

// StylesheetPath is the asset a build is expected to have generated.
const StylesheetPath = "css/app.css"

// QRThumbClass is the class the link page's QR thumbnail carries, and it is
// exported because the drawing is generated outside this package (M48).
//
// **The box has to be stated in a class rather than left to the picture.** The
// thumbnail sits in the link page's heading row, in front of the destination
// box that M47 measured at 327px from the top of a 1280×800 viewport and that
// M48 re-measured at 349px with this picture beside it, and `internal/qr` sizes
// an `<svg>` from the encoded version — a longer URL is a bigger code, so the
// `width` and `height` attributes are a function of the data. A class is not:
// 6rem is 6rem for every link in the product, which is what makes the height of
// the heading row something the markup states rather than something the data
// decides. TestTheEditControlIsReachableWithoutScrolling is the rule that
// requires it, and decisions.md carries why it is a rule and not an exemption.
//
// Both dimensions, because a QR code is square and a CSS height alone would
// leave the `width` attribute to fight it.
const QRThumbClass = "h-24 w-24"

// Renderer holds the parsed template set and the fingerprinted asset table.
//
// Templates are parsed once at boot. A syntax error therefore fails startup
// rather than the first request that happens to reach that page, which is the
// difference between a deploy that refuses to come up and one that looks
// healthy until someone clicks the wrong tab.
type Renderer struct {
	pages  map[string]*template.Template
	assets map[string]asset
	names  []string
	// Mail bodies, kept in their own set because they are text/template rather
	// than html/template and must not be reachable from a page. See mail.go.
	mail      map[string]*textTemplate
	mailNames []string
}

type asset struct {
	body        []byte
	contentType string
	etag        string
	// url carries the fingerprint as a query parameter, so the response can be
	// cached hard while a new build still busts it.
	url string
}

// New parses every template and fingerprints every static asset.
func New() (*Renderer, error) {
	r := &Renderer{
		pages:  make(map[string]*template.Template),
		assets: make(map[string]asset),
	}

	if err := r.loadAssets(); err != nil {
		return nil, err
	}

	// The shared set: the layout, every partial, and the funcs. Each page is
	// then parsed into a clone, so pages cannot see each other's blocks — two
	// pages defining "content" is the normal case, not a collision.
	base, err := template.New("linkctrl").Funcs(r.funcs()).ParseFS(files,
		"templates/layout.html", "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("ui: parse layout and partials: %w", err)
	}

	pages, err := fs.Glob(files, "templates/pages/*.html")
	if err != nil {
		return nil, fmt.Errorf("ui: list pages: %w", err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("ui: no page templates were embedded")
	}

	for _, p := range pages {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("ui: clone base template: %w", err)
		}
		if _, err := clone.ParseFS(files, p); err != nil {
			return nil, fmt.Errorf("ui: parse %s: %w", p, err)
		}
		name := strings.TrimSuffix(path.Base(p), ".html")
		r.pages[name] = clone
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)

	if err := r.loadMail(); err != nil {
		return nil, err
	}

	return r, nil
}

// Pages lists the parsed page names, for the test that asserts every page
// renders.
func (r *Renderer) Pages() []string { return append([]string(nil), r.names...) }

// MissingAssets reports expected assets that no build produced.
//
// Returned rather than fatal: a stylesheet-less dashboard is ugly but working,
// and refusing to start would turn a forgotten build step into an outage. The
// caller logs it loudly at boot.
func (r *Renderer) MissingAssets() []string {
	var missing []string
	if _, ok := r.assets[StylesheetPath]; !ok {
		missing = append(missing, StylesheetPath)
	}
	return missing
}

// Render writes a full page.
//
// Rendered into a buffer first. Executing straight to the ResponseWriter would
// commit a 200 and a half-written page the moment a template referenced a
// missing field, leaving the browser with truncated HTML and the operator with
// no error to look at.
func (r *Renderer) Render(w http.ResponseWriter, status int, page string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("ui: no such page %q", page)
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("ui: render page %s: %w", page, err)
	}
	return write(w, status, buf.Bytes())
}

// RenderPartial writes one named block, for an HTMX swap.
//
// The block is looked up in the page's own template set, so a partial can use
// anything that page defines. Same buffering rule as Render.
func (r *Renderer) RenderPartial(w http.ResponseWriter, status int, page, block string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("ui: no such page %q", page)
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		return fmt.Errorf("ui: render partial %s/%s: %w", page, block, err)
	}
	return write(w, status, buf.Bytes())
}

func write(w http.ResponseWriter, status int, body []byte) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(body)
	return err
}

// --- static assets ----------------------------------------------------------

func (r *Renderer) loadAssets() error {
	return fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := files.ReadFile(p)
		if err != nil {
			return fmt.Errorf("ui: read asset %s: %w", p, err)
		}

		name := strings.TrimPrefix(p, "static/")
		// input.css is the Tailwind entry point, an input to the build rather
		// than something a browser should ever fetch.
		if name == "css/input.css" {
			return nil
		}

		sum := sha256.Sum256(body)
		fingerprint := base64.RawURLEncoding.EncodeToString(sum[:8])
		r.assets[name] = asset{
			body:        body,
			contentType: contentTypeFor(name),
			etag:        `"` + fingerprint + `"`,
			url:         "/static/" + name + "?v=" + fingerprint,
		}
		return nil
	})
}

// contentTypeFor is explicit rather than relying on mime.TypeByExtension, whose
// answer depends on the host's registry — on Windows a mangled registry entry
// has been known to serve .js as text/plain, which browsers refuse to execute.
func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// AssetURL returns the fingerprinted URL for an asset, or the plain path if the
// asset is absent — a missing stylesheet should 404 visibly rather than render
// as an empty href.
func (r *Renderer) AssetURL(name string) string {
	if a, ok := r.assets[name]; ok {
		return a.url
	}
	return "/static/" + name
}

// StaticHandler serves the embedded assets.
//
// Served from memory with a strong ETag and a one-year max-age. The long
// lifetime is safe because every URL the templates emit carries a content
// fingerprint: a new build changes the URL, so nothing can be served stale.
// Requests without the fingerprint still work and still validate.
func (r *Renderer) StaticHandler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(req.URL.Path, prefix)
		a, ok := r.assets[name]
		if !ok {
			http.NotFound(w, req)
			return
		}

		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("ETag", a.etag)
		if req.URL.Query().Get("v") != "" {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// No fingerprint, so the URL cannot be trusted to change. Revalidate.
			h.Set("Cache-Control", "public, max-age=300")
		}

		if match := req.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if req.Method == http.MethodHead {
			h.Set("Content-Length", fmt.Sprint(len(a.body)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(w, bytes.NewReader(a.body))
	})
}
