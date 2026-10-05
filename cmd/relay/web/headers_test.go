package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// indexWithThemeScript mirrors the relay-ui index.html: one inline theme
// bootstrap script and one module script loaded from assets/.
const indexWithThemeScript = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
    <link rel="icon" type="image/png" sizes="32x32" href="/favicon-32.png" />
    <link rel="apple-touch-icon" href="/apple-touch-icon.png" />
    <title>Wyolet Relay</title>
    <script>
      (function () {
        var theme = localStorage.getItem("theme");
        var prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
        if (theme === "dark" || (theme !== "light" && prefersDark)) {
          document.documentElement.classList.add("dark");
        }
      })();
    </script>
    <script type="module" crossorigin src="/assets/index-zgDtKnnp.js"></script>
  </head>
  <body>
    <div id="app"></div>
  </body>
</html>
`

// sha256 of the inline script body above, as a browser computes it.
const themeScriptHash = "'sha256-LhDTMNAI1zmDl921+MiA22CmGoFXSG3japbSSnZSl1E='"

func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == name || strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	return ""
}

func TestSPAResponsesCarrySecurityHeaders(t *testing.T) {
	root := fstest.MapFS{
		"index.html":                {Data: []byte(indexWithThemeScript)},
		"assets/index-zgDtKnnp.js":  {Data: []byte("console.log(1)")},
		"assets/mount-JH0S0QOh.css": {Data: []byte("body{}")},
	}
	h := handlerFor(root, "/api", "https://api.relay.example.com/", "https://key@o1.ingest.example.com/2", "https://api.relay.example.com")

	for _, path := range []string{"/", "/models", "/settings/governance", "/assets/index-zgDtKnnp.js"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rr.Code)
		}
		hdr := rr.Header()
		if got := hdr.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, got)
		}
		if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := hdr.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy = %q", path, got)
		}
		csp := hdr.Get("Content-Security-Policy")
		if got := directive(csp, "frame-ancestors"); got != "frame-ancestors 'none'" {
			t.Errorf("%s: %q, want frame-ancestors 'none'", path, got)
		}
		if got := directive(csp, "default-src"); got != "default-src 'self'" {
			t.Errorf("%s: %q", path, got)
		}
		if got := directive(csp, "script-src"); got != "script-src 'self' "+themeScriptHash {
			t.Errorf("%s: %q, want 'self' plus the theme script hash only", path, got)
		}
		if got := directive(csp, "connect-src"); got != "connect-src 'self' https://api.relay.example.com https://o1.ingest.example.com" {
			t.Errorf("%s: %q", path, got)
		}
		if strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows eval: %q", path, csp)
		}
	}
}

func TestSPAPolicyWithoutInlineScripts(t *testing.T) {
	root := fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><script type="module" src="/assets/a.js"></script>`)}}
	csp := contentSecurityPolicy(root, nil)
	if got := directive(csp, "script-src"); got != "script-src 'self'" {
		t.Fatalf("script-src = %q", got)
	}
	if got := directive(csp, "connect-src"); got != "connect-src 'self'" {
		t.Fatalf("connect-src = %q", got)
	}
}
