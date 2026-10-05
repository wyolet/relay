package httpapi

import (
	"html"
	"net/http"
)

// scalarScript is an exact Scalar release with its SRI hash, the pair huma
// ships for its own Scalar renderer; bump both together.
const (
	scalarScript    = "https://unpkg.com/@scalar/api-reference@1.44.20/dist/browser/standalone.js"
	scalarIntegrity = "sha384-tMz7GAo6dMy55x9tLFtH+sHtogji6Scmb+feBR31TAHmvSPRUTboK9H3M5NFaP4R"
)

// scalarCSP limits the docs page to the pinned script and same-origin
// fetches of the spec. Scalar needs eval and inline styles.
const scalarCSP = "default-src 'none'; base-uri 'none'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; " +
	"img-src 'self' data:; script-src 'unsafe-eval' " + scalarScript + "; style-src 'unsafe-inline'"

// ScalarHandler returns an HTML page that renders the OpenAPI spec at
// specURL using Scalar API Reference. Use it to replace huma's default
// Stoplight Elements docs UI: set cfg.DocsPath = "" before
// humachi.New(...) and register this handler on the chi router instead.
func ScalarHandler(title, specURL string) http.HandlerFunc {
	body := []byte(`<!doctype html>
<html>
<head>
  <title>` + html.EscapeString(title) + `</title>
  <meta charset="utf-8" />
  <meta name="referrer" content="no-referrer" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</head>
<body>
  <script id="api-reference" data-url="` + html.EscapeString(specURL) + `"></script>
  <script src="` + scalarScript + `" integrity="` + scalarIntegrity + `" crossorigin="anonymous"></script>
</body>
</html>`)
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", scalarCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		_, _ = w.Write(body)
	}
}
