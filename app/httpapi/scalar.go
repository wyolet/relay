package httpapi

import "net/http"

// ScalarHandler returns an HTML page that renders the OpenAPI spec at
// specURL using Scalar API Reference. Use it to replace huma's default
// Stoplight Elements docs UI: set cfg.DocsPath = "" before
// humachi.New(...) and register this handler on the chi router instead.
func ScalarHandler(title, specURL string) http.HandlerFunc {
	body := `<!doctype html>
<html>
<head>
  <title>` + title + `</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</head>
<body>
  <script id="api-reference" data-url="` + specURL + `"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}
