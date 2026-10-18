package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/catalogsource"
)

// publicTag is newer than any embedded catalog, so Latest serves it rather than the embed.
const publicTag = "v99.0.0"

func publicCatalogFixture() *catalog.Catalog {
	return &catalog.Catalog{
		Version: publicTag,
		Hosts: []catalog.Host{
			{Name: "openai", BaseURL: "https://openai.example", Models: []catalog.Binding{
				{Name: "gpt-4o", MetadataName: "gpt-4o", Adapter: "openai", Providers: []string{"openai"}},
				{Name: "gpt-4o-mini", MetadataName: "gpt-4o-mini", Adapter: "openai", Providers: []string{"openai"}},
			}},
			{Name: "reseller", BaseURL: "https://reseller.example", Models: []catalog.Binding{
				{Name: "openai/gpt-4o", MetadataName: "gpt-4o", Adapter: "openai", Providers: []string{"openai"}},
				{Name: "other-model", MetadataName: "other-model", Adapter: "openai", Providers: []string{"otherco"}},
			}},
		},
		Models: []catalog.ModelInfo{
			{MetadataName: "gpt-4o", Provider: "openai"},
			{MetadataName: "gpt-4o-mini", Provider: "openai"},
			{MetadataName: "other-model", Provider: "otherco"},
		},
		Providers: []catalog.ProviderInfo{{Name: "openai"}, {Name: "otherco"}},
	}
}

// newPublicCatalogServer serves publicCatalogFixture in the catalog server layout.
func newPublicCatalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	data, err := json.Marshal(publicCatalogFixture())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	asset := buf.Bytes()
	sum := sha256.Sum256(asset)
	files := map[string][]byte{
		"/index.yaml": []byte("channels:\n    " + catalogsource.Channel + ":\n        latest: " + publicTag + "\n"),
		"/releases/" + publicTag + "/catalog.json.gz":        asset,
		"/releases/" + publicTag + "/catalog.json.gz.sha256": []byte(hex.EncodeToString(sum[:]) + "  catalog.json.gz\n"),
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
	return srv
}

func publicSource(t *testing.T, srv *httptest.Server, opts ...catalogsource.LatestOption) *catalogsource.LatestSource {
	t.Helper()
	t.Setenv(catalogsource.EnvCatalogURL, "")
	t.Setenv(catalogsource.EnvCatalogCacheDir, "")
	return catalogsource.Latest(append([]catalogsource.LatestOption{catalogsource.WithBaseURL(srv.URL), catalogsource.WithLatestHTTPClient(srv.Client())}, opts...)...)
}

