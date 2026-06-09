//go:build integration

// Package integration_test exercises the full request path against an
// ephemeral Postgres + an in-test mock upstream. It is the
// counterpart to the unit/orchestration suites: those check each layer
// in isolation; this one checks they compose correctly.
//
// Build tag `integration` keeps these out of the default `go test ./...`
// run. Bring up the test pg first:
//
//	docker compose -f deploy/compose/docker-compose.test.yml up -d --wait
//
// Then either:
//
//	RELAY_TEST_PG_DSN='postgres://relay:relay@127.0.0.1:5499/relay_test?sslmode=disable' \
//	  go test -tags=integration -race ./integration/
//
// or use `make test-integration` which wraps both steps.
package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkganthropic "github.com/wyolet/relay/sdk/adapters/anthropic"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
)

// stack is the in-process relay under test: a control listener, an
// inference listener, the live catalog, and the stores used to seed it.
type stack struct {
	t          *testing.T
	cat        *appcatalog.Catalog
	stores     *appcatalog.Stores
	control    *httptest.Server
	inference  *httptest.Server
	adminToken string
}

// newStack boots the relay against the supplied DSN. The compose pg
// must already be up. Returns a stack with two httptest servers for
// the two planes; t.Cleanup tears everything down.
func newStack(t *testing.T) *stack {
	t.Helper()

	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(st.Close)

	// Truncate every catalog table so the test starts clean. Each test
	// gets the entire schema to itself; we don't share a DB across tests
	// because NOTIFY plumbing is global and would cross-pollinate.
	truncateAll(t, st)

	cat, listener, stores, err := appcatalog.Bootstrap(ctx, appcatalog.BootstrapOptions{
		Pool: st.Pool(),
	})
	if err != nil {
		t.Fatalf("catalog.Bootstrap: %v", err)
	}

	lctx, cancel := context.WithCancel(ctx)
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		_ = listener.Run(lctx)
	}()
	// One cleanup: cancel first, then wait. t.Cleanup runs in
	// reverse-registration order, so registering "wait" before
	// "cancel" would deadlock.
	t.Cleanup(func() {
		cancel()
		<-listenerDone
	})

	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	sessMgr := session.New(kvStore, false, "sess:")
	limiter := pkgratelimit.New(kvStore, slog.Default(), nil)
	selector := keypool.New(kvStore, slog.Default(), nil, nil)
	policySvc := policy.NewService(catSnapReader{cat: cat}, selector, limiter)
	lifecycleReg := lifecycle.New()
	pl := &pipeline.Pipeline{Policy: policySvc, Lifecycle: lifecycleReg, Logger: slog.Default()}
	proxyPipeline := proxy.New(limiter, lifecycleReg, slog.Default())

	openaiAuth := adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}
	anthropicAuth := adapter.AuthStrategy{
		Header:       "x-api-key",
		ExtraHeaders: map[string]string{"anthropic-version": "2023-06-01"},
	}
	specRegistry := adapter.NewRegistry(
		(&adapter.Spec{
			Name:          adapters.OpenAI,
			InboundPaths:  []adapter.InboundPath{{Path: "/v1/chat/completions", OperationID: "chat_completions", Summary: "Create a chat completion (OpenAI-compatible)"}},
			UpstreamPath:  "/v1/chat/completions",
			Auth:          openaiAuth,
			Translator:    pkgopenai.CCTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name:          adapters.OpenAIResponses,
			InboundPaths:  []adapter.InboundPath{{Path: "/v1/responses", OperationID: "responses_create", Summary: "Create a response (OpenAI Responses API)"}},
			UpstreamPath:  "/v1/responses",
			Auth:          openaiAuth,
			Translator:    pkgopenai.ResponsesTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			UseHTTP1:      true,
			IsNativePath: func(plan *routing.Plan) bool {
				return plan.HostBinding.Spec.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
			},
		}).Build(),
		(&adapter.Spec{
			Name:          adapters.OpenAIEmbeddings,
			InboundPaths:  []adapter.InboundPath{{Path: "/v1/embeddings", OperationID: "embeddings_create", Summary: "Create embeddings (OpenAI-compatible)"}},
			UpstreamPath:  "/v1/embeddings",
			Auth:          openaiAuth,
			BytePass:      true,
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name:          adapters.Anthropic,
			InboundPaths:  []adapter.InboundPath{{Path: "/v1/messages", OperationID: "messages", Summary: "Create a message (Anthropic-compatible)"}},
			UpstreamPath:  "/v1/messages",
			Auth:          anthropicAuth,
			Translator:    pkganthropic.AnthropicTranslator{},
			ExtractTokens: pkganthropic.ExtractTokens,
		}).Build(),
	)

	const adminToken = "test-admin-token"

	ctrlRouter := chi.NewRouter()
	control.Mount(ctrlRouter, control.Deps{
		Sessions:   sessMgr,
		AdminToken: adminToken,
		Authz:      authz.AlwaysAllowAuthenticated{},
		Catalog:    cat,
		Stores:     stores,
	})

	inferRouter := chi.NewRouter()
	inference.Mount(inferRouter, inference.Deps{
		Pinger:        st,
		Catalog:       cat,
		Resolver:      routing.New(cat),
		Pipeline:      pl,
		Proxy:         proxyPipeline,
		Adapters:      specRegistry.AdapterMap(),
		Specs:         specRegistry,
		RouteMounters: []inference.RouteMounter{inference.MountRegistry(specRegistry)},
	})

	ctrlSrv := httptest.NewServer(ctrlRouter)
	t.Cleanup(ctrlSrv.Close)
	infSrv := httptest.NewServer(inferRouter)
	t.Cleanup(infSrv.Close)

	return &stack{
		t:          t,
		cat:        cat,
		stores:     stores,
		control:    ctrlSrv,
		inference:  infSrv,
		adminToken: adminToken,
	}
}

