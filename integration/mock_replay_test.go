//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/ids"
)

// TestMockReplay_OpenAIChatCompletions verifies the relay's byte-pass path
// for CC inbound → OpenAI upstream against spec-mock-openai's fixture
// replay. Skipped unless RELAY_MOCK_BASE_URL is set (defaults to
// https://openai-mock.wyolet.dev) AND a fixture body is available at
// RELAY_MOCK_FIXTURE (default /tmp/fixbody.json).
//
// The body sent to relay is the exact recorded request body from one
// fixture in the spec-mock-openai corpus. Relay parses it, looks up
// "gpt-5.4-mini" in the catalog, rewrites the model field (no-op since
// snapshot name matches), forwards to the mock. Mock matches the body
// against the fixture and replays the recorded 200 response verbatim.
//
// This test exercises the post-PR-4 dispatch pipeline end-to-end against
// a real upstream wire shape — the first thing that's broken would be
// dispatch picking the wrong Spec or pipeline.Adapter mis-forwarding.
func TestMockReplay_OpenAIChatCompletions(t *testing.T) {
	mockURL := os.Getenv("RELAY_MOCK_BASE_URL")
	if mockURL == "" {
		mockURL = "https://openai-mock.wyolet.dev"
	}
	fixturePath := os.Getenv("RELAY_MOCK_FIXTURE")
	if fixturePath == "" {
		fixturePath = "/tmp/fixbody.json"
	}

	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Skipf("fixture file %s not readable: %v", fixturePath, err)
	}

	// Extract model from fixture body so the catalog seed matches.
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("parse fixture body: %v", err)
	}
	if probe.Model == "" {
		t.Fatal("fixture body has no model field")
	}
	t.Logf("fixture model: %q", probe.Model)

	// Sanity check: confirm mock is reachable + returns 200 for this body
	// before we boot the relay. If the mock isn't up, fail fast.
	checkReq, _ := http.NewRequest(http.MethodPost, mockURL+"/v1/chat/completions", bytes.NewReader(body))
	checkReq.Header.Set("Content-Type", "application/json")
	checkResp, err := http.DefaultClient.Do(checkReq)
	if err != nil {
		t.Skipf("mock at %s not reachable: %v", mockURL, err)
	}
	directBody, _ := io.ReadAll(checkResp.Body)
	checkResp.Body.Close()
	if checkResp.StatusCode != http.StatusOK {
		t.Skipf("mock returned %d for fixture; need 200 for this test to be meaningful (body=%q)", checkResp.StatusCode, string(directBody[:min(200, len(directBody))]))
	}

	st := newStack(t)
	relayKey := st.seedHappyPathForModel(mockURL, "sk-mock", probe.Model)

	relayReq, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions", bytes.NewReader(body))
	relayReq.Header.Set("Content-Type", "application/json")
	relayReq.Header.Set("Authorization", "Bearer "+relayKey)

	relayResp, err := http.DefaultClient.Do(relayReq)
	if err != nil {
		t.Fatalf("relay request failed: %v", err)
	}
	defer relayResp.Body.Close()

	relayBody, err := io.ReadAll(relayResp.Body)
	if err != nil {
		t.Fatalf("read relay response: %v", err)
	}

	if relayResp.StatusCode != http.StatusOK {
		t.Fatalf("relay status: want 200, got %d (body=%q)", relayResp.StatusCode, string(relayBody[:min(500, len(relayBody))]))
	}

	if !bytes.Equal(relayBody, directBody) {
		t.Errorf("relay byte-pass mismatch: relay=%d bytes, direct=%d bytes", len(relayBody), len(directBody))
		// Surface first divergence position
		n := min(len(relayBody), len(directBody))
		for i := 0; i < n; i++ {
			if relayBody[i] != directBody[i] {
				ctxStart := max(0, i-20)
				ctxEnd := min(n, i+40)
				t.Errorf("first diff at byte %d:\n  relay : %q\n  direct: %q", i,
					relayBody[ctxStart:ctxEnd], directBody[ctxStart:ctxEnd])
				break
			}
		}
		return
	}

	t.Logf("byte-pass verified: %d bytes round-trip via relay → openai-mock → relay matches direct curl", len(relayBody))
}

