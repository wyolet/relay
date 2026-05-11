//go:build integration

// Package integration provides end-to-end tests that run against the live
// compose stack (pg / clickhouse / valkey / relay-a / relay-b / nginx).
//
// Run with:
//
//	go test -tags=integration ./integration/...
//
// The tests skip automatically when the compose stack is unreachable.
// Start it first with `make up` from the repo root.
//
// Environment variables:
//
//	RELAY_CONTROL_URL  — control-plane base URL (default http://localhost:5103)
//	RELAY_DATA_URL     — data-plane base URL  (default http://localhost:5100)
//	RELAY_ADMIN_TOKEN  — admin bearer token   (default admin-token, matches smoke.env)
package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Env / defaults
// ---------------------------------------------------------------------------

func controlURL() string {
	if u := os.Getenv("RELAY_CONTROL_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:5103"
}

func dataURL() string {
	if u := os.Getenv("RELAY_DATA_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:5100"
}

func adminToken() string {
	if t := os.Getenv("RELAY_ADMIN_TOKEN"); t != "" {
		return t
	}
	return "admin-token"
}

// ---------------------------------------------------------------------------
// Reachability probe — skip if compose is not up
// ---------------------------------------------------------------------------

// requireStack skips the test immediately if the compose stack is unreachable.
func requireStack(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(controlURL() + "/healthz")
	if err != nil {
		t.Skipf("compose stack not running; run `make up` first (%v)", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("compose stack not healthy (status %d); run `make up` first", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Fake upstream
// ---------------------------------------------------------------------------

// startFakeUpstream starts an httptest.Server on a real TCP port (bound to
// 0.0.0.0 so compose containers can reach it via host.docker.internal).
// Returns the server and its host.docker.internal URL for use in provider config.
func startFakeUpstream(t *testing.T) (srv *httptest.Server, dockerURL string) {
	t.Helper()

	// We need to listen on 0.0.0.0 so Docker Desktop's host.docker.internal
	// resolves to this process.  httptest.NewServer binds to 127.0.0.1 by
	// default, which is unreachable from inside containers.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen fake upstream: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
  "id": "chatcmpl-test",
  "object": "chat.completion",
  "model": "test-model",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "ok"},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
}`)
	})

	srv = &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: mux},
	}
	srv.Start()
	t.Cleanup(srv.Close)

	// Use host.docker.internal so relay containers can reach us.
	dockerURL = fmt.Sprintf("http://host.docker.internal:%d", port)
	return srv, dockerURL
}

// ---------------------------------------------------------------------------
// Admin HTTP helpers
// ---------------------------------------------------------------------------

func adminDo(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, controlURL()+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Relay-Admin-Token", adminToken())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// adminPost sends a POST and returns the decoded response body as a map.
func adminPost(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	resp := adminDo(t, http.MethodPost, path, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST %s → %d: %s", path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response from POST %s: %v\nbody: %s", path, err, raw)
	}
	return out
}

// adminDelete sends a DELETE and expects 2xx.
func adminDelete(t *testing.T, path string) {
	t.Helper()
	resp := adminDo(t, http.MethodDelete, path, nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Logf("DELETE %s → %d: %s (cleanup — continuing)", path, resp.StatusCode, raw)
	}
}

// adminPut sends a PUT and returns the decoded response.
func adminPut(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	resp := adminDo(t, http.MethodPut, path, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("PUT %s → %d: %s", path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response from PUT %s: %v\nbody: %s", path, err, raw)
	}
	return out
}

// metaID extracts metadata.id from a decoded admin response.
func metaID(t *testing.T, resp map[string]any) string {
	t.Helper()
	meta, _ := resp["metadata"].(map[string]any)
	id, _ := meta["id"].(string)
	if id == "" {
		t.Fatalf("response missing metadata.id: %v", resp)
	}
	return id
}

// metaName extracts metadata.name from a decoded admin response.
func metaName(t *testing.T, resp map[string]any) string {
	t.Helper()
	meta, _ := resp["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		t.Fatalf("response missing metadata.name: %v", resp)
	}
	return name
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// fixture holds the IDs and names of all provisioned resources for one test.
type fixture struct {
	providerID   string
	secretName   string
	modelID      string
	rateLimitID  string
	policyID     string
	routeID      string
	keyID        string
	relayKey     string // plaintext bearer token
	routeName    string // slug, used as X-Relay-Route header
	policyName   string // slug, used in provider defaultPolicy
	modelName    string // slug, used in the request body's "model" field
}

// setupFixture provisions the full resource tree for one strategy test.
//   - suffix must be unique per test (use t.Name() + UUID slice).
//   - strategy is the RateLimitStrategy string.
//   - amount and window control the single rate-limit rule.
//
// Returns the fixture and a cleanup func that DELETEs all resources by id.
func setupFixture(t *testing.T, fakeDockerURL, suffix, strategy string, amount int64, window time.Duration) (*fixture, func()) {
	t.Helper()

	// Sanitise suffix for slug use (test names have slashes / spaces).
	safeSuffix := slugSafe(suffix)

	displayName := func(kind string) string {
		return kind + "-" + safeSuffix
	}

	// 1. Provider
	provResp := adminPost(t, "/control/providers", map[string]any{
		"metadata": map[string]any{"name": displayName("provider")},
		"spec": map[string]any{
			"kind":    "openai",
			"baseURL": fakeDockerURL,
		},
	})
	provID := metaID(t, provResp)
	provName := metaName(t, provResp)

	// 2. Secret — the bespoke secret handler uses {name, provider, valueFrom}
	// (not the standard metadata-wrapped shape). Response has a top-level "name".
	secName := "sec-" + safeSuffix
	secResp := adminPost(t, "/control/secrets", map[string]any{
		"name":     secName,
		"provider": provName,
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-test-" + safeSuffix,
		},
	})
	// Confirm creation returned the name we expect.
	if got, _ := secResp["name"].(string); got != "" {
		secName = got
	}

	// 3. Model
	modelResp := adminPost(t, "/control/models", map[string]any{
		"metadata": map[string]any{"name": displayName("model")},
		"spec": map[string]any{
			"provider":     provName,
			"upstreamName": "test-model",
		},
	})
	modelID := metaID(t, modelResp)
	modelName := metaName(t, modelResp)

	// 4. RateLimit. Schema requires spec.strategy + spec.window + spec.rules;
	// per-rule window isn't exposed by the OpenAPI schema (Window has json:"-"
	// for the duration shim). spec.strategy + spec.window fan out to rules.
	rlResp := adminPost(t, "/control/ratelimits", map[string]any{
		"metadata": map[string]any{"name": displayName("rl")},
		"spec": map[string]any{
			"strategy": strategy,
			"window":   window.Nanoseconds(),
			"rules": []map[string]any{
				{"meter": "requests", "amount": amount, "strategy": strategy},
			},
		},
	})
	rlID := metaID(t, rlResp)
	rlName := metaName(t, rlResp)

	// 5. Policy — bind provider + secret + rateLimit + model allowlist
	polResp := adminPost(t, "/control/policies", map[string]any{
		"metadata": map[string]any{"name": displayName("policy")},
		"spec": map[string]any{
			"provider":   provName,
			"secrets":    []string{secName},
			"models":     []string{modelName},
			"rateLimits":        []map[string]any{{"Ref": rlName}},
			"skipDefaultLimits": true,
		},
	})
	polID := metaID(t, polResp)
	polName := metaName(t, polResp)

	// Update provider to set defaultPolicy so routing resolves the policy.
	adminPut(t, "/control/providers/by-id/"+provID, map[string]any{
		"metadata": map[string]any{"name": displayName("provider")},
		"spec": map[string]any{
			"kind":          "openai",
			"baseURL":       fakeDockerURL,
			"defaultPolicy": polName,
		},
	})

	// 6. Route — list the resolved model name
	routeResp := adminPost(t, "/control/routes", map[string]any{
		"metadata": map[string]any{"name": displayName("route")},
		"spec": map[string]any{
			"models": []string{modelName},
		},
	})
	routeID := metaID(t, routeResp)
	routeName := metaName(t, routeResp)

	// 7. RelayKey — scoped to policy via policyRef. Schema rejects extra fields
	// at top level (no top-level "value" allowed), and spec.keyHash is required.
	// Compute the hash client-side.
	bearerPlain := "rk-test-" + safeSuffix + "-" + randHex()
	sum := sha256.Sum256([]byte(bearerPlain))
	keyHash := hex.EncodeToString(sum[:])
	prefix := bearerPlain
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	keyResp := adminPost(t, "/control/keys", map[string]any{
		"metadata": map[string]any{"name": displayName("key")},
		"spec": map[string]any{
			"keyHash":   keyHash,
			"prefix":    prefix,
			"policyRef": polName,
		},
	})
	keyID := metaID(t, keyResp)

	fx := &fixture{
		providerID:  provID,
		secretName:  secName,
		modelID:     modelID,
		rateLimitID: rlID,
		policyID:    polID,
		routeID:     routeID,
		keyID:       keyID,
		relayKey:    bearerPlain,
		routeName:   routeName,
		policyName:  polName,
		modelName:   modelName,
	}

	cleanup := func() {
		// Clear provider.defaultPolicy first so the policy/ratelimit/secret/model
		// can be deleted without failing referential-integrity validation.
		adminPut(t, "/control/providers/by-id/"+provID, map[string]any{
			"metadata": map[string]any{"name": displayName("provider")},
			"spec":     map[string]any{"kind": "openai", "baseURL": fakeDockerURL},
		})
		adminDelete(t, "/control/keys/by-id/"+keyID)
		adminDelete(t, "/control/routes/by-id/"+routeID)
		adminDelete(t, "/control/policies/by-id/"+polID)
		adminDelete(t, "/control/ratelimits/by-id/"+rlID)
		adminDelete(t, "/control/models/by-id/"+modelID)
		adminDelete(t, "/control/secrets/"+secName) // secrets are slug-routed
		adminDelete(t, "/control/providers/by-id/"+provID)
	}

	// Give the data-plane snapshot time to reload (PG NOTIFY → reload is ~100ms).
	// We can't probe via real requests because each call consumes rate-limit
	// capacity and would invalidate the test.
	time.Sleep(800 * time.Millisecond)

	return fx, cleanup
}

// ---------------------------------------------------------------------------
// Data-plane request helper
// ---------------------------------------------------------------------------

// dataRequest fires one chat-completion request and returns (statusCode, retryAfter).
// retryAfter is 0 when the header is absent or unparseable.
func dataRequest(fx *fixture) (int, int) {
	body, _ := json.Marshal(map[string]any{
		"model":    fx.modelName,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, dataURL()+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fx.relayKey)
	req.Header.Set("X-Relay-Route", fx.routeName)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)

	ra := 0
	if h := resp.Header.Get("Retry-After"); h != "" {
		fmt.Sscanf(h, "%d", &ra)
	}
	if os.Getenv("RELAY_DEBUG") != "" {
		fmt.Printf("[debug] status=%d retry=%d xstatus=%s body=%s\n",
			resp.StatusCode, ra, resp.Header.Get("X-Relay-Status"), string(rb[:min(200, len(rb))]))
	}
	return resp.StatusCode, ra
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// slugSafe replaces characters not valid in DNS slugs with hyphens.
func slugSafe(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	// Trim leading/trailing hyphens and collapse runs.
	result := b.String()
	// Limit length to avoid slug overflow.
	if len(result) > 40 {
		result = result[len(result)-40:]
	}
	return strings.Trim(result, "-")
}

// randHex returns 8 random-ish hex chars derived from current nanosecond time.
func randHex() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
}

// uniqueSuffix returns a short suffix that combines the test name and a
// timestamp, suitable for disambiguating concurrent test runs.
func uniqueSuffix(t *testing.T) string {
	t.Helper()
	return slugSafe(t.Name()) + fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}