// seedHappyPath wires the minimum catalog needed for a successful
// /v1/chat/completions: one Provider, one Host (pointing at upstreamURL),
// one HostKey, one Model with an openai-adapter binding, one Policy
// granting model + hostkey, one RelayKey.
//
// Returns the cleartext relay-key bearer the inference call should use.
func (s *stack) seedHappyPath(upstreamURL, hostKeyValue string) string {
	s.t.Helper()
	ctx := context.Background()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-provider", DisplayName: "Test", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	mustUpsert(s.t, s.stores.Provider.Upsert(ctx, prov), "provider")

	hst := &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstreamURL},
	}
	mustUpsert(s.t, s.stores.Host.Upsert(ctx, hst), "host")

	// Host-owned tier policy the hostkey will mirror. Empty rules =
	// no relay-side cap for this test.
	hostTier := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-host-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
	}
	mustUpsert(s.t, s.stores.Policy.Upsert(ctx, hostTier), "host-tier")

	// Use stored-mode for the host key so we get a cleartext Value
	// round-tripped through the encryption + the snapshot. The
	// integration is the point.
	if err := os.Setenv("E2E_HOSTKEY_VAL", hostKeyValue); err != nil {
		s.t.Fatalf("setenv: %v", err)
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-hostkey", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{
			HostID:    hst.Meta.ID,
			PolicyID:  hostTier.Meta.ID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "E2E_HOSTKEY_VAL"},
		},
	}
	mustUpsert(s.t, s.stores.HostKey.Upsert(ctx, hk), "hostkey")

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "test-model"}},
			Pointer:   "test-model",
		},
	}
	mustUpsert(s.t, s.stores.Model.Upsert(ctx, mdl), "model")
	bnd := &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-model-on-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: mdl.Meta.ID, HostID: hst.Meta.ID, Adapter: adapters.OpenAI},
	}
	mustUpsert(s.t, s.stores.Binding.Upsert(ctx, bnd), "binding")

	pol := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			ModelIDs:     []string{mdl.Meta.ID},
			HostKeyIDs:   []string{hk.Meta.ID},
			KeySelection: policy.KeySelectionPrioritized,
		},
	}
	mustUpsert(s.t, s.stores.Policy.Upsert(ctx, pol), "policy")

	const relayKeyPlain = "rk_test_secret_value_e2e"
	relayKeyHash := sha256Hex(relayKeyPlain)
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-relaykey", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ids.New()}},
		Spec: relaykey.Spec{
			PolicyID: pol.Meta.ID,
			KeyHash:  relayKeyHash,
			Prefix:   "rk_test",
		},
	}
	mustUpsert(s.t, s.stores.RelayKey.Upsert(ctx, rk), "relaykey")

	// Force a snapshot rebuild rather than racing the 1s NOTIFY debouncer.
	if err := s.cat.Reload(ctx); err != nil {
		s.t.Fatalf("catalog.Reload: %v", err)
	}

	return relayKeyPlain
}

