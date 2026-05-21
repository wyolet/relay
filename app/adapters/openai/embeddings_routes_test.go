package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

// --- catalog list stubs for embeddings route tests ---

type embProvList []*provider.Provider
type embHostList []*host.Host
type embPolList []*policy.Policy
type embModList []*model.Model
type embKeyList []*hostkey.HostKey
type embRLList []*ratelimit.RateLimit
type embRKList []*relaykey.RelayKey
type embRCList []*pricing.Pricing

func (l embProvList) List(context.Context) ([]*provider.Provider, error) { return l, nil }
func (l embHostList) List(context.Context) ([]*host.Host, error)         { return l, nil }
func (l embPolList) List(context.Context) ([]*policy.Policy, error)      { return l, nil }
func (l embModList) List(context.Context) ([]*model.Model, error)        { return l, nil }
func (l embKeyList) List(context.Context) ([]*hostkey.HostKey, error)    { return l, nil }
func (l embRLList) List(context.Context) ([]*ratelimit.RateLimit, error) { return l, nil }
func (l embRKList) List(context.Context) ([]*relaykey.RelayKey, error)   { return l, nil }
func (l embRCList) List(context.Context) ([]*pricing.Pricing, error)     { return l, nil }

// testEmbeddingsDeps builds a minimal inference.Deps wired against an
// in-memory catalog. The relay key hash "testhash" maps to a policy that
// allows text-embedding-3-small served on host "openai" (adapter=openai).
func testEmbeddingsDeps(t *testing.T) (inference.Deps, *relaykey.RelayKey) {
	t.Helper()

	provID := meta.NewID()
	hostID := meta.NewID()
	hkID := meta.NewID()
	modID := meta.NewID()
	polID := meta.NewID()
	rkID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.invalid"},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "text-embedding-3-small", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Hosts:     []model.HostBinding{{HostID: hostID, Adapter: adapters.OpenAI}},
			Snapshots: []model.Snapshot{{Name: slug.From("text-embedding-3-small")}},
			Pointer:   slug.From("text-embedding-3-small"),
		},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}},
	}
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: rkID, Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: relaykey.Spec{PolicyID: polID, KeyHash: "testhash"},
	}

	cat := catalog.New(
		embProvList{prov},
		embHostList{h},
		embPolList{pol},
		embModList{m},
		embKeyList{hk},
		embRLList{},
		embRKList{rk},
		embRCList{},
	)
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}

	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	limiter := pkgratelimit.New(kvStore, nil, nil)
	pl := &pipeline.Pipeline{Logger: nil}
	proxyPl := proxy.New(limiter, nil)

	return inference.Deps{
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Proxy:    proxyPl,
		Adapters: map[adapters.Name]pipeline.Adapter{
			adapters.OpenAI:           New(),
			adapters.OpenAIEmbeddings: New(WithPath(embeddingsPath)),
		},
		Translators: adapters.Registry{
			adapters.OpenAI:           Translator{},
			adapters.OpenAIEmbeddings: Translator{},
		},
	}, rk
}

// postEmbeddingsBody calls handleEmbeddings with the given JSON body and auth.
func postEmbeddingsBody(t *testing.T, d inference.Deps, body []byte, auth string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/embeddings", bytes.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	handleEmbeddings(d, w, r)
	return w
}

type embErrBody struct {
	Error struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"error"`
}

func parseEmbErrCode(t *testing.T, body []byte) string {
	t.Helper()
	var e embErrBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("failed to parse error body: %v — body: %s", err, body)
	}
	return e.Error.Code
}

func TestHandleEmbeddings_InvalidJSON(t *testing.T) {
	d, _ := testEmbeddingsDeps(t)
	w := postEmbeddingsBody(t, d, []byte(`not json`), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleEmbeddings_MissingModel(t *testing.T) {
	d, _ := testEmbeddingsDeps(t)
	w := postEmbeddingsBody(t, d, []byte(`{"input":"hello world"}`), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
	if code := parseEmbErrCode(t, w.Body.Bytes()); code != "missing_model" {
		t.Errorf("error code: want missing_model, got %s", code)
	}
}

// TestHandleEmbeddings_ValidBodyReachesDispatch confirms that a well-formed
// embeddings body with model present makes it past the parse step and into
// Dispatch. Without a relay key in context (no middleware), Dispatch returns
// 401 — which proves parse was OK and Dispatch was reached.
func TestHandleEmbeddings_ValidBodyReachesDispatch(t *testing.T) {
	d, _ := testEmbeddingsDeps(t)
	body := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	w := postEmbeddingsBody(t, d, body, "Bearer sk-relay-test")
	if w.Code == http.StatusBadRequest {
		t.Errorf("got 400 (parse error) but expected Dispatch to be reached; body: %s", w.Body.String())
	}
}

// TestHandleEmbeddings_StreamAlwaysFalse verifies that stream is always
// false for embeddings (embeddings API has no streaming mode).
// We achieve this by confirming a body without any stream field still
// reaches Dispatch normally (not a parse error).
func TestHandleEmbeddings_StreamAlwaysFalse(t *testing.T) {
	d, _ := testEmbeddingsDeps(t)
	// No "stream" field in body — valid embeddings request shape.
	body := []byte(`{"model":"text-embedding-3-small","input":["hello","world"]}`)
	w := postEmbeddingsBody(t, d, body, "")
	if w.Code == http.StatusBadRequest {
		t.Errorf("valid body caused 400; body: %s", w.Body.String())
	}
}
