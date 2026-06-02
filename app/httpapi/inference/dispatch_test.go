package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
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
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
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
type bndListD []*binding.Binding

func (l bndListD) List(context.Context) ([]*binding.Binding, error)    { return l, nil }
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
		bndListD{},
	)
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}
	return cat, rk
}

// stubAdapter is a minimal pipeline.Adapter for tests.
type stubAdapter struct{}

func (stubAdapter) Call(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
	return nil, fmt.Errorf("stub: no upstream")
}
func (stubAdapter) ExtractTokens(_ []byte) pkgusage.Tokens { return nil }
func (stubAdapter) Retryable(_ *http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

// stubV1Translator is a no-op v1.Translator for test specs.
type stubV1Translator struct{}

func (stubV1Translator) ParseRequest(body []byte) (*pkgrelay.Request, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) SerializeRequest(req *pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) ParseResponse(body []byte) (*pkgrelay.Response, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) SerializeResponse(resp *pkgrelay.Response, req *pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (stubV1Translator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

// buildTestRegistry constructs a minimal adapter.Registry for tests.
// Registers specs for openai, openai_responses, openai_embeddings, and
// anthropic — each with a stubAdapter so tests exercise dispatch routing
// without a live upstream.
func buildTestRegistry() *adapter.Registry {
	openaiSpec := (&adapter.Spec{
		Name:         adapters.OpenAI,
		UpstreamPath: "/v1/chat/completions",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:   stubV1Translator{},
	}).Build()

	// Responses: IsNativePath returns true only when host name == "openai".
	responsesSpec := (&adapter.Spec{
		Name:         adapters.OpenAIResponses,
		UpstreamPath: "/v1/responses",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:   stubV1Translator{},
		IsNativePath: func(plan *routing.Plan) bool {
			return plan.HostBinding.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
		},
	}).Build()

	embeddingsSpec := (&adapter.Spec{
		Name:         adapters.OpenAIEmbeddings,
		UpstreamPath: "/v1/embeddings",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		BytePass:     true,
	}).Build()

	anthropicSpec := (&adapter.Spec{
		Name:         adapters.Anthropic,
		UpstreamPath: "/v1/messages",
		Auth:         adapter.AuthStrategy{Header: "x-api-key"},
		Translator:   stubV1Translator{},
	}).Build()

	// Canonical inbound shape — real identity translator (not a stub).
	canonicalSpec := (&adapter.Spec{
		Name:       adapters.Canonical,
		Translator: pkgrelay.IdentityTranslator{},
	}).Build()

	return adapter.NewRegistry(openaiSpec, responsesSpec, embeddingsSpec, anthropicSpec, canonicalSpec)
}

func buildDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	limiter := pkgratelimit.New(kvStore, nil, nil)
	pl := &pipeline.Pipeline{Logger: nil}

	reg := buildTestRegistry()

	return Deps{
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Proxy:    proxy.New(limiter, nil, nil),
		Adapters: reg.AdapterMap(),
		Specs:    reg,
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

// TestDispatch_RoutingFailure_EmitsUsageEvent verifies that a request
// rejected during routing (model not resolvable) — i.e. before any runner
// is invoked — still fires a failure usage event off the Context minted at
// dispatch entry. This is the routing-stage capture the runner-side
// failure firing alone could not reach.
func TestDispatch_RoutingFailure_EmitsUsageEvent(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)

	var gotKind string
	var wg sync.WaitGroup
	wg.Add(1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		gotKind = ev.ErrorKind
		wg.Done()
		return nil, nil
	}})
	d.Lifecycle = reg

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"no-such-model"}`),
		ModelName: "no-such-model",
		Stream:    false,
	})

	if w.Code < 400 {
		t.Fatalf("status = %d, want a 4xx/5xx routing rejection", w.Code)
	}
	wg.Wait()
	if gotKind == "" {
		t.Fatal("routing failure fired a usage event with empty ErrorKind")
	}
}

func TestInjectRelayUsage(t *testing.T) {
	ru := &pkgrelay.RelayUsage{RequestID: "r1", Attempts: 2}

	cases := []struct {
		name       string
		body       string
		wantInject bool
	}{
		{"object", `{"id":"x","object":"chat.completion"}`, true},
		{"empty object", `{}`, true},
		{"trailing newline", "{\"id\":\"x\"}\n", true},
		{"array not object", `[1,2,3]`, false},
		{"bare string", `"nope"`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := injectRelayUsage([]byte(tc.body), ru)
			if !tc.wantInject {
				if string(out) != tc.body {
					t.Fatalf("non-object should be unchanged: got %q", out)
				}
				return
			}
			// Must be valid JSON with relay_usage carrying our fields, and the
			// original keys preserved.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("injected body not valid JSON: %v — %s", err, out)
			}
			raw, ok := m["relay_usage"]
			if !ok {
				t.Fatalf("relay_usage not present: %s", out)
			}
			var got pkgrelay.RelayUsage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("relay_usage not parseable: %v", err)
			}
			if got.RequestID != "r1" || got.Attempts != 2 {
				t.Fatalf("relay_usage fields lost: %+v", got)
			}
		})
	}
}

// TestDispatch_Responses_OpenAIProperHost_BytePass verifies that
// Inbound=OpenAIResponses on host "openai" (Adapter=OpenAI) takes the
// byte-pass path (IsNativePath returns true). The pipeline fails on the stub
// adapter, but the cross-shape "translate_request" error does NOT fire.
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
		case "translate_request", "invalid_responses_request":
			t.Fatalf("byte-pass path should not have hit a cross-shape error: %q (%s)", e.Error.Code, e.Error.Message)
		}
	}
}

// TestDispatch_Responses_OpenAICompatHost_CrossShape verifies that a host
// with Adapter=OpenAI but Meta.Name != "openai" (Ollama, Groq, Together, etc.)
// falls through to the cross-shape canonical chain. With stubV1Translator
// returning an error on ParseRequest, we get a 400 translate_request error —
// which proves dispatch tried to translate (not byte-pass).
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

	// stubV1Translator.ParseRequest always errors → translate_request 400.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (translate_request), got %d; body: %s", w.Code, w.Body.String())
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "translate_request" {
		t.Errorf("error code: want translate_request, got %q", e.Error.Code)
	}
}

// TestDispatch_Responses_AnthropicHost_CrossShape verifies that a host with
// Adapter=Anthropic also routes to the cross-shape canonical chain.
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

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (translate_request), got %d; body: %s", w.Code, w.Body.String())
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "translate_request" {
		t.Errorf("error code: want translate_request, got %q", e.Error.Code)
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

// TestDispatch_CanonicalInbound_ReachesUpstream verifies canonical inbound
// (identity translator) takes the cross-shape chain: v1.Parse succeeds on a
// valid canonical body (no translate_request), then the upstream leg is
// reached. The stub upstream translator errors on SerializeRequest, surfacing
// marshal_request — which proves the canonical request flowed into the upstream
// serialize step (where the real Anthropic translator would emit cache_control).
func TestDispatch_CanonicalInbound_ReachesUpstream(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Canonical,
		Body:      []byte(`{"model":"test-model","input":"hi","cache_config":{"instructions":true}}`),
		ModelName: "test-model",
		Stream:    false,
	})

	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code == "translate_request" {
		t.Fatalf("canonical identity ParseRequest should succeed, got translate_request: %s", e.Error.Message)
	}
	if e.Error.Code != "marshal_request" {
		t.Errorf("want marshal_request (reached upstream serialize), got %q (status %d)", e.Error.Code, w.Code)
	}
}

// TestDispatch_CanonicalInbound_InvalidBody surfaces v1.Parse errors as
// translate_request — proving the identity translator's parse path is wired.
func TestDispatch_CanonicalInbound_InvalidBody(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	// Valid JSON but no input → v1.Parse rejects it.
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Canonical,
		Body:      []byte(`{"model":"test-model"}`),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "translate_request" {
		t.Errorf("error code: want translate_request, got %q", e.Error.Code)
	}
}

// TestStreamCanonical_StampsReasoningSpan feeds a canonical SSE stream with
// a reasoning item through streamCanonical and asserts the reasoning span is
// stamped onto the lifecycle Context (start on first reasoning frame, end on
// the last), independent of usage-echo.
func TestStreamCanonical_StampsReasoningSpan(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	d.Lifecycle = lifecycle.New()

	// Anchor in the past so elapsed marks are unambiguously > 0.
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now().Add(-time.Millisecond))
	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	r = r.WithContext(lifecycle.ContextWith(r.Context(), lc))
	w := httptest.NewRecorder()

	stream := "" +
		"event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n" +
		"event: item.started\ndata: {\"item_id\":\"r1\",\"item_type\":\"reasoning\",\"index\":0}\n\n" +
		"event: item.delta\ndata: {\"item_id\":\"r1\",\"kind\":\"reasoning\",\"delta\":\"think\"}\n\n" +
		"event: item.completed\ndata: {\"item_id\":\"r1\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"summary\":[{\"text\":\"t\"}]}}\n\n" +
		"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"hi\"}\n\n" +
		"event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n"

	identity := func(b []byte) ([]byte, error) { return b, nil }
	streamCanonical(d, w, r, io.NopCloser(strings.NewReader(stream)), false /*echo*/, true /*trackReasoning*/, identity, identity)

	rz := lc.Timing.Reasoning
	if rz.Start <= 0 {
		t.Fatalf("reasoning start not stamped: %v", rz.Start)
	}
	if rz.End < rz.Start {
		t.Fatalf("reasoning end %v before start %v", rz.End, rz.Start)
	}
}

// TestStreamCanonical_NoReasoning_NoSpan confirms a stream with no reasoning
// frames leaves the reasoning span zero (so buildEvent omits it).
func TestStreamCanonical_NoReasoning_NoSpan(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	d.Lifecycle = lifecycle.New()

	lc := lifecycle.NewContext("req-2", "pipeline", time.Now().Add(-time.Millisecond))
	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	r = r.WithContext(lifecycle.ContextWith(r.Context(), lc))
	w := httptest.NewRecorder()

	stream := "" +
		"event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n" +
		"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"hi\"}\n\n" +
		"event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n"

	identity := func(b []byte) ([]byte, error) { return b, nil }
	streamCanonical(d, w, r, io.NopCloser(strings.NewReader(stream)), false, true, identity, identity)

	if rz := lc.Timing.Reasoning; rz.Start != 0 || rz.End != 0 {
		t.Fatalf("expected zero reasoning span, got %+v", rz)
	}
}

func TestExtractModelStream_StreamSignals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"canonical output_mode stream", `{"model":"m","output_mode":"stream"}`, true},
		{"canonical output_mode sync", `{"model":"m","output_mode":"sync"}`, false},
		{"vendor stream true", `{"model":"m","stream":true}`, true},
		{"vendor stream false", `{"model":"m","stream":false}`, false},
		{"neither", `{"model":"m"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stream, err := extractModelStream([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if stream != tc.want {
				t.Errorf("stream = %v, want %v", stream, tc.want)
			}
		})
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
			t.Fatalf("guard fired on standard OpenAI request (got %q)", e.Error.Code)
		}
	}
}