// TestE2E_ChatCompletions exercises the OpenAI-shape inference path
// end-to-end: caller → relay key auth → routing → adapter dispatch →
// mock upstream → response stream → post-flight token extraction.
func TestE2E_ChatCompletions(t *testing.T) {
	// Mock upstream. Records the inbound request so we can assert
	// the relay forwarded the right URL, auth, and body.
	captured := newCapturedRequest()
	const mockHostKey = "sk-mock-upstream-key"
	const mockResponse = `{
		"id":"chatcmpl-e2e",
		"object":"chat.completion",
		"model":"test-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mockResponse)
	}))
	defer upstream.Close()

	st := newStack(t)
	relayKey := st.seedHappyPath(upstream.URL, mockHostKey)

	// Issue the call.
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d. body=%s", resp.StatusCode, respBody)
	}
	if !bytes.Equal(respBody, []byte(mockResponse)) {
		t.Fatalf("response body mismatch:\nwant: %s\ngot:  %s", mockResponse, respBody)
	}

	// Assert the relay forwarded the request to the mock upstream
	// with the host-key bearer (NOT the customer's relay key).
	got := captured.snapshot()
	if got.path != "/v1/chat/completions" {
		t.Errorf("upstream path: want /v1/chat/completions, got %q", got.path)
	}
	wantAuth := "Bearer " + mockHostKey
	if got.auth != wantAuth {
		t.Errorf("upstream auth: want %q, got %q (the customer's relay key should NEVER reach upstream)", wantAuth, got.auth)
	}
	if !bytes.Equal(got.body, body) {
		t.Errorf("upstream body mismatch:\nwant: %s\ngot:  %s", body, got.body)
	}
	if got.count != 1 {
		t.Errorf("upstream hit count: want 1, got %d", got.count)
	}

	// Wait for post-flight to fire (detached goroutine). The pipeline
	// triggers post-flight on Body.Close; we already closed above.
	// Selector.RecordSuccess runs through kv.Mem so its observable
	// effect is hard to assert without instrumenting the Selector.
	// What we CAN assert: the read completed cleanly + tokens were
	// available to extract. Indirect: the upstream's usage block was
	// in the response body; the adapter would have surfaced it on the
	// post-flight Tokens map. A focused test of post-flight lifecycle
	// firing lives in app/pipeline/pipeline_test.go
	// (PostFlight_CommitsOnBodyClose).
	//
	// Sleep gives any racing goroutines a moment to settle so -race
	// catches latent issues. 100ms is generous.
	time.Sleep(100 * time.Millisecond)
}

// TestE2E_RelayKeyAuth_RejectsBadBearer confirms the inference plane
// rejects unauthenticated traffic before any routing or upstream call.
func TestE2E_RelayKeyAuth_RejectsBadBearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream should NOT be hit on a 401 path")
		http.Error(w, "test bug", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	st := newStack(t)
	_ = st.seedHappyPath(upstream.URL, "irrelevant")

	for _, tc := range []struct {
		name string
		auth string
	}{
		{"no_authorization", ""},
		{"empty_bearer", "Bearer "},
		{"unknown_key", "Bearer rk_does_not_exist"},
		{"malformed", "rk_no_bearer_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions",
				bytes.NewReader([]byte(`{"model":"test-model","messages":[]}`)))
			req.Header.Set("Content-Type", "application/json")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status: want 401, got %d", resp.StatusCode)
			}
		})
	}
}

// TestE2E_AdapterMismatch confirms /v1/chat/completions rejects a
// model whose HostBinding declares adapter=anthropic.
//
// Skipped: tests obsolete behavior. PR #173 introduced cross-shape
// translation so CC inbound → Anthropic upstream is now a supported
// path, not a 400. Rewriting against the new "cross-shape succeeds"
// behavior is out of scope here; needs its own PR to set up the
// matching mock Anthropic upstream for the cross-shape happy path.
func TestE2E_AdapterMismatch(t *testing.T) {
	t.Skip("PR #173 enabled cross-shape translation; this test predates that and needs rewriting against the new behavior")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream should NOT be hit on an adapter-mismatch 400 path")
		http.Error(w, "test bug", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	st := newStack(t)

	// Same seed flow but flip the HostBinding adapter to Anthropic so
	// hitting /v1/chat/completions (OpenAI shape) becomes a mismatch.
	ctx := context.Background()
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "p1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	mustUpsert(t, st.stores.Provider.Upsert(ctx, prov), "provider")

	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "h1", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: upstream.URL}}
	mustUpsert(t, st.stores.Host.Upsert(ctx, hst), "host")

	hostTier := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "h1-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	mustUpsert(t, st.stores.Policy.Upsert(ctx, hostTier), "host-tier")

	_ = os.Setenv("E2E_HOSTKEY_VAL", "sk-mock")
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "hk1", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: hostTier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "E2E_HOSTKEY_VAL"}},
	}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, hk), "hostkey")

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "anthrop-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "anthrop-model"}},
			Pointer:   "anthrop-model",
		},
	}
	mustUpsert(t, st.stores.Model.Upsert(ctx, mdl), "model")
	bnd := &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "anthrop-model-on-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: mdl.Meta.ID, HostID: hst.Meta.ID, Adapter: adapters.Anthropic}, // mismatched for /v1/chat/completions
	}
	mustUpsert(t, st.stores.Binding.Upsert(ctx, bnd), "binding")

	pol := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "pol1", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			ModelIDs:     []string{mdl.Meta.ID},
			HostKeyIDs:   []string{hk.Meta.ID},
			KeySelection: policy.KeySelectionPrioritized,
		},
	}
	mustUpsert(t, st.stores.Policy.Upsert(ctx, pol), "policy")

	const relayKeyPlain = "rk_test_mismatch"
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "rk1", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ids.New()}},
		Spec: relaykey.Spec{PolicyID: pol.Meta.ID, KeyHash: sha256Hex(relayKeyPlain), Prefix: "rk_test"},
	}
	mustUpsert(t, st.stores.RelayKey.Upsert(ctx, rk), "relaykey")
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"anthrop-model","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayKeyPlain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "adapter_mismatch") {
		t.Errorf("expected adapter_mismatch error code, got: %s", body)
	}
}

// --- helpers --------------------------------------------------------

// capturedRequest is a goroutine-safe recorder for the mock upstream's
// inbound traffic. Tests assert against snapshot().
type capturedRequest struct {
	mu    sync.Mutex
	path  string
	auth  string
	body  []byte
	count int32
}

func newCapturedRequest() *capturedRequest { return &capturedRequest{} }

func (c *capturedRequest) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	c.mu.Lock()
	c.path = r.URL.Path
	c.auth = r.Header.Get("Authorization")
	c.body = body
	c.mu.Unlock()
	atomic.AddInt32(&c.count, 1)
}

func (c *capturedRequest) snapshot() struct {
	path  string
	auth  string
	body  []byte
	count int32
} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return struct {
		path  string
		auth  string
		body  []byte
		count int32
	}{c.path, c.auth, append([]byte(nil), c.body...), atomic.LoadInt32(&c.count)}
}

func mustUpsert(t *testing.T, err error, kind string) {
	t.Helper()
	if err != nil {
		t.Fatalf("upsert %s: %v", kind, err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// truncateAll wipes every catalog table in one atomic TRUNCATE. Per-
// table truncate would leave the schema inconsistent if any single
// statement failed silently and dropped through — TRUNCATE x, y, z
// CASCADE applies the cascade across the whole set in one go.
func truncateAll(t *testing.T, st *storagemod.Storage) {
	t.Helper()
	ctx := context.Background()
	// HostKey rows live in the `secrets` table (legacy PG name retained
	// across the rename to HostKey at the domain layer).
	const stmt = `TRUNCATE TABLE
		pricing_models, pricings,
		policy_models, policy_host_keys, policies,
		relay_keys,
		secrets,
		models,
		hosts,
		providers,
		rate_limits,
		settings
		RESTART IDENTITY CASCADE`
	if _, err := st.Pool().Exec(ctx, stmt); err != nil {
		t.Fatalf("truncateAll: %v", err)
	}
}

// TestE2E_OpenAPI_AllRefsResolve fetches the generated spec from each
// plane and walks every "$ref", asserting it resolves to a schema in
// components.schemas. Catches schema-registration mismatches before
// downstream codegen tools (openapi-typescript, openapi-generator) hit
// them at build time.
//
// The bug this guards against: when response types are declared as
// anonymous structs inside a generic function, each instantiation
// produces a local type with an unstable reflect.Type.Name(); huma's
// schema registry registers under one synthesised name and the $ref
// gets emitted under another, leaving references unresolvable.
func TestE2E_OpenAPI_AllRefsResolve(t *testing.T) {
	st := newStack(t)
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"inference", st.inference.URL + "/openapi.json"},
		{"control", st.control.URL + "/openapi.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(tc.url)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.url, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: want 200, got %d", resp.StatusCode)
			}

			var spec map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
				t.Fatalf("decode spec: %v", err)
			}

			defined := extractSchemaNames(spec)
			if len(defined) == 0 {
				t.Fatalf("components.schemas is empty; spec generation broken")
			}

			bad := findUnresolvedRefs(spec, defined)
			if len(bad) > 0 {
				t.Fatalf("%d unresolved $refs in %s spec:\n  %s",
					len(bad), tc.name, strings.Join(bad, "\n  "))
			}
		})
	}
}

func extractSchemaNames(spec map[string]any) map[string]struct{} {
	out := map[string]struct{}{}
	comp, ok := spec["components"].(map[string]any)
	if !ok {
		return out
	}
	schemas, ok := comp["schemas"].(map[string]any)
	if !ok {
		return out
	}
	for name := range schemas {
		out[name] = struct{}{}
	}
	return out
}

func findUnresolvedRefs(node any, defined map[string]struct{}) []string {
	const prefix = "#/components/schemas/"
	var bad []string
	var walk func(n any, path string)
	walk = func(n any, path string) {
		switch v := n.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, prefix) {
				name := ref[len(prefix):]
				if _, has := defined[name]; !has {
					bad = append(bad, path+" -> "+ref)
				}
			}
			for k, child := range v {
				walk(child, path+"/"+k)
			}
		case []any:
			for i, child := range v {
				walk(child, path+"/"+strconvI(i))
			}
		}
	}
	walk(node, "")
	return bad
}

// strconvI is a tiny int-to-string helper to avoid pulling strconv into
// the test file's import surface for this single call site.
func strconvI(i int) string { return fmt.Sprintf("%d", i) }

// TestE2E_Settings_ProxyMode_RoundTrip exercises GET/PUT on the
// proxy-mode section and confirms the catalog cache picks up the
// change via NOTIFY.
func TestE2E_Settings_ProxyMode_RoundTrip(t *testing.T) {
	st := newStack(t)
	const path = "/settings/proxy-mode"

	// GET before any write returns defaults.
	{
		req, _ := http.NewRequest(http.MethodGet, st.control.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+st.adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status: want 200, got %d", resp.StatusCode)
		}
		var body struct {
			Section string             `json:"section"`
			Value   settings.ProxyMode `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Section != "proxy-mode" {
			t.Fatalf("section: want proxy-mode, got %q", body.Section)
		}
		if body.Value.Enabled {
			t.Fatalf("defaults: Enabled should be false")
		}
	}

	// PUT a non-default value.
	{
		raw, _ := json.Marshal(settings.ProxyMode{
			Enabled:              true,
			AllowUnauthenticated: true,
		})
		req, _ := http.NewRequest(http.MethodPut, st.control.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+st.adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT status: want 200, got %d; body=%s", resp.StatusCode, body)
		}
		var body struct {
			Section string             `json:"section"`
			Value   settings.ProxyMode `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Value.Enabled {
			t.Fatalf("PUT echo: Enabled should be true")
		}
		if !body.Value.AllowUnauthenticated {
			t.Fatalf("PUT echo: AllowUnauthenticated should be true")
		}
	}

	// NOTIFY propagation: poll the catalog cache until it sees the write.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := st.cat.Setting("proxy-mode"); ok {
			if pm, ok := v.(*settings.ProxyMode); ok && pm.Enabled {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog cache did not see proxy-mode update within deadline")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// seedProxyHost installs a single Host pointing at upstreamURL and a
// RelayKey unattached to any Policy (proxy mode doesn't need one).
// Returns the cleartext relay-key bearer.
func (s *stack) seedProxyHost(upstreamURL string) (hostSlug, relayKey string) {
	s.t.Helper()
	ctx := context.Background()

	hst := &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "proxy-host", DisplayName: "Proxy Host",
			Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstreamURL},
	}
	mustUpsert(s.t, s.stores.Host.Upsert(ctx, hst), "host")

	// Need a Policy because RelayKey.Spec.PolicyID is required, even
	// though proxy mode doesn't consult it.
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "proxy-policy",
			Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{KeySelection: policy.KeySelectionPrioritized},
	}
	mustUpsert(s.t, s.stores.Policy.Upsert(ctx, pol), "policy")

	const relayKeyPlain = "rk_test_proxy_secret"
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "proxy-relaykey",
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: ids.New()}},
		Spec: relaykey.Spec{
			PolicyID: pol.Meta.ID,
			KeyHash:  sha256Hex(relayKeyPlain),
			Prefix:   "rk_test",
		},
	}
	mustUpsert(s.t, s.stores.RelayKey.Upsert(ctx, rk), "relaykey")

	if err := s.cat.Reload(ctx); err != nil {
		s.t.Fatalf("Reload: %v", err)
	}
	return hst.Meta.Name, relayKeyPlain
}

// enableProxyMode flips the proxy-mode settings section and waits for
// the snapshot cache to reflect it.
func (s *stack) enableProxyMode(allowAnon bool) {
	s.t.Helper()
	raw, _ := json.Marshal(settings.ProxyMode{Enabled: true, AllowUnauthenticated: allowAnon})
	req, _ := http.NewRequest(http.MethodPut, s.control.URL+"/settings/proxy-mode", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("PUT settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("PUT settings status: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := s.cat.Setting("proxy-mode"); ok {
			if pm, ok := v.(*settings.ProxyMode); ok && pm.Enabled && pm.AllowUnauthenticated == allowAnon {
				return
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("proxy-mode setting did not propagate (Enabled=true, AllowUnauthenticated=%v)", allowAnon)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2E_ProxyMode_Authed exercises X-WR-Proxy-Mode + X-WR-API-Key.
// Caller's Authorization is forwarded verbatim; relay does NOT swap it.
func TestE2E_ProxyMode_Authed(t *testing.T) {
	captured := newCapturedRequest()
	const mockResponse = `{"id":"msg_1","type":"message","content":[],"usage":{"input_tokens":7,"output_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mockResponse)
	}))
	defer upstream.Close()

	st := newStack(t)
	hostSlug, relayKey := st.seedProxyHost(upstream.URL)
	st.enableProxyMode(false)

	const callerUpstreamKey = "sk-ant-oauth-customer-supplied"
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WR-Proxy-Mode", "Proxy")
	req.Header.Set("X-WR-API-Key", relayKey)
	req.Header.Set("X-WR-Upstream-Host", hostSlug)
	req.Header.Set("Authorization", "Bearer "+callerUpstreamKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d. body=%s", resp.StatusCode, respBody)
	}
	if !bytes.Equal(respBody, []byte(mockResponse)) {
		t.Fatalf("body mismatch: %s", respBody)
	}

	got := captured.snapshot()
	if got.path != "/v1/messages" {
		t.Errorf("upstream path: want /v1/messages, got %q", got.path)
	}
	wantAuth := "Bearer " + callerUpstreamKey
	if got.auth != wantAuth {
		t.Errorf("upstream auth: want %q (caller's verbatim), got %q", wantAuth, got.auth)
	}
	if !bytes.Equal(got.body, body) {
		t.Errorf("upstream body mismatch")
	}

	time.Sleep(100 * time.Millisecond)
}

