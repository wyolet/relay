package inference

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalogembed"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/settings"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// registerCatalog serves GET /v1/catalog: the SDK catalog document cut down to the bindings the caller's key can route to, by the same grant rule /v1/models lists with. Upstream addresses and the operator console are stripped; the document is for relay-aware UIs, which call through relay.
func registerCatalog(api huma.API, d Deps, mw huma.Middlewares) {
	cache := newCatalogCache()
	schema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(sdkcatalog.Catalog{}), true, "Catalog")
	huma.Register(api, huma.Operation{
		OperationID: "get_catalog",
		Method:      http.MethodGet,
		Path:        "/v1/catalog",
		Summary:     "Catalog of the models, hosts and prices the caller can use",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Responses: map[string]*huma.Response{
			"200": {Description: "OK", Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}},
			"304": {Description: "Not Modified"},
		},
		Errors: []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		entry, err := scopedCatalog(ctx, d, cache)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			writeCatalog(w, r, entry)
		}}, nil
	})
}

// scopedCatalog returns the caller's rendered document, from the cache while the caller's route table and the seeded catalog version are unchanged.
func scopedCatalog(ctx context.Context, d Deps, cache *catalogCache) (*catalogEntry, error) {
	principal := PrincipalFrom(ctx)
	if principal == nil {
		return nil, huma.Error401Unauthorized("missing relay key")
	}
	table := principal.Caller.Routes
	scope := principal.Caller.Principal
	version := deploymentCatalogVersion(d.Catalog)
	if e, ok := cache.get(scope, table, version); ok {
		return e, nil
	}

	snap := table.Catalog()
	listing := table.Listing()
	visible := make(map[string]struct{}, len(listing))
	granted := map[string]struct{}{}
	for _, l := range listing {
		visible[l.Model.Meta.ID] = struct{}{}
		for _, b := range l.Bindings {
			granted[b.Meta.ID] = struct{}{}
		}
	}
	keep := func(m *model.Model, b *binding.Binding) bool {
		if _, ok := visible[m.Meta.ID]; !ok {
			return false
		}
		_, ok := granted[b.Meta.ID]
		return ok
	}
	e, err := renderCatalog(catalogembed.FlattenScoped(snap, time.Time{}, keep), version, time.Now())
	if err != nil {
		return nil, huma.Error500InternalServerError("render catalog", err)
	}
	e.table = table
	cache.put(scope, e)
	return e, nil
}

// renderCatalog strips what a key holder must not see and encodes the document. The ETag hashes the document with generatedAt zeroed, so it changes only with content and agrees across pods and restarts.
func renderCatalog(cat *sdkcatalog.Catalog, version string, at time.Time) (*catalogEntry, error) {
	for i := range cat.Hosts {
		cat.Hosts[i].BaseURL = ""
		cat.Hosts[i].Path = nil
		cat.Hosts[i].ConsoleURL = ""
	}
	cat.Version = version
	cat.GeneratedAt = time.Time{}
	unstamped, err := compactCatalogJSON(cat)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(unstamped)
	cat.GeneratedAt = at.UTC()
	body, err := compactCatalogJSON(cat)
	if err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return &catalogEntry{
		version: version,
		etag:    `W/"` + hex.EncodeToString(sum[:])[:16] + `"`,
		json:    body,
		gz:      gz.Bytes(),
	}, nil
}

// compactCatalogJSON encodes through catalogembed.MarshalJSON, whose sorted order is what keeps the ETag stable across renders.
func compactCatalogJSON(cat *sdkcatalog.Catalog) ([]byte, error) {
	indented, err := catalogembed.MarshalJSON(cat)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Compact(&out, indented); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// deploymentCatalogVersion names the document after the catalog release this deployment was seeded from. The "@" keeps it from ever parsing as a pristine release tag.
func deploymentCatalogVersion(cat *appcatalog.Catalog) string {
	if v, ok := cat.Setting(settings.SectionCatalogSource); ok {
		if cs, ok := v.(*settings.CatalogSource); ok && cs != nil && cs.Version != "" {
			return "deployment@" + cs.Version
		}
	}
	return "deployment"
}

func writeCatalog(w http.ResponseWriter, r *http.Request, e *catalogEntry) {
	h := w.Header()
	h.Set("ETag", e.etag)
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Vary", "Accept-Encoding, Authorization")
	if etagMatches(r.Header.Get("If-None-Match"), e.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := e.json
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = e.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// etagMatches applies If-None-Match's weak comparison: "*" or any listed tag equal to etag once the W/ prefixes are set aside.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == want {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether Accept-Encoding admits gzip: an explicit gzip entry decides, else a "*" entry; q=0 refuses.
func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		ok := qualityAboveZero(params)
		if coding == "*" {
			wildcard = ok
			continue
		}
		return ok
	}
	return wildcard
}

func qualityAboveZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, found := strings.Cut(strings.TrimSpace(p), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q > 0
	}
	return true
}
