package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPresentAndServe(t *testing.T) {
	if !Present() {
		t.Skip("no real dist embedded (source build)")
	}
	h := Handler()
	// index fallback for a client route
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "<!doctype html") && !strings.Contains(strings.ToLower(rr.Body.String()), "<!doctype html") {
		t.Fatalf("client route should serve index.html, got %d body=%.80q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Cache-Control"); ct != "no-cache" {
		t.Fatalf("index should be no-cache, got %q", ct)
	}
}

func TestAssetCacheHeaders(t *testing.T) {
	root := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><div id=app></div>")},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
		"favicon.ico":          {Data: []byte("ico")},
	}
	h := handlerFor(root)

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	if cc := get("/assets/app-abc123.js").Header().Get("Cache-Control"); cc != "public, max-age=604800, immutable" {
		t.Errorf("hashed asset Cache-Control = %q, want week-long immutable", cc)
	}
	if cc := get("/favicon.ico").Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("favicon Cache-Control = %q, want 1h", cc)
	}
	for _, p := range []string{"/", "/dashboard"} {
		if cc := get(p).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache (SPA shell)", p, cc)
		}
	}
}
