package httpx

import (
	"bytes"
	"html/template"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/api"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// DocsHandlers serves the API reference: Swagger UI at /docs and the OpenAPI
// document under the API prefix.
//
// Swagger UI rather than a lighter viewer because the plan promised it and its
// try-it-out console genuinely earns its megabyte on a self-hosted product —
// paste an API key, exercise the API from the browser, no curl. The assets are
// vendored and checksum-pinned like htmx; the renderer serves them
// fingerprinted from the same embedded static tree as everything else.
type DocsHandlers struct {
	UI *ui.Renderer
}

// docsCSP relaxes exactly one directive for exactly one page.
//
// Swagger UI is React that writes inline style attributes, which the strict
// policy blocks — rendering it under the app CSP produces an unstyled heap of
// text. So /docs alone gets style-src 'unsafe-inline'. That waiver does not
// extend to scripts: script-src stays 'self', which Swagger UI satisfies as
// long as its initializer lives in a real file rather than the inline <script>
// its stock index.html uses — ours is static/js/docs.js.
const docsCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// docsPage is the whole page. Not a ui template: it shares nothing with the
// dashboard — no layout, no nav, no session — and putting it there would drag
// Tailwind scanning and shell data into a page that needs neither.
var docsPage = template.Must(template.New("docs").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>API reference · LinkCtrl</title>
  <link rel="stylesheet" href="{{.CSS}}">
  <link rel="icon" href="{{.Favicon}}" type="image/svg+xml">
  <script src="{{.Bundle}}" defer></script>
  <script src="{{.Init}}" defer></script>
</head>
<body>
  <div id="swagger-ui" data-spec-url="{{.SpecURL}}"></div>
</body>
</html>
`))

func (d *DocsHandlers) Page(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	err := docsPage.Execute(&buf, map[string]string{
		"CSS":     d.UI.AssetURL("vendor/swagger-ui.css"),
		"Bundle":  d.UI.AssetURL("vendor/swagger-ui-bundle.js"),
		"Init":    d.UI.AssetURL("js/docs.js"),
		"Favicon": d.UI.AssetURL("favicon.svg"),
		"SpecURL": APIPrefix + "/openapi.json",
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Overwrites the strict CSP the middleware already set; headers are not
	// on the wire until the first write, so the last Set wins.
	w.Header().Set("Content-Security-Policy", docsCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// SpecJSON serves the contract in the form tooling asks for.
func (d *DocsHandlers) SpecJSON(w http.ResponseWriter, r *http.Request) {
	body, err := api.SpecJSON()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

// SpecYAML serves the contract as authored.
func (d *DocsHandlers) SpecYAML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(api.SpecYAML())
}
