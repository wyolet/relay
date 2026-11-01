package inference

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/catalogembed"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/rolebinding"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// catalogRouteFixture serves two models from two authors. big-model is on both hosts, with the second host restricted to its newest snapshot; small-model is on the second host only. The first host carries every field the route strips. grants are the policy's modelref grants.
func catalogRouteFixture(t *testing.T, grants ...string) (*catalog.Catalog, *Principal) {
	t.Helper()
	provA, provB := meta.NewID(), meta.NewID()
	alphaID, betaID := meta.NewID(), meta.NewID()
	bigID, smallID := meta.NewID(), meta.NewID()
	path := "/v9/chat"

	// One system-owned credential per host, so the caller policy alone decides the grant.
	var w plantest.World
	hostKeys := []*hostkey.HostKey{
		w.Credential(&host.Host{Meta: meta.Metadata{ID: alphaID}}, nil, plantest.System, "sk-alpha"),
		w.Credential(&host.Host{Meta: meta.Metadata{ID: betaID}}, nil, plantest.System, "sk-beta"),
	}
	bind := func(name, modID, hostID string, snapshots ...string) *binding.Binding {
		return &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI, Snapshots: snapshots},
		}
	}
	u := w.User()
	k := w.UserKey(u, "catalog-route-secret")
	k.Meta.Name = "rk"
	pol := w.Policy("p", policy.Spec{Models: grants})
	w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	c := catalogtest.Catalog{
		Providers: []*provider.Provider{
			{Meta: meta.Metadata{ID: provA, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
			{Meta: meta.Metadata{ID: provB, Name: "other-prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
		},
		Hosts: []*host.Host{
			{
				Meta: meta.Metadata{ID: alphaID, Name: "alpha-host", DisplayName: "Alpha", Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: host.Spec{BaseURL: "http://alpha.internal:8000", Path: &path, ConsoleURL: "https://console.alpha.example", DocsURL: "https://docs.alpha.example"},
			},
			{
				Meta: meta.Metadata{ID: betaID, Name: "beta-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: host.Spec{BaseURL: "http://beta.internal"},
			},
		},
		Models: []*model.Model{
			{
				Meta: meta.Metadata{ID: bigID, Name: "big-model", DisplayName: "Big", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provA}},
				Spec: model.Spec{
					Snapshots:   []model.Snapshot{{Name: "big-model-2026"}, {Name: "big-model-2025"}},
					Pointer:     "big-model-2026",
					Family:      "big",
					ProductLine: "max",
				},
			},
			{
				Meta: meta.Metadata{ID: smallID, Name: "small-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provB}},
				Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "small-model-1"}}, Pointer: "small-model-1"},
			},
		},
		HostKeys: hostKeys,
		Keys:     []*key.Key{k},
		Bindings: []*binding.Binding{
			bind("big-on-alpha", bigID, alphaID),
			bind("big-on-beta", bigID, betaID, "big-model-2026"),
			bind("small-on-beta", smallID, betaID),
		},
	}
	cat := c.Load(t)
	w.Providers, w.Hosts, w.Models, w.Bindings = c.Providers, c.Hosts, c.Models, c.Bindings
	f := &dispatchFixture{plans: w.Load(t), key: k}
	f.principal = f.resolve(t)
	fixturesMu.Lock()
	fixtures[cat] = f
	fixturesMu.Unlock()
	t.Cleanup(func() {
		fixturesMu.Lock()
		delete(fixtures, cat)
		fixturesMu.Unlock()
	})
	return cat, f.principal
}

func catalogRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	api := humachi.New(r, huma.DefaultConfig("test", "1"))
	registerCatalog(api, d, nil)
	return r
}

func getCatalog(t *testing.T, h http.Handler, p *Principal, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	if p != nil {
		req = withNormalContext(req, p)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeCatalog(t *testing.T, body []byte) *sdkcatalog.Catalog {
	t.Helper()
	var c sdkcatalog.Catalog
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode catalog: %v — body: %s", err, body)
	}
	return &c
}

// catalogSlugs is the set of snapshot slugs the document's bindings name.
func catalogSlugs(c *sdkcatalog.Catalog) []string {
	seen := map[string]struct{}{}
	for _, h := range c.Hosts {
		for _, b := range h.Models {
			seen[b.MetadataName] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func listedIDs(t *testing.T, d Deps, p *Principal) []string {
	t.Helper()
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, p)
	out, err := listModels(ctx, "")
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	ids := make([]string, 0, len(out.Body.Data))
	for _, row := range out.Body.Data {
		ids = append(ids, row.ID)
	}
	sort.Strings(ids)
	return ids
}

func hostNamesOf(c *sdkcatalog.Catalog) []string {
	out := make([]string, 0, len(c.Hosts))
	for _, h := range c.Hosts {
		out = append(out, h.Name)
	}
	return out
}

// The document and /v1/models must agree on what the key can address: both read the same grant and Serves rules.
func TestCatalog_SlugsMatchListedModels(t *testing.T) {
	cases := []struct {
		name      string
		grants    []string
		wantHosts []string
	}{
		{"whole models", []string{"prov/big-model", "other-prov/small-model"}, []string{"alpha-host", "beta-host"}},
		{"binding-scoped grant", []string{"prov/big-model@beta-host"}, []string{"beta-host"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat, pr := catalogRouteFixture(t, tc.grants...)
			d := buildDeps(t, cat)
			rec := getCatalog(t, catalogRouter(d), pr)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d — body: %s", rec.Code, rec.Body.String())
			}
			doc := decodeCatalog(t, rec.Body.Bytes())
			want := listedIDs(t, d, pr)
			if got := catalogSlugs(doc); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("catalog slugs = %v, /v1/models ids = %v", got, want)
			}
			if got := hostNamesOf(doc); strings.Join(got, ",") != strings.Join(tc.wantHosts, ",") {
				t.Errorf("hosts = %v, want %v", got, tc.wantHosts)
			}
			if doc.Version != "deployment" {
				t.Errorf("version = %q, want deployment with no seeded marker", doc.Version)
			}
		})
	}
}

func TestCatalog_CarriesProductLineAndStripsUpstreamFields(t *testing.T) {
	cat, pr := catalogRouteFixture(t, "prov/big-model")
	rec := getCatalog(t, catalogRouter(buildDeps(t, cat)), pr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — body: %s", rec.Code, rec.Body.String())
	}
	doc := decodeCatalog(t, rec.Body.Bytes())
	var lines []string
	for _, m := range doc.Models {
		lines = append(lines, m.MetadataName+"="+m.ProductLine)
	}
	sort.Strings(lines)
	if strings.Join(lines, ",") != "big-model-2025=max,big-model-2026=max" {
		t.Errorf("models sidecar product lines = %v", lines)
	}

	var raw struct {
		Hosts []map[string]json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, h := range raw.Hosts {
		for _, field := range []string{"baseURL", "path", "consoleURL"} {
			if _, ok := h[field]; ok {
				t.Errorf("host %s carries %q", h["name"], field)
			}
		}
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("docs.alpha.example")) {
		t.Error("public host metadata (docsURL) must stay")
	}
}

func TestCatalog_Headers(t *testing.T) {
	cat, pr := catalogRouteFixture(t, "prov/big-model")
	rec := getCatalog(t, catalogRouter(buildDeps(t, cat)), pr)
	for k, want := range map[string]string{
		"Cache-Control": "private, no-cache",
		"Vary":          "Accept-Encoding, Authorization",
		"Content-Type":  "application/json",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if etag := rec.Header().Get("ETag"); !strings.HasPrefix(etag, `W/"`) || len(etag) != len(`W/""`)+16 {
		t.Errorf("ETag = %q, want a weak 16-hex tag", etag)
	}
}

// Equal content must carry equal tags on any pod and after a restart, so the tag ignores generatedAt and the per-process cache.
func TestCatalog_ETagStableAcrossRendersAndChangedByContent(t *testing.T) {
	cat, pr := catalogRouteFixture(t, "prov/big-model", "other-prov/small-model")
	d := buildDeps(t, cat)
	h := catalogRouter(d)
	first := getCatalog(t, h, pr)
	second := getCatalog(t, catalogRouter(d), pr)
	if first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatalf("separate renders of one catalog: ETag %q vs %q", first.Header().Get("ETag"), second.Header().Get("ETag"))
	}

	snap := cat.Current()
	a := catalogembed.FlattenScoped(snap, time.Time{}, nil)
	b := catalogembed.FlattenScoped(snap, time.Time{}, nil)
	ea, err := renderCatalog(a, "deployment", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	eb, err := renderCatalog(b, "deployment", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if ea.etag != eb.etag || bytes.Equal(ea.json, eb.json) {
		t.Fatalf("renders at two times: etags %q/%q, bodies equal=%v; want equal tags and differing bodies", ea.etag, eb.etag, bytes.Equal(ea.json, eb.json))
	}

	// A catalog change bumps the generation: the same router must drop its cached entry and tag the new content.
	var big *model.Model
	for _, m := range snap.AllModels() {
		if m.Meta.Name == "big-model" {
			cp := *m
			big = &cp
		}
	}
	big.Meta.DisplayName = "Bigger"
	f := fixtureOf(t, cat)
	plantest.Apply(t, f.plans, big)
	*pr = *f.resolve(t)
	after := getCatalog(t, h, pr)
	if after.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("ETag unchanged after a catalog change")
	}
	if !bytes.Contains(after.Body.Bytes(), []byte("Bigger")) {
		t.Error("the cached document outlived the snapshot it was built from")
	}
}

func TestCatalog_IfNoneMatch(t *testing.T) {
	cat, pr := catalogRouteFixture(t, "prov/big-model")
	h := catalogRouter(buildDeps(t, cat))
	etag := getCatalog(t, h, pr).Header().Get("ETag")
	for _, inm := range []string{etag, strings.TrimPrefix(etag, "W/"), `W/"0000000000000000", ` + etag, "*"} {
		rec := getCatalog(t, h, pr, "If-None-Match", inm)
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q: status = %d, want 304", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: 304 carried a body", inm)
		}
		if rec.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %q: 304 ETag = %q", inm, rec.Header().Get("ETag"))
		}
	}
	if rec := getCatalog(t, h, pr, "If-None-Match", `W/"0000000000000000"`); rec.Code != http.StatusOK {
		t.Errorf("a stale tag: status = %d, want 200", rec.Code)
	}
}

func TestCatalog_Gzip(t *testing.T) {
	cat, pr := catalogRouteFixture(t, "prov/big-model")
	h := catalogRouter(buildDeps(t, cat))
	plain := getCatalog(t, h, pr)
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("no Accept-Encoding: Content-Encoding = %q", plain.Header().Get("Content-Encoding"))
	}
	zipped := getCatalog(t, h, pr, "Accept-Encoding", "br, gzip")
	if zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Accept-Encoding gzip: Content-Encoding = %q", zipped.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(inflated, plain.Body.Bytes()) {
		t.Error("the gzip body does not inflate to the plain one")
	}
	if refused := getCatalog(t, h, pr, "Accept-Encoding", "gzip;q=0, *"); refused.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 must refuse gzip")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                   false,
		"gzip":               true,
		"GZIP;q=0.5":         true,
		"deflate, gzip;q=0":  false,
		"gzip;q=0, *":        false,
		"*":                  true,
		"*;q=0":              false,
		"br, x-gzip":         true,
		"identity":           false,
		"gzip ; q=0.0":       false,
		"br;q=1.0, gzip;q=1": true,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestCatalog_RejectsMissingKey(t *testing.T) {
	cat, _ := catalogRouteFixture(t, "prov/big-model")
	if rec := getCatalog(t, catalogRouter(buildDeps(t, cat)), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCatalogCache_TableAndVersionInvalidate(t *testing.T) {
	c := newCatalogCache()
	t1, t2 := &plan.RouteTable{}, &plan.RouteTable{}
	c.put("p", &catalogEntry{table: t1, version: "deployment@v1", etag: "x"})
	if _, ok := c.get("p", t1, "deployment@v1"); !ok {
		t.Fatal("hit expected")
	}
	if _, ok := c.get("p", t2, "deployment@v1"); ok {
		t.Error("an entry from a replaced route table must miss")
	}
	if _, ok := c.get("p", t1, "deployment@v2"); ok {
		t.Error("an entry from an older seeded catalog must miss")
	}
}

func TestCatalogCache_ResetsPastTheCap(t *testing.T) {
	c := newCatalogCache()
	for i := 0; i < catalogCacheMax; i++ {
		c.put(meta.NewID(), &catalogEntry{})
	}
	c.put("first-over", &catalogEntry{})
	if len(c.entries) != 1 {
		t.Fatalf("entries = %d after overflow, want the map reset to the new entry", len(c.entries))
	}
}

// The route is part of the published API, with the SDK document as its 200 schema.
func TestCatalog_ListedInOpenAPI(t *testing.T) {
	cat, _ := catalogRouteFixture(t, "prov/big-model")
	api := Mount(chi.NewRouter(), buildDeps(t, cat))
	op := api.OpenAPI().Paths["/v1/catalog"]
	if op == nil || op.Get == nil || op.Get.Hidden {
		t.Fatal("GET /v1/catalog missing from the OpenAPI document")
	}
	mt := op.Get.Responses["200"].Content["application/json"]
	if mt == nil || mt.Schema == nil || mt.Schema.Ref == "" {
		t.Fatalf("200 response schema = %+v, want a reference to the catalog document", mt)
	}
	if !strings.HasSuffix(mt.Schema.Ref, "/Catalog") {
		t.Errorf("200 schema ref = %q", mt.Schema.Ref)
	}
}
