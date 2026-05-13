package main

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

//go:embed all:web/dist
var uiFS embed.FS

// uiDistFS is the sub-filesystem rooted at web/dist.
// It is nil when the dist directory is empty (developer didn't run make ui-fetch).
var uiDistFS fs.FS

func init() {
	sub, err := fs.Sub(uiFS, "web/dist")
	if err != nil {
		// Should not happen — the embed directive guarantees the dir exists.
		return
	}
	// Check whether index.html exists to detect an empty dist.
	if _, err := fs.Stat(sub, "index.html"); err == nil {
		uiDistFS = sub
	}
}

// uiHandler returns an http.Handler that serves the embedded SPA.
// If the dist directory is empty it returns a friendly 503 with instructions.
func uiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uiDistFS == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("UI not built. Run `make ui-fetch` then rebuild the binary.\n"))
			return
		}

		// Strip the /ui prefix to get the path within the embedded FS.
		urlPath := r.URL.Path
		urlPath = strings.TrimPrefix(urlPath, "/ui")
		if urlPath == "" {
			urlPath = "/"
		}
		// Clean the path and strip leading slash for fs lookup.
		urlPath = path.Clean(urlPath)
		fsPath := strings.TrimPrefix(urlPath, "/")
		if fsPath == "" {
			fsPath = "index.html"
		}

		// Try to serve the exact file.
		f, err := uiDistFS.Open(fsPath)
		if err != nil {
			// SPA fallback: serve index.html for any unmatched path.
			serveEmbeddedFile(w, r, "index.html", "text/html; charset=utf-8")
			return
		}
		stat, statErr := f.Stat()
		_ = f.Close()
		if statErr != nil || stat.IsDir() {
			// Directory — serve index.html (e.g. /ui/ itself).
			serveEmbeddedFile(w, r, "index.html", "text/html; charset=utf-8")
			return
		}

		// Determine MIME type from extension.
		ext := path.Ext(fsPath)
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = "application/octet-stream"
		}
		serveEmbeddedFile(w, r, fsPath, ct)
	})
}

func serveEmbeddedFile(w http.ResponseWriter, _ *http.Request, fsPath, contentType string) {
	data, err := fs.ReadFile(uiDistFS, fsPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// mountUI registers /ui and /ui/* on the given chi router.
// These routes are intentionally outside auth middleware; API endpoints are gated separately (PER-274).
func mountUI(r chi.Router) {
	h := uiHandler()
	// Redirect /ui → /ui/ so relative asset paths work.
	r.Get("/ui", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/ui/", http.StatusMovedPermanently)
	})
	r.Handle("/ui/*", h)
}