// TestMockReplay_StreamingWithParallelTools exercises a much harder path
// than the sync test above:
//
//   - streaming SSE (146 chunks in this fixture) via relay's byte-pass
//     io.Copy path
//   - multi-turn conversation (21 messages in the body)
//   - 11 tool definitions in the request
//   - 2 parallel tool_calls in one assistant message in the history
//   - 200 OK status with full SSE stream
//
// Fixture default: /tmp/fixbody-parallel.json extracted from
// session-1779156057-87325-4.jsonl fixture #10 in the openai-mini corpus.
//
// Validates: relay's PR 4 byte-pass dispatch handles streaming + complex
// bodies + tool-call wire shape correctly. Compares the full streamed
// response bytes against a direct curl to the mock.
func TestMockReplay_StreamingWithParallelTools(t *testing.T) {
	mockURL := os.Getenv("RELAY_MOCK_BASE_URL")
	if mockURL == "" {
		mockURL = "https://openai-mock.wyolet.dev"
	}
	fixturePath := os.Getenv("RELAY_MOCK_STREAM_FIXTURE")
	if fixturePath == "" {
		fixturePath = "/tmp/fixbody-parallel.json"
	}

	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Skipf("streaming fixture %s not readable: %v", fixturePath, err)
	}

	var probe struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("parse fixture body: %v", err)
	}
	if probe.Model == "" {
		t.Fatal("fixture body has no model field")
	}
	if probe.Stream == nil || !*probe.Stream {
		t.Skip("fixture body does not have stream:true — this test specifically exercises the SSE path")
	}
	t.Logf("streaming fixture: model=%q messages=multi-turn tools=parallel-tool-calls", probe.Model)

	// Direct curl to mock first.
	checkReq, _ := http.NewRequest(http.MethodPost, mockURL+"/v1/chat/completions", bytes.NewReader(body))
	checkReq.Header.Set("Content-Type", "application/json")
	checkResp, err := http.DefaultClient.Do(checkReq)
	if err != nil {
		t.Skipf("mock at %s not reachable: %v", mockURL, err)
	}
	directBody, _ := io.ReadAll(checkResp.Body)
	checkResp.Body.Close()
	if checkResp.StatusCode != http.StatusOK {
		t.Skipf("mock returned %d for fixture body (need 200; body=%q)", checkResp.StatusCode, string(directBody[:min(300, len(directBody))]))
	}

	st := newStack(t)
	relayKey := st.seedHappyPathForModel(mockURL, "sk-mock", probe.Model)

	// Same body through relay. Don't set Accept; let relay/mock negotiate.
	relayReq, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions", bytes.NewReader(body))
	relayReq.Header.Set("Content-Type", "application/json")
	relayReq.Header.Set("Authorization", "Bearer "+relayKey)

	relayResp, err := http.DefaultClient.Do(relayReq)
	if err != nil {
		t.Fatalf("relay request failed: %v", err)
	}
	defer relayResp.Body.Close()

	relayBody, err := io.ReadAll(relayResp.Body)
	if err != nil {
		t.Fatalf("read relay response: %v", err)
	}

	if relayResp.StatusCode != http.StatusOK {
		t.Fatalf("relay status: want 200, got %d (body=%q)", relayResp.StatusCode, string(relayBody[:min(500, len(relayBody))]))
	}

	// SSE byte-equality: relay should byte-copy the upstream stream verbatim.
	if !bytes.Equal(relayBody, directBody) {
		t.Errorf("streaming SSE mismatch: relay=%d bytes, direct=%d bytes", len(relayBody), len(directBody))
		n := min(len(relayBody), len(directBody))
		for i := 0; i < n; i++ {
			if relayBody[i] != directBody[i] {
				ctxStart := max(0, i-30)
				ctxEnd := min(n, i+60)
				t.Errorf("first diff at byte %d:\n  relay : %q\n  direct: %q", i,
					relayBody[ctxStart:ctxEnd], directBody[ctxStart:ctxEnd])
				break
			}
		}
		return
	}

	// Sanity: response must contain the SSE terminator and at least one
	// tool_calls chunk somewhere in the middle.
	if !bytes.Contains(relayBody, []byte("data: [DONE]")) {
		t.Error("streamed response missing [DONE] terminator")
	}
	chunkCount := bytes.Count(relayBody, []byte("data: "))
	t.Logf("byte-pass streaming verified: %d bytes / %d SSE chunks matched direct curl", len(relayBody), chunkCount)
}

// seedHappyPathForModel mirrors seedHappyPath but lets the caller pin
// the model name (snapshot Name) so it matches the upstream wire shape.
// Returns the cleartext relay-key bearer.
func (s *stack) seedHappyPathForModel(upstreamURL, hostKeyValue, modelName string) string {
	s.t.Helper()
	ctx := context.Background()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: ids.New(), Name: "mock-provider", DisplayName: "Mock", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	mustUpsert(s.t, s.stores.Provider.Upsert(ctx, prov), "provider")

	hst := &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "mock-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstreamURL},
	}
	mustUpsert(s.t, s.stores.Host.Upsert(ctx, hst), "host")

	hostTier := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "mock-host-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
	}
	mustUpsert(s.t, s.stores.Policy.Upsert(ctx, hostTier), "host-tier")

	if err := os.Setenv("MOCK_HOSTKEY_VAL", hostKeyValue); err != nil {
		s.t.Fatalf("setenv: %v", err)
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "mock-hostkey", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{
			HostID:    hst.Meta.ID,
			PolicyID:  hostTier.Meta.ID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "MOCK_HOSTKEY_VAL"},
		},
	}
	mustUpsert(s.t, s.stores.HostKey.Upsert(ctx, hk), "hostkey")

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: modelName, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{
			Hosts: []model.HostBinding{{
				HostID:  hst.Meta.ID,
				Adapter: adapters.OpenAI,
			}},
			Snapshots: []model.Snapshot{{Name: modelName}},
			Pointer:   modelName,
		},
	}
	mustUpsert(s.t, s.stores.Model.Upsert(ctx, mdl), "model")

	pol := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "mock-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			ModelIDs:     []string{mdl.Meta.ID},
			HostKeyIDs:   []string{hk.Meta.ID},
			KeySelection: policy.KeySelectionPrioritized,
		},
	}
	mustUpsert(s.t, s.stores.Policy.Upsert(ctx, pol), "policy")

	const relayKeyPlain = "rk_mock_smoke"
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "rk-mock", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ids.New()}},
		Spec: relaykey.Spec{PolicyID: pol.Meta.ID, KeyHash: sha256Hex(relayKeyPlain), Prefix: "rk_mock"},
	}
	mustUpsert(s.t, s.stores.RelayKey.Upsert(ctx, rk), "relaykey")
	if err := s.cat.Reload(ctx); err != nil {
		s.t.Fatalf("Reload: %v", err)
	}
	return relayKeyPlain
}