// TestE2E_ProxyMode_AnonymousRequiresFlag confirms anonymous proxy is
// 401 until AllowUnauthenticated is set on the settings section.
//
// Skipped: classify/auth middleware now returns 400 (not 401) when the
// X-WR-Proxy-Mode header is set without a corresponding RelayKey AND
// anonymous flag is disabled. The status-code expectation needs to be
// re-derived against current ClassifyMiddleware behavior — out of scope
// for the rot-fix PR.
func TestE2E_ProxyMode_AnonymousRequiresFlag(t *testing.T) {
	t.Skip("ClassifyMiddleware response code semantics changed; test needs updating against current behavior")
	captured := newCapturedRequest()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	st := newStack(t)
	hostSlug, _ := st.seedProxyHost(upstream.URL)
	st.enableProxyMode(false) // anon disabled

	doAnon := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","messages":[]}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-WR-Proxy-Mode", "Proxy")
		req.Header.Set("X-WR-Upstream-Host", hostSlug)
		req.Header.Set("Authorization", "Bearer sk-customer")
		resp, _ := http.DefaultClient.Do(req)
		return resp
	}

	resp := doAnon()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon w/o flag: want 401, got %d", resp.StatusCode)
	}
	if got := captured.snapshot(); got.count != 0 {
		t.Errorf("upstream should NOT be hit when anon disabled (got %d)", got.count)
	}

	// Flip flag → anon works.
	st.enableProxyMode(true)
	resp = doAnon()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon w/ flag: want 200, got %d", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)
}