// noRequests is a server that fails the test on any request it receives.
func noRequests(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hostNames(ic *catalog.IndexedCatalog) string {
	var names []string
	for _, h := range ic.Catalog.Hosts {
		names = append(names, h.Name)
	}
	return strings.Join(names, ",")
}

// wantHostScoped checks c reads only the openai host of the fixture, from the catalog server.
func wantHostScoped(t *testing.T, c *Client) {
	t.Helper()
	mc, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mc.Origin != OriginCatalog || mc.Stale || mc.Err != nil || mc.Catalog.Version != publicTag {
		t.Fatalf("origin %q stale %v err %v version %q", mc.Origin, mc.Stale, mc.Err, mc.Catalog.Version)
	}
	if hostNames(mc.IndexedCatalog) != "openai" || len(mc.Catalog.Models) != 2 || len(mc.Catalog.Providers) != 1 || mc.Catalog.Providers[0].Name != "openai" {
		t.Fatalf("hosts %q, %d models, providers %+v; want only the openai host and what it references", hostNames(mc.IndexedCatalog), len(mc.Catalog.Models), mc.Catalog.Providers)
	}
	if _, _, err := mc.Resolve("other-model"); err == nil {
		t.Error("another host's model resolves in a host-scoped catalog")
	}
	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list.IDs, ",") != "gpt-4o,gpt-4o-mini" || list.Origin != OriginCatalog || list.Stale || list.Err != nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestPublicCatalog_DirectClientSeesItsHost(t *testing.T) {
	src := publicSource(t, newPublicCatalogServer(t))
	clearModeEnv(t)

	c, err := ForMode("gpt-4o@openai", WithMode(ModeDirect), WithProviderKey("sk"), WithClientOptions(WithCatalogSource(src)))
	if err != nil {
		t.Fatal(err)
	}
	wantHostScoped(t, c)

	// The provider base URL is never contacted either.
	c, err = For("gpt-4o@openai", "sk", WithBaseURL(noRequests(t).URL), WithClient(WithCatalogSource(src)))
	if err != nil {
		t.Fatal(err)
	}
	wantHostScoped(t, c)
}

func TestPublicCatalog_TelemetryClientNeverReadsRelay(t *testing.T) {
	src := publicSource(t, newPublicCatalogServer(t))
	clearModeEnv(t)
	t.Setenv(EnvBaseURL, noRequests(t).URL)
	t.Setenv(EnvAPIKey, "rk")
	sink := newExportSink(t)

	c, err := ForMode("gpt-4o@openai", WithMode(ModeTelemetry), WithProviderKey("sk"), WithEmitter(sink.emitter(t, false)), WithClientOptions(WithCatalogSource(src)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.telemetry == nil {
		t.Fatal("not a telemetry client")
	}
	wantHostScoped(t, c)
}

func TestPublicCatalog_ConstructorClientSeesEveryHost(t *testing.T) {
	src := publicSource(t, newPublicCatalogServer(t))
	c := OpenAI(noRequests(t).URL, "sk", WithCatalogSource(src))
	mc, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hostNames(mc.IndexedCatalog) != "openai,reseller" || len(mc.Catalog.Models) != 3 || mc.Origin != OriginCatalog {
		t.Fatalf("hosts %q models %d origin %q", hostNames(mc.IndexedCatalog), len(mc.Catalog.Models), mc.Origin)
	}
	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list.IDs, ",") != "gpt-4o,gpt-4o-mini,other-model" {
		t.Fatalf("ids = %v, want each slug once in catalog order", list.IDs)
	}
}

func TestPublicCatalog_HostMissingFromTheCatalogIsEmpty(t *testing.T) {
	src := publicSource(t, newPublicCatalogServer(t))
	private := &catalog.Catalog{Hosts: []catalog.Host{{Name: "private-host", BaseURL: "https://private.example", Models: []catalog.Binding{
		{Name: "in-house", MetadataName: "in-house", Adapter: "openai"},
	}}}}
	ic, err := catalog.Index(private)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ForFrom(ic, "in-house", "sk", WithClient(WithCatalogSource(src)))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mc.Catalog.Hosts) != 0 || len(mc.Catalog.Models) != 0 || mc.Origin != OriginCatalog {
		t.Fatalf("hosts %d models %d origin %q, want an empty catalog", len(mc.Catalog.Hosts), len(mc.Catalog.Models), mc.Origin)
	}
	list, err := c.Models(context.Background())
	if err != nil || list.IDs == nil || len(list.IDs) != 0 {
		t.Fatalf("list %+v err %v, want an empty list", list, err)
	}
}

func TestPublicCatalog_Origins(t *testing.T) {
	srv := newPublicCatalogServer(t)
	src := publicSource(t, srv, catalogsource.WithRefreshInterval(0))
	c := OpenAI("http://upstream.invalid", "sk", WithCatalogSource(src))
	fresh, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Origin != OriginCatalog || fresh.Stale || fresh.Err != nil {
		t.Fatalf("fresh: origin %q stale %v err %v", fresh.Origin, fresh.Stale, fresh.Err)
	}

	srv.Close()
	stale, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stale.Origin != OriginCatalog || !stale.Stale || stale.Err == nil || stale.Catalog.Version != publicTag {
		t.Fatalf("server down with a held copy: origin %q stale %v err %v", stale.Origin, stale.Stale, stale.Err)
	}
	list, err := c.Models(context.Background())
	if err != nil || !list.Stale || list.Err == nil || list.Origin != OriginCatalog {
		t.Fatalf("Models must carry Stale and Err: %+v %v", list, err)
	}

	unreachable := OpenAI("http://upstream.invalid", "sk", WithCatalogSource(publicSource(t, srv)))
	embedded, err := unreachable.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if embedded.Origin != OriginEmbedded || embedded.Stale || embedded.Err == nil || len(embedded.Catalog.Hosts) == 0 {
		t.Fatalf("nothing held: origin %q stale %v err %v", embedded.Origin, embedded.Stale, embedded.Err)
	}
}

func TestPublicCatalog_WithCatalogSourceOverridesTheDefault(t *testing.T) {
	t.Setenv(catalogsource.EnvCatalogURL, "off")
	prev := defaultCatalogSource
	defaultCatalogSource = sync.OnceValue(func() *catalogsource.LatestSource { return catalogsource.Latest() })
	t.Cleanup(func() { defaultCatalogSource = prev })

	byDefault, err := OpenAI("http://upstream.invalid", "sk").Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if byDefault.Origin != OriginEmbedded || byDefault.Err != nil {
		t.Fatalf("default source with WR_CATALOG_URL=off: origin %q err %v", byDefault.Origin, byDefault.Err)
	}

	src := publicSource(t, newPublicCatalogServer(t))
	overridden, err := OpenAI("http://upstream.invalid", "sk", WithCatalogSource(src)).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overridden.Origin != OriginCatalog || overridden.Catalog.Version != publicTag {
		t.Fatalf("override: origin %q version %q", overridden.Origin, overridden.Catalog.Version)
	}
}
