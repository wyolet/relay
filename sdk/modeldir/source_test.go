package modeldir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddFromRelease(t *testing.T) {
	const tag = "v0.0.0-modeldir-test"
	asset := gzipped(t, catalogJSON(t, fixtureCatalog("relay-catalog@"+Channel)))
	sum := sha256.Sum256(asset)
	files := map[string][]byte{
		"/" + tag + "/catalog.json.gz":        asset,
		"/" + tag + "/catalog.json.gz.sha256": []byte(hex.EncodeToString(sum[:])),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	src := Release(tag, t.TempDir(), WithDownloadURL(srv.URL), WithHTTPClient(srv.Client()))
	m, err := Add(context.Background(), t.TempDir(), "gpt-5.5-2026-04-23", src, AddOptions{})
	if err != nil || m.Source.Catalog != tag {
		t.Fatalf("Add from release = %+v, %v", m, err)
	}
}
