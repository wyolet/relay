package catalogserver

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

func TestHTTP_HeadersAndConditionalRequests(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	data, cat := src.release("v0.1.0", "Rocket", oldCommit)
	rel.publishBoth("v0.1.0", data, cat)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	const index = "channels:\n    v1alpha2:\n        latest: v0.1.0\n"
	sum := string(SumFile(CatalogAsset, cat))
	etag := func(b string) string { return `"` + sha256Hex([]byte(b)) + `"` }
	served := func(body, contentType, cacheControl string) answer {
		return answer{Code: http.StatusOK, Body: body, Header: map[string]string{
			"Content-Type": contentType, "Content-Encoding": "", "Content-Length": strconv.Itoa(len(body)),
			"Cache-Control": cacheControl, "ETag": etag(body), "Location": "", "Last-Modified": "",
		}}
	}
	notModified := func(of answer) answer {
		of.Code, of.Body = http.StatusNotModified, ""
		of.Header["Content-Type"], of.Header["Content-Length"] = "", ""
		return of
	}
	redirect := func(to string) answer {
		return answer{Code: http.StatusFound, Body: "<a href=\"" + to + "\">Found</a>.\n\n", Header: map[string]string{
			"Content-Type": "text/html; charset=utf-8", "Content-Encoding": "", "Content-Length": "",
			"Cache-Control": "public, max-age=60", "ETag": "", "Location": to, "Last-Modified": "",
		}}
	}
	for _, c := range []struct {
		name, path string
		request    []string
		want       answer
	}{
		{"index", "/index.yaml", nil, served(index, "application/yaml", shortCache)},
		{"index revalidated", "/index.yaml", []string{"If-None-Match", etag(index)}, notModified(served(index, "", shortCache))},
		// Clients check the sha256 of the stored bytes, so nothing is re-encoded.
		{"catalog", "/releases/v0.1.0/catalog.json.gz", []string{"Accept-Encoding", "gzip"}, served(string(cat), "application/gzip", immutableCache)},
		{"catalog revalidated", "/releases/v0.1.0/catalog.json.gz", []string{"If-None-Match", etag(string(cat))}, notModified(served(string(cat), "", immutableCache))},
		{"data", "/releases/v0.1.0/relay-catalog-data-v0.1.0.tar.gz", nil, served(string(data), "application/gzip", immutableCache)},
		{"catalog sum", "/releases/v0.1.0/catalog.json.gz.sha256", nil, served(sum, "text/plain; charset=utf-8", immutableCache)},
		{"channel catalog", "/channels/v1alpha2/latest/catalog.json.gz", nil, redirect("/releases/v0.1.0/catalog.json.gz")},
		{"channel data", "/channels/v1alpha2/latest/relay-catalog-data.tar.gz", nil, redirect("/releases/v0.1.0/relay-catalog-data-v0.1.0.tar.gz")},
		{"channel data sum", "/channels/v1alpha2/latest/relay-catalog-data.tar.gz.sha256", nil, redirect("/releases/v0.1.0/relay-catalog-data-v0.1.0.tar.gz.sha256")},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantAnswer(t, "answer", c.want, get(t, s, c.path, c.request...))
		})
	}
}

func TestHTTP_NotFound(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	src.release("v0.1.1", "Rocket 2", newCommit)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	// A file that may be published later must not be cached as missing; a
	// path that can never name a file may.
	const later, never = "no-store", ""
	for path, cacheControl := range map[string]string{
		"/releases/v9.9.9/catalog.json.gz":                           later,
		"/releases/v0.1.1/catalog.json.gz":                           later,
		"/releases/v0.1.1/catalog.json.gz.sha256":                    later,
		"/releases/latest/catalog.json.gz":                           never,
		"/releases/v0.1/catalog.json.gz":                             never,
		"/releases/0.1.0/catalog.json.gz":                            never,
		"/releases/v01.1.0/catalog.json.gz":                          never,
		"/releases/v0.1.0-rc.1/catalog.json.gz":                      never,
		"/releases/v0.1.0+meta/catalog.json.gz":                      never,
		"/releases/..%2Fv0.1.0/catalog.json.gz":                      never,
		"/releases/v0.1.0/catalog.json":                              never,
		"/releases/v0.1.0/relay-catalog-data.tar.gz":                 never,
		"/releases/v0.1.0/relay-catalog-data-v0.1.1.tar.gz":          never,
		"/releases/v0.1.0/catalog.json.gz.sha256.sha256":             never,
		"/channels/v9/latest/catalog.json.gz":                        never,
		"/channels/v1alpha2/latest/index.yaml":                       never,
		"/channels/v1alpha2/latest/relay-catalog-data-v0.1.0.tar.gz": never,
	} {
		want := answer{Code: http.StatusNotFound, Body: "404 page not found\n", Header: map[string]string{"Cache-Control": cacheControl}}
		wantAnswer(t, path, want, get(t, s, path))
	}
	wantCount(t, "catalog builds (v0.1.0 only)", s.metrics.builds.WithLabelValues("catalog"), 1)
}

func TestHTTP_BeforeFirstSync(t *testing.T) {
	s := newTestServer(t, newSourceRepo(t), newFakeReleases(t), 64<<20)
	for path, want := range map[string]answer{
		"/readyz":     {Code: http.StatusServiceUnavailable, Body: "not ready\n"},
		"/index.yaml": {Code: http.StatusServiceUnavailable, Body: "index not built yet\n"},
		"/livez":      {Code: http.StatusOK, Body: "ok\n"},
	} {
		wantAnswer(t, path, want, get(t, s, path))
	}
	// A sync that finds no release leaves the server unready, with an empty index.
	s.sync(context.Background())
	for path, want := range map[string]answer{
		"/readyz":     {Code: http.StatusServiceUnavailable, Body: "not ready\n"},
		"/index.yaml": {Code: http.StatusOK, Body: "channels: {}\n"},
	} {
		wantAnswer(t, "after an empty sync, "+path, want, get(t, s, path))
	}
}
