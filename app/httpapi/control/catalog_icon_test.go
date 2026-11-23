package control

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/catalogicon"
)

func TestCatalogIconHandler(t *testing.T) {
	seeded := catalogicon.New("claude.png", []byte("seeded"))
	bundled := catalogicon.New("kimi.png", []byte("bundled"))
	reader := func(icons ...catalogicon.Icon) CatalogIconReader {
		return func(_ context.Context, name string) (catalogicon.Icon, error) {
			for _, ic := range icons {
				if ic.Name == name {
					return ic, nil
				}
			}
			return catalogicon.Icon{}, fs.ErrNotExist
		}
	}
	broken := func(context.Context, string) (catalogicon.Icon, error) {
		return catalogicon.Icon{}, errors.New("database is down")
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	r := chi.NewRouter()
	r.Get(CatalogIconPattern, CatalogIconHandler(next, broken, reader(seeded), reader(bundled, catalogicon.New("claude.png", []byte("older")))))

	get := func(path string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	for _, c := range []struct {
		name, path, body string
		code             int
	}{
		{"the first reader that has it", "/provider/claude.png", "seeded", http.StatusOK},
		{"a later reader", "/provider/kimi.png", "bundled", http.StatusOK},
		{"no reader has it", "/provider/grok.png", "", http.StatusTeapot},
		{"not an image name", "/provider/notes.txt", "", http.StatusTeapot},
	} {
		rec := get(c.path)
		if rec.Code != c.code || rec.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, rec.Code, rec.Body.String(), c.code, c.body)
		}
	}

	rec := get("/provider/claude.png")
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+seeded.SHA256+`"` {
		t.Errorf("ETag %q", etag)
	}
	if rec := get("/provider/claude.png", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Errorf("revalidation: %d, want 304", rec.Code)
	}
}
