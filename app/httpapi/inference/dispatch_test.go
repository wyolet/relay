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
	pkgusage "github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/slug"
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
// hostName (Meta.Name) with adapter=openai. Returns the catalog and the
// relay key that authorises access.
func buildDispatchCatalog(t *testing.T, hostName string) (*catalog.Catalog, *relaykey.RelayKey) {
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
		Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Hosts:     []model.HostBinding{{HostID: hostID, Adapter: adapters.OpenAI}},
			Snapshots: []model.Snapshot{{Name: slug.From("gpt-4o")}},
			Pointer:   slug.From("gpt-4o"),
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
func (stubAdapter) ExtractTokens(_ []byte) pkgusage.Tokens            { return nil }
func (stubAdapter) Retryable(_ *http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

// stubTranslator satisfies adapters.Translator using pkg openai identity.
// It's an adapters.Identity wrapper — avoids importing app/adapters/openai.
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
			adapters.OpenAI:          stubAdapter{},
			adapters.OpenAIResponses: stubAdapter{},
		},
		Translators: adapters.Registry{
			adapters.OpenAI:          stubTranslator{},
			adapters.OpenAIResponses: stubTranslator{},
		},
	}
}

// withNormalContext injects a ModeNormal classification and relay key into r's context.
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

// TestDispatch_ResponsesGuard_NonOpenAIHost verifies that
// Inbound=OpenAIResponses on a non-"openai" host returns 400
// responses_unsupported_host.
func TestDispatch_ResponsesGuard_NonOpenAIHost(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "groq") // host.Meta.Name = "groq"
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"gpt-4o","stream":false}`),
		ModelName: "gpt-4o",
		Stream:    false,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
	e := parseDispatchErr(t, w.Body.Bytes())
	if e.Error.Code != "responses_unsupported_host" {
		t.Errorf("error code: want responses_unsupported_host, got %q", e.Error.Code)
	}
}

// TestDispatch_ResponsesGuard_OpenAIHost verifies that
// Inbound=OpenAIResponses on host "openai" passes the guard and reaches
// the pipeline (which fails because the upstream is unreachable, but
// that's a different error — the guard itself does not fire).
func TestDispatch_ResponsesGuard_OpenAIHost(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai") // host.Meta.Name = "openai"
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      []byte(`{"model":"gpt-4o","stream":false}`),
		ModelName: "gpt-4o",
		Stream:    false,
	})

	// Guard passed: status is NOT 400 responses_unsupported_host.
	// The pipeline hits the unreachable upstream and returns 502.
	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "responses_unsupported_host" {
			t.Fatalf("guard fired for host 'openai' but should not have")
		}
	}
}

// TestDispatch_NormalOpenAI_UnaffectedByGuard confirms that a standard
// Inbound=OpenAI request on a non-"openai" host (e.g. "groq") is not
// blocked by the Phase 1 guard (the guard is Responses-only).
func TestDispatch_NormalOpenAI_UnaffectedByGuard(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "groq")
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"gpt-4o","stream":false}`),
		ModelName: "gpt-4o",
		Stream:    false,
	})

	if w.Code == http.StatusBadRequest {
		e := parseDispatchErr(t, w.Body.Bytes())
		if e.Error.Code == "responses_unsupported_host" {
			t.Fatalf("Responses guard fired on standard OpenAI request — guard is shape-conditional")
		}
	}
}
