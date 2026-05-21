package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
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
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// --- catalog list stubs for dispatch tests ---

type provListD []*provider.Provider
type hostListD []*host.Host
type polListD []*policy.Policy
type modListD []*model.Model
type keyListD []*hostkey.HostKey
type rlListD []*ratelimit.RateLimit
type rkListD []*relaykey.RelayKey
type rcListD []*pricing.Pricing

func (l provListD) List(context.Context) ([]*provider.Provider, error) { return l, nil }
func (l hostListD) List(context.Context) ([]*host.Host, error)         { return l, nil }
func (l polListD) List(context.Context) ([]*policy.Policy, error)      { return l, nil }
func (l modListD) List(context.Context) ([]*model.Model, error)        { return l, nil }
func (l keyListD) List(context.Context) ([]*hostkey.HostKey, error)    { return l, nil }
func (l rlListD) List(context.Context) ([]*ratelimit.RateLimit, error) { return l, nil }
func (l rkListD) List(context.Context) ([]*relaykey.RelayKey, error)   { return l, nil }
func (l rcListD) List(context.Context) ([]*pricing.Pricing, error)     { return l, nil }

// buildDispatchCatalog creates a catalog with a model bound to the given
// hostName (Meta.Name) with the provided adapter. Returns the catalog and
// the relay key that authorises access.
func buildDispatchCatalog(t *testing.T, hostName string, hostAdapter adapters.Name) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()

	provID := meta.NewID()
	hostID := meta.NewID()
	hkID := meta.NewID()
	modID := meta.NewID()
	polID := meta.NewID()
	rkID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.invalid"},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Hosts:     []model.HostBinding{{HostID: hostID, Adapter: hostAdapter}},
			Snapshots: []model.Snapshot{{Name: slug.From("test-model")}},
			Pointer:   slug.From("test-model"),
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
		provListD{prov},
		hostListD{h},
		polListD{pol},
		modListD{m},
		keyListD{hk},
		rlListD{},
		rkListD{rk},
		rcListD{},
	)
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}
	return cat, rk
}

// stubAdapter is a minimal pipeline.Adapter for tests. It always returns
// a connection error so the pipeline fails fast without a real upstream.
type stubAdapter struct{}

func (stubAdapter) Call(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
	return nil, fmt.Errorf("stub: no upstream")
}
func (stubAdapter) ExtractTokens(_ []byte) pkgusage.Tokens { return nil }
func (stubAdapter) Retryable(_ *http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

// stubTranslator satisfies adapters.Translator using the Identity base.
// Avoids importing app/adapters/openai to prevent import cycles.
type stubTranslator struct{ adapters.Identity }

func buildDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	limiter := pkgratelimit.New(kvStore, nil, nil)
	pl := &pipeline.Pipeline{Logger: nil}

	return Deps{
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Proxy:    proxy.New(limiter, nil),
		Adapters: map[adapters.Name]pipeline.Adapter{
			adapters.OpenAI:           stubAdapter{},
			adapters.OpenAIResponses:  stubAdapter{},
			adapters.OpenAIEmbeddings: stubAdapter{},
			adapters.Anthropic:        stubAdapter{},
		},
		Translators: adapters.Registry{
			adapters.OpenAI:           stubTranslator{},
			adapters.OpenAIResponses:  stubTranslator{},
			adapters.OpenAIEmbeddings: stubTranslator{},
			adapters.Anthropic:        stubTranslator{},
		},
	}
}

// withNormalContext injects a ModeNormal classification and relay key into
// r's context, simulating what the classifier + auth middleware would do.
func withNormalContext(r *http.Request, rk *relaykey.RelayKey) *http.Request {
	ctx := WithClassification(r.Context(), Classification{Mode: ModeNormal})
	ctx = context.WithValue(ctx, ctxRelayKeyT{}, rk)
	return r.WithContext(ctx)
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseDispatchErr(t *testing.T, body []byte) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("failed to parse error body: %v — raw: %s", err, body)
	}
	return e
}

