package control

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/catalogicon"
)

// CatalogIconPattern is the listener-root route of CatalogIconHandler: the
// path a catalog entity's icon names.
const CatalogIconPattern = "/provider/{file}"

// CatalogIconReader returns the icon named name, or fs.ErrNotExist.
type CatalogIconReader func(ctx context.Context, name string) (catalogicon.Icon, error)

// CatalogIconHandler serves an icon from the first reader that has it and
// hands every other request to next, so a file only the UI bundles is still
// served. A reader that fails is logged and skipped: a missing logo must not
// take a page down.
func CatalogIconHandler(next http.Handler, readers ...CatalogIconReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "file")
		contentType, ok := catalogicon.ContentType(name)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		for _, read := range readers {
			icon, err := read(r.Context(), name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				slog.Warn("control: catalog icon unavailable", "name", name, "err", err)
				continue
			}
			h := w.Header()
			h.Set("Content-Type", contentType)
			h.Set("X-Content-Type-Options", "nosniff")
			// An SVG opened as a document can run script; none is allowed.
			h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
			// The name is not content-addressed: a catalog update may replace the file, so the ETag decides.
			h.Set("Cache-Control", "public, max-age=3600")
			h.Set("ETag", `"`+icon.SHA256+`"`)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(icon.Body))
			return
		}
		next.ServeHTTP(w, r)
	}
}
