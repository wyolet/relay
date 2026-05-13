//go:build integration

// Package integration_test exercises the full request path against an
// ephemeral Postgres + an in-test mock upstream. It is the
// counterpart to the unit/orchestration suites: those check each layer
// in isolation; this one checks they compose correctly.
//
// Build tag `integration` keeps these out of the default `go test ./...`
// run. Bring up the test pg first:
//
//   docker compose -f deploy/compose/docker-compose.test.yml up -d --wait
//
// Then either:
//
//   RELAY_TEST_PG_DSN='postgres://relay:relay@127.0.0.1:5499/relay_test?sslmode=disable' \
//     go test -tags=integration -race ./integration/
//
// or use `make test-integration` which wraps both steps.
package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	apianthropic "github.com/wyolet/relay/app/api/anthropic"
	apiopenai "github.com/wyolet/relay/app/api/openai"
	"github.com/wyolet/relay/app/authz"
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
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/session"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
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
	pl := &pipeline.Pipeline{
		Limiter:  pkgratelimit.New(kvStore, slog.Default(), nil),
		Selector: keypool.New(kvStore, slog.Default(), nil, nil),
		Logger:   slog.Default(),
	}

	adapters := map[adapter.Kind]pipeline.Adapter{
		adapter.OpenAI:    apiopenai.New(),
		adapter.Anthropic: apianthropic.New(),
	}

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
		Pinger:   st,
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Adapters: adapters,
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

	// Use stored-mode for the host key so we get a cleartext Value
	// round-tripped through the encryption + the snapshot. The
	// integration is the point.
	if err := os.Setenv("E2E_HOSTKEY_VAL", hostKeyValue); err != nil {
		s.t.Fatalf("setenv: %v", err)
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-hostkey", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
		Spec: hostkey.Spec{
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "E2E_HOSTKEY_VAL"},
		},
	}
	mustUpsert(s.t, s.stores.HostKey.Upsert(ctx, hk), "hostkey")

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{
			Hosts: []model.HostBinding{{
				HostID:       hst.Meta.ID,
				UpstreamName: "test-model",
				Adapter:      adapter.OpenAI,
			}},
		},
	}
	mustUpsert(s.t, s.stores.Model.Upsert(ctx, mdl), "model")

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
	// post-flight Tokens map. A focused test of OnSuccess wiring lives
	// in app/pipeline/pipeline_test.go (PostFlight_CommitsOnBodyClose).
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
// model whose HostBinding declares adapter=anthropic. Cross-shape
// translation is deliberately disabled in v1.
func TestE2E_AdapterMismatch(t *testing.T) {
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

	_ = os.Setenv("E2E_HOSTKEY_VAL", "sk-mock")
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "hk1", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
		Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "E2E_HOSTKEY_VAL"}},
	}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, hk), "hostkey")

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "anthrop-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Hosts: []model.HostBinding{{
			HostID:       hst.Meta.ID,
			UpstreamName: "anthrop-model",
			Adapter:      adapter.Anthropic, // mismatched for /v1/chat/completions
		}}},
	}
	mustUpsert(t, st.stores.Model.Upsert(ctx, mdl), "model")

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
		rate_limits
		RESTART IDENTITY CASCADE`
	if _, err := st.Pool().Exec(ctx, stmt); err != nil {
		t.Fatalf("truncateAll: %v", err)
	}
}

// Silence the unused-fakeanthropic import if we add it later for
// streaming tests. Keeps the file's import surface stable.
var _ = json.Marshal