// TestDispatch_Responses_OpenAIProperHost_BytePass verifies that
// Inbound=OpenAIResponses on host "openai" (Adapter=OpenAI) takes the
// byte-pass path. The pipeline fails on the stub adapter, but neither
// the cross-shape "responses_unsupported_host" nor "translate_request"
// codes fire — those only show up on the cross-shape paths.
func TestDispatch_Responses_OpenAIProperHost_BytePass(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"test-model","stream":false}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		switch e.Error.Code {
		case "responses_unsupported_host", "translate_request", "invalid_responses_request":
			t.Fatalf("byte-pass path should not have hit a cross-shape error: %q (%s)", e.Error.Code, e.Error.Message)
		}
	}
}

// TestDispatch_Responses_OpenAICompatHost_CrossShape verifies that a host
// with Adapter=OpenAI but Meta.Name != "openai" (Ollama, Groq, Together,
// Fireworks, etc.) takes the cctranslator cross-shape path, not the
// old "responses_unsupported_host" rejection.
func TestDispatch_Responses_OpenAICompatHost_CrossShape(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"test-model","input":"hi","stream":false}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "responses_unsupported_host" {
			t.Fatalf("old guard still fires on OpenAI-compat host — cross-shape path not taken")
		}
		// invalid_responses_request / translate_request would mean the parse
		// failed; that's a body-shape issue, not what we're testing.
	}
}

// TestDispatch_Responses_AnthropicHost_CrossShape verifies that a host with
// Adapter=Anthropic takes the anthropictranslator cross-shape path. The
// stub adapter fails the pipeline call after translation, but the guard
// itself must not fire.
func TestDispatch_Responses_AnthropicHost_CrossShape(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"test-model","input":"hi","stream":false}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "responses_unsupported_host" {
			t.Fatalf("guard fired for Anthropic host — anthropictranslator path not taken")
		}
	}
}

// TestDispatch_Responses_CrossShape_RejectsPreviousResponseID verifies that
// Responses-only fields (previous_response_id) get rejected at the
// translator boundary with a 400 translate_request when routed to a CC
// upstream. (On OpenAI proper byte-pass, they pass through untouched.)
func TestDispatch_Responses_CrossShape_RejectsPreviousResponseID(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"test-model","input":"hi","previous_response_id":"resp_123"}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code != "translate_request" {
		t.Errorf("error code: want translate_request, got %q (msg: %s)", e.Error.Code, e.Error.Message)
	}
}

// TestDispatch_EmbeddingsGuard_AnthropicHost verifies that
// Inbound=OpenAIEmbeddings on a host with adapter=anthropic returns 400
// embeddings_unsupported_host.
func TestDispatch_EmbeddingsGuard_AnthropicHost(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/embeddings", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIEmbeddings,
		Body:      []byte(`{"model":"test-model","input":"hello"}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code != "embeddings_unsupported_host" {
		t.Errorf("error code: want embeddings_unsupported_host, got %q", e.Error.Code)
	}
}

// TestDispatch_EmbeddingsGuard_OpenAICompatHost verifies that
// Inbound=OpenAIEmbeddings on a host with adapter=openai passes the guard
// even when the host name is not "openai" (e.g. "ollama-self", "together").
func TestDispatch_EmbeddingsGuard_OpenAICompatHost(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/embeddings", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIEmbeddings,
		Body:      []byte(`{"model":"test-model","input":"hello"}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "embeddings_unsupported_host" {
			t.Fatalf("guard fired for OpenAI-compat host %q but should not have", "ollama-self")
		}
	}
}

// TestDispatch_EmbeddingsGuard_OpenAINamedHost verifies that the guard
// also accepts the canonical host "openai" (adapter=openai).
func TestDispatch_EmbeddingsGuard_OpenAINamedHost(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/embeddings", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIEmbeddings,
		Body:      []byte(`{"model":"test-model","input":"hello"}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "embeddings_unsupported_host" {
			t.Fatalf("guard fired for host 'openai' but should not have")
		}
	}
}

// TestDispatch_NormalOpenAI_UnaffectedByGuards confirms that a standard
// Inbound=OpenAI request is not blocked by either the Responses or
// Embeddings guard (the guards are shape-conditional).
func TestDispatch_NormalOpenAI_UnaffectedByGuards(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "groq", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","stream":false}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "responses_unsupported_host" || e.Error.Code == "embeddings_unsupported_host" {
			t.Fatalf("guard fired on standard OpenAI request — guards are shape-conditional (got %q)", e.Error.Code)
		}
	}
}