// okUpstreamTranslator parses any body into a fixed canonical Response with a
// FLAT orthogonal-meter usage map — the shape relay's real adapters produce.
type okUpstreamTranslator struct{ stubV1Translator }

func (okUpstreamTranslator) ParseResponse(_ []byte) (*pkgrelay.Response, error) {
	return &pkgrelay.Response{
		Object: "response",
		Status: pkgrelay.StatusCompleted,
		Usage:  pkgusage.Tokens{"input": 10, "output": 5},
	}, nil
}

// rawUpstreamBody is a vendor (OpenAI/Ollama-cloud) sync body whose usage block
// carries NESTED detail objects — the shape that crashes a canonical client's
// usage.Tokens (map[string]int64) decode.
const rawUpstreamBody = `{"id":"x","object":"chat.completion","model":"gpt-oss:120b-cloud",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0},` +
	`"completion_tokens_details":{"reasoning_tokens":0}}}`

// TestBufferCanonical_NeverForwardsRawVendorBody is the contract guard: when the
// upstream translator fails to parse the response, the canonical caller must get
// a canonical ERROR — never the raw vendor body. The pre-fix code fell back to
// out := raw, leaking the nested-usage vendor shape and crashing canonical
// clients with "cannot unmarshal object into int64". See codebase rule 11.
func TestBufferCanonical_NeverForwardsRawVendorBody(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	body := io.NopCloser(strings.NewReader(rawUpstreamBody))

	// stubV1Translator.ParseResponse always errors → exercises the fallback path.
	bufferCanonical(Deps{}, rec, r, body, http.StatusOK, false, nil,
		stubV1Translator{}, pkgrelay.IdentityTranslator{})

	res := rec.Result()
	out := rec.Body.Bytes()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
	if strings.Contains(string(out), "prompt_tokens_details") {
		t.Fatalf("raw vendor body leaked to canonical caller: %s", out)
	}
	e := parseDispatchErr(t, out)
	if e.Error.Code != "translate_response" {
		t.Fatalf("error code = %q, want translate_response (body: %s)", e.Error.Code, out)
	}
}

