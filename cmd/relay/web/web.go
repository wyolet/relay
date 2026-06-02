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
	"embed"
	"io/fs"
	"net/http"
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
// after all API operations — matched API paths never reach it.
func Handler() http.Handler {
	root := dist()
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, r, root)
			return
		}
		if f, err := root.Open(p); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, root)
	})
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
