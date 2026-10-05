// Package web embeds the relay-ui single-page app and serves it from the
// control plane.
//
// The built UI dist is fetched at image-build time (a pinned relay-ui release
// tarball untarred into ./dist; see the Dockerfile and `make ui-fetch`) and
// baked into the binary via go:embed. The embed lives here in the composition
// root rather than under app/ so the http API packages stay asset-free.
//
// Serving is same-origin by design: the UI resolves the control API to
// window.location.origin when VITE_CONTROL_API_URL is unset, so mounting it on
// the control listener needs no runtime config, no CORS, and no cookie
// relaxation. Out of scope: a standalone UI port (deliberately folded onto the
// control plane — disable via RELAY_UI_DISABLE instead).
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Present reports whether a real UI dist was baked in. A source/dev build (or
// any build where ui-fetch did not run) embeds only an empty dist, so the
// handler must not be mounted — index.html absence is the signal.
func Present() bool {
	if _, err := fs.Stat(dist(), "index.html"); err != nil {
		return false
	}
	return true
}

func dist() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// dist is a compile-time embed; Sub only fails on a malformed path.
		panic(err)
	}
	return sub
}

// Handler serves the embedded SPA: real files (assets, favicon, ...) are served
// directly; everything else falls back to index.html so client-side routes
// resolve. Intended to be registered as the control router's NotFound handler,
// after all API operations — matched API paths never reach it. apiURLs are the
// control/inference URLs the UI is configured to call; their origins join the
// CSP's connect-src.
func Handler(apiURLs ...string) http.Handler {
	return handlerFor(dist(), apiURLs...)
}

func handlerFor(root fs.FS, apiURLs ...string) http.Handler {
	files := http.FileServer(http.FS(root))
	csp := contentSecurityPolicy(root, apiURLs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, r, root)
			return
		}
		if f, err := root.Open(p); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				// go:embed files carry no modtime, so FileServer emits no
				// validator (Last-Modified/ETag) — without an explicit policy
				// the browser re-downloads every bundle on every visit. Vite
				// content-hashes everything under assets/, so a week of
				// immutable caching is safe (a release changes the hashes and
				// index.html — no-cache below — points at the new ones);
				// other real files (favicon, manifest) revalidate hourly.
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=3600")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, root)
	})
}

var inlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)

// contentSecurityPolicy builds the SPA policy from the dist it serves: inline
// scripts in index.html (the theme bootstrap) are allowed by hash, so a new UI
// release needs no relay change. style-src keeps 'unsafe-inline' because the
// UI's component library injects <style> elements and style attributes.
func contentSecurityPolicy(root fs.FS, apiURLs []string) string {
	scriptSrc := []string{"'self'"}
	if index, err := fs.ReadFile(root, "index.html"); err == nil {
		for _, m := range inlineScript.FindAllSubmatch(index, -1) {
			if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
				continue
			}
			sum := sha256.Sum256(m[2])
			scriptSrc = append(scriptSrc, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
	}
	connectSrc := []string{"'self'"}
	for _, raw := range apiURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue // relative URLs are same-origin
		}
		origin := u.Scheme + "://" + u.Host
		if !slices.Contains(connectSrc, origin) {
			connectSrc = append(connectSrc, origin)
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scriptSrc, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + strings.Join(connectSrc, " "),
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	b, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The HTML shell must not be cached — asset filenames are content-hashed,
	// but index.html references the current hashes and changes every release.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
