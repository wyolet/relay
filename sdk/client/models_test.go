package client

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

// scopedCatalogJSON is the shape relay's /v1/catalog serves: no baseURL, path or consoleURL on hosts.
const scopedCatalogJSON = `{
  "version": "deployment@v0.1.0",
  "generatedAt": "2026-10-19T00:00:00Z",
  "hosts": [{"name": "alpha-host", "displayName": "Alpha", "models": [
    {"model": "big-model-2026", "upstream": "big-model-2026", "adapter": "openai", "providers": ["prov"]}
  ]}]
}`

// fakeRelay answers /v1/models and /v1/catalog with whatever status the test sets, and records what each request carried.
type fakeRelay struct {
	mu     sync.Mutex
	status int
	got    []*http.Request
	srv    *httptest.Server
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status := f.status
		f.got = append(f.got, r.Clone(context.Background()))
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"refused"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"zeta"},{"id":"alpha"}]}`))
		case "/v1/catalog":
			const etag = `W/"0123456789abcdef"`
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
				zw := gzip.NewWriter(w)
				_, _ = zw.Write([]byte(scopedCatalogJSON))
				_ = zw.Close()
				return
			}
			_, _ = w.Write([]byte(scopedCatalogJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) setStatus(s int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

func (f *fakeRelay) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

func relayFor(t *testing.T, f *fakeRelay) *Client {
	t.Helper()
	t.Setenv(EnvHeaders, "")
	return Relay(f.srv.URL, "rk-test", WithHeader("X-Team", "red"))
}

func TestModels_ReturnsIDsInServerOrder(t *testing.T) {
	f := newFakeRelay(t)
	list, err := relayFor(t, f).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list.IDs, ",") != "zeta,alpha" || list.Origin != OriginRelay || list.Stale || list.Err != nil {
		t.Fatalf("list = %+v", list)
	}
	req := f.last()
	if req.URL.Path != "/v1/models" || req.Header.Get("Authorization") != "Bearer rk-test" || req.Header.Get("X-Team") != "red" {
		t.Errorf("request = %s %v", req.URL.Path, req.Header)
	}
}

func TestCatalog_FreshThenRevalidated(t *testing.T) {
	f := newFakeRelay(t)
	c := relayFor(t, f)
	first, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Origin != OriginRelay || first.Stale || first.Err != nil {
		t.Fatalf("first = origin %q stale %v err %v", first.Origin, first.Stale, first.Err)
	}
	if first.Catalog.Version != "deployment@v0.1.0" || len(first.Catalog.Hosts) != 1 {
		t.Fatalf("catalog = %q with %d hosts", first.Catalog.Version, len(first.Catalog.Hosts))
	}
	req := f.last()
	if req.Header.Get("Authorization") != "Bearer rk-test" || req.Header.Get("X-Team") != "red" || req.Header.Get("If-None-Match") != "" {
		t.Errorf("first request headers = %v", req.Header)
	}
	if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		t.Errorf("the transport did not negotiate gzip, so the gzip body went untested: %v", req.Header)
	}

	second, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.last().Header.Get("If-None-Match") != `W/"0123456789abcdef"` {
		t.Fatalf("revalidation sent If-None-Match %q", f.last().Header.Get("If-None-Match"))
	}
	if second.IndexedCatalog != first.IndexedCatalog || second.Origin != OriginRelay || second.Stale || second.Err != nil {
		t.Errorf("a 304 must return the held copy as fresh: %+v", second)
	}
}

func TestCatalog_ServerErrorServesTheHeldCopyAsStale(t *testing.T) {
	f := newFakeRelay(t)
	c := relayFor(t, f)
	fresh, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.setStatus(http.StatusBadGateway)
	got, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("a 5xx with a held copy must not error: %v", err)
	}
	var apiErr *APIError
	if got.IndexedCatalog != fresh.IndexedCatalog || got.Origin != OriginRelay || !got.Stale || !errors.As(got.Err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("got origin %q stale %v err %v", got.Origin, got.Stale, got.Err)
	}

	if _, err := c.Models(context.Background()); err == nil {
		t.Error("Models with no held list must return the 5xx")
	}
}

func TestCatalog_ServerErrorWithoutACopyFallsBackToEmbedded(t *testing.T) {
	f := newFakeRelay(t)
	f.setStatus(http.StatusServiceUnavailable)
	got, err := relayFor(t, f).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != OriginEmbedded || got.Stale || got.Err == nil || len(got.Catalog.Hosts) == 0 {
		t.Fatalf("got origin %q stale %v err %v hosts %d", got.Origin, got.Stale, got.Err, len(got.Catalog.Hosts))
	}
}

func TestModelsAndCatalog_NetworkError(t *testing.T) {
	f := newFakeRelay(t)
	c := relayFor(t, f)
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()

	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models with a held list: %v", err)
	}
	if !list.Stale || list.Err == nil || strings.Join(list.IDs, ",") != "zeta,alpha" {
		t.Errorf("list = %+v", list)
	}
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog without a held copy: %v", err)
	}
	if cat.Origin != OriginEmbedded || cat.Err == nil {
		t.Errorf("catalog origin %q err %v", cat.Origin, cat.Err)
	}
	if _, err := relayFor(t, f).Models(context.Background()); err == nil {
		t.Error("Models with nothing held must return the network error")
	}
}

// A refused credential must not keep rendering the picker it used to see.
func TestModelsAndCatalog_RefusedCredentialDropsTheHeldCopy(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeRelay(t)
			c := relayFor(t, f)
			if _, err := c.Catalog(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Models(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.setStatus(status)
			var apiErr *APIError
			if _, err := c.Catalog(context.Background()); !errors.As(err, &apiErr) || apiErr.StatusCode != status {
				t.Fatalf("Catalog err = %v, want the %d", err, status)
			}
			if _, err := c.Models(context.Background()); err == nil {
				t.Fatal("Models must error too")
			}
			f.setStatus(http.StatusInternalServerError)
			got, err := c.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Origin != OriginEmbedded {
				t.Errorf("after a %d the held copy must be gone, got origin %q stale %v", status, got.Origin, got.Stale)
			}
			if _, err := c.Models(context.Background()); err == nil {
				t.Error("after a refusal Models must not serve the held list")
			}
		})
	}
}

func TestCatalog_NotServedByThisRelay(t *testing.T) {
	f := newFakeRelay(t)
	f.setStatus(http.StatusNotFound)
	_, err := relayFor(t, f).Catalog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not serve /v1/catalog") {
		t.Fatalf("err = %v", err)
	}
}

func TestModelsAndCatalog_NeedARelayClient(t *testing.T) {
	c := OpenAI("http://upstream.invalid", "sk")
	if _, err := c.Models(context.Background()); err == nil {
		t.Error("Models on a vendor client must error")
	}
	if _, err := c.Catalog(context.Background()); err == nil {
		t.Error("Catalog on a vendor client must error")
	}
}

// RelayWS accepts a ws:// base; the reads go over HTTP to the same relay.
func TestModels_RelayWSUsesTheHTTPBase(t *testing.T) {
	f := newFakeRelay(t)
	t.Setenv(EnvHeaders, "")
	c := RelayWS("ws://"+strings.TrimPrefix(f.srv.URL, "http://"), "rk-test")
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestForMode_ProxyIsARelayClient(t *testing.T) {
	f := newFakeRelay(t)
	t.Setenv(EnvBaseURL, f.srv.URL)
	t.Setenv(EnvAPIKey, "rk-test")
	t.Setenv(EnvHeaders, "")
	c, err := ForMode("big-model-2026", WithMode(ModeProxy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestForFrom_RejectsAHostWithoutABaseURL(t *testing.T) {
	cat, err := catalog.LoadBytes([]byte(scopedCatalogJSON))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ForFrom(cat, "big-model-2026@alpha-host", "sk"); err == nil || !strings.Contains(err.Error(), `"alpha-host" has no base URL`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ForFrom(cat, "big-model-2026@alpha-host", "sk", WithBaseURL("http://gateway.example")); err != nil {
		t.Fatalf("WithBaseURL must satisfy the guard: %v", err)
	}
}