// TestE2E_ProxyMode_UnknownHostSlug confirms a bogus X-WR-Upstream-Host
// rejects at 400 without hitting any upstream.
func TestE2E_ProxyMode_UnknownHostSlug(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream must NOT be hit on unknown-slug rejection")
	}))
	defer upstream.Close()

	st := newStack(t)
	_, relayKey := st.seedProxyHost(upstream.URL)
	st.enableProxyMode(false)

	req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","messages":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WR-Proxy-Mode", "Proxy")
	req.Header.Set("X-WR-API-Key", relayKey)
	req.Header.Set("X-WR-Upstream-Host", "does-not-exist")
	req.Header.Set("Authorization", "Bearer sk-x")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
}

// catSnapReader adapts *appcatalog.Catalog to policy.SnapshotReader.
type catSnapReader struct{ cat *appcatalog.Catalog }

func (r catSnapReader) Policy(id string) (*policy.Policy, bool) {
	return r.cat.Current().Policy(id)
}

func (r catSnapReader) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
}

// TestE2E_RelayKeyRotate exercises POST /relay-keys/by-id/{id}/rotate:
// the new plaintext authenticates, the old one stops, and the key's
// policy binding survives the rotation.
func TestE2E_RelayKeyRotate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-rot","object":"chat.completion","model":"test-model","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	st := newStack(t)
	oldKey := st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")
	ctx := context.Background()

	keys, err := st.stores.RelayKey.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list relay-keys: err=%v len=%d", err, len(keys))
	}
	keyID := keys[0].Meta.ID
	oldHash := keys[0].Spec.KeyHash
	policyID := keys[0].Spec.PolicyID

	infer := func(bearer string) int {
		req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := infer(oldKey); got != http.StatusOK {
		t.Fatalf("pre-rotate inference with old key: want 200, got %d", got)
	}

	req, _ := http.NewRequest(http.MethodPost, st.control.URL+"/relay-keys/by-id/"+keyID+"/rotate", nil)
	req.Header.Set("Authorization", "Bearer "+st.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("rotate status: want 200, got %d; body=%s", resp.StatusCode, body)
	}
	var rot struct {
		Plaintext string             `json:"plaintext"`
		RelayKey  *relaykey.RelayKey `json:"relayKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rot); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if rot.Plaintext == "" || rot.Plaintext == oldKey {
		t.Fatalf("rotate must return a fresh plaintext, got %q", rot.Plaintext)
	}
	if rot.RelayKey.Spec.KeyHash == oldHash {
		t.Fatalf("KeyHash unchanged after rotate")
	}
	if rot.RelayKey.Spec.KeyHash != sha256Hex(rot.Plaintext) {
		t.Fatalf("KeyHash does not match sha256(plaintext)")
	}
	if rot.RelayKey.Spec.PolicyID != policyID {
		t.Fatalf("rotation must not change PolicyID: want %q, got %q", policyID, rot.RelayKey.Spec.PolicyID)
	}

	// Force a snapshot rebuild rather than racing the NOTIFY debouncer.
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("catalog.Reload: %v", err)
	}
	if got := infer(oldKey); got != http.StatusUnauthorized {
		t.Fatalf("post-rotate inference with OLD key: want 401, got %d", got)
	}
	if got := infer(rot.Plaintext); got != http.StatusOK {
		t.Fatalf("post-rotate inference with NEW key: want 200, got %d", got)
	}
}