// TestBufferCanonical_FlatUsage is the happy path: a parseable upstream body is
// translated to canonical with the FLAT usage map a canonical client can decode.
func TestBufferCanonical_FlatUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	body := io.NopCloser(strings.NewReader(rawUpstreamBody))

	bufferCanonical(Deps{}, rec, r, body, http.StatusOK, false, nil,
		okUpstreamTranslator{}, pkgrelay.IdentityTranslator{})

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}
	var resp struct {
		Usage pkgusage.Tokens `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("canonical body must decode into usage.Tokens, got: %v (body: %s)", err, rec.Body.Bytes())
	}
	if resp.Usage["input"] != 10 || resp.Usage["output"] != 5 {
		t.Fatalf("usage not flat-canonical: %v", resp.Usage)
	}
}

// TestForwardHeaders_DropsAcceptEncoding guards the content-coding fix: the
// relay must not forward the caller's Accept-Encoding upstream, so Go's
// transport transparently decompresses and the canonical translate path always
// receives a parseable, plain body (and never emits a stale Content-Encoding).
func TestForwardHeaders_DropsAcceptEncoding(t *testing.T) {
	in := http.Header{}
	in.Set("Accept-Encoding", "gzip")
	in.Set("Content-Type", "application/json")

	out := forwardHeaders(in)

	if got := out.Get("Accept-Encoding"); got != "" {
		t.Fatalf("Accept-Encoding should be stripped, got %q", got)
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type should pass through, got %q", got)
	}
}
