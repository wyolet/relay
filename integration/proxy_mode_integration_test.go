//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Passthrough config helpers
// ---------------------------------------------------------------------------

// setPassthrough replaces the Passthrough singleton via PUT /control/passthrough.
// Returns a cleanup func that restores the previous value.
func setPassthrough(t *testing.T, enabled, unauthEnabled bool) func() {
	t.Helper()

	// Fetch current config so we can restore it.
	resp := adminDo(t, http.MethodGet, "/control/passthrough", nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var prev map[string]any
	_ = json.Unmarshal(raw, &prev)

	newConfig := map[string]any{
		"metadata": map[string]any{"name": "default"},
		"spec": map[string]any{
			"enabled": enabled,
			"unauthenticated": map[string]any{
				"enabled":  unauthEnabled,
				"bucketBy": "credential_hash",
			},
		},
	}
	adminPut(t, "/control/passthrough", newConfig)
	// Give data-plane snapshot time to reload.
	time.Sleep(800 * time.Millisecond)

	return func() {
		if prev != nil {
			adminPut(t, "/control/passthrough", prev)
		}
	}
}

// ---------------------------------------------------------------------------
// Data-plane request builders for proxy-mode tests
// ---------------------------------------------------------------------------

type proxyReqOpts struct {
	// relayKey is the Relay bearer token — sent in X-WR-API-Key.
	// If empty the header is omitted (anonymous proxy or no-key tests).
	relayKey string
	// authzHeader is the full value of the Authorization header.
	// E.g. "Bearer sk-provider-xyz" or "" to omit.
	authzHeader string
	// proxyModeHeader is the value of X-WR-Proxy-Mode.
	// "" = omit the header entirely.
	proxyModeHeader string
	// routeName is the X-Relay-Route header value. May be empty.
	routeName string
	// modelName is the body model field.
	modelName string
}

func sendProxyRequest(t *testing.T, opts proxyReqOpts) *http.Response {
	t.Helper()
	if opts.modelName == "" {
		opts.modelName = "test-model"
	}
	body, _ := json.Marshal(map[string]any{
		"model":    opts.modelName,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, dataURL()+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if opts.relayKey != "" {
		req.Header.Set("X-WR-API-Key", opts.relayKey)
	}
	if opts.authzHeader != "" {
		req.Header.Set("Authorization", opts.authzHeader)
	}
	if opts.proxyModeHeader != "" {
		req.Header.Set("X-WR-Proxy-Mode", opts.proxyModeHeader)
	}
	if opts.routeName != "" {
		req.Header.Set("X-Relay-Route", opts.routeName)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("data-plane request: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// TestProxyMode_Normal_AuthedHappy
//
// No X-WR-Proxy-Mode (default Normal mode), valid Relay key in Authorization.
// Expects 200.
// ---------------------------------------------------------------------------

func TestProxyMode_Normal_AuthedHappy(t *testing.T) {
	requireStack(t)
	_, dockerURL := startFakeUpstream(t)

	fx, cleanup := setupFixture(t, dockerURL, uniqueSuffix(t), "token-bucket", 100, 60*time.Second)
	defer cleanup()

	resp := sendProxyRequest(t, proxyReqOpts{
		// Normal mode: relay key in Authorization Bearer
		authzHeader: "Bearer " + fx.relayKey,
		routeName:   fx.routeName,
		modelName:   fx.modelName,
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_ProxyAuthed_Happy
//
// X-WR-Proxy-Mode: Proxy + Relay key in X-WR-API-Key + provider key in Authorization.
// Expects 200.
// ---------------------------------------------------------------------------

func TestProxyMode_ProxyAuthed_Happy(t *testing.T) {
	requireStack(t)
	_, dockerURL := startFakeUpstream(t)

	fx, cleanup := setupFixture(t, dockerURL, uniqueSuffix(t), "token-bucket", 100, 60*time.Second)
	defer cleanup()

	// ProxyAuthed requires (a) the relay key marked passthroughAllowed=true,
	// and (b) the global Passthrough singleton enabled. setupFixture creates a
	// vanilla key; flip both here.
	restorePassthrough := setPassthrough(t, true, false)
	defer restorePassthrough()

	// GET the current key to obtain its keyHash/prefix/name, then PUT with
	// passthroughAllowed=true.
	keyResp := adminDo(t, http.MethodGet, "/control/keys/by-id/"+fx.keyID, nil)
	var keyDoc struct {
		Metadata struct{ Name string } `json:"metadata"`
		Spec     struct {
			KeyHash   string `json:"keyHash"`
			Prefix    string `json:"prefix"`
			PolicyRef string `json:"policyRef"`
		} `json:"spec"`
	}
	defer keyResp.Body.Close()
	if err := json.NewDecoder(keyResp.Body).Decode(&keyDoc); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	adminPut(t, "/control/keys/by-id/"+fx.keyID, map[string]any{
		"metadata": map[string]any{"name": keyDoc.Metadata.Name},
		"spec": map[string]any{
			"keyHash":            keyDoc.Spec.KeyHash,
			"prefix":             keyDoc.Spec.Prefix,
			"policyRef":          keyDoc.Spec.PolicyRef,
			"passthroughAllowed": true,
		},
	})

	resp := sendProxyRequest(t, proxyReqOpts{
		proxyModeHeader: "Proxy",
		relayKey:        fx.relayKey,
		// Authorization carries the provider key (fake — fake upstream accepts anything)
		authzHeader: "Bearer sk-provider-key-for-proxy-test",
		routeName:   fx.routeName,
		modelName:   fx.modelName,
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_ProxyAnonymous_WhenEnabled_200
//
// X-WR-Proxy-Mode: Proxy + provider key only (no Relay key).
// passthrough.unauthenticated.enabled = true → 200.
// ---------------------------------------------------------------------------

func TestProxyMode_ProxyAnonymous_WhenEnabled_200(t *testing.T) {
	requireStack(t)

	restorePassthrough := setPassthrough(t, true, true)
	defer restorePassthrough()

	_, dockerURL := startFakeUpstream(t)
	fx, cleanup := setupFixture(t, dockerURL, uniqueSuffix(t), "token-bucket", 100, 60*time.Second)
	defer cleanup()

	resp := sendProxyRequest(t, proxyReqOpts{
		proxyModeHeader: "Proxy",
		// No X-WR-API-Key — anonymous proxy
		authzHeader: "Bearer sk-anon-provider-key",
		routeName:   fx.routeName,
		modelName:   fx.modelName,
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_ProxyAnonymous_WhenDisabled_401
//
// X-WR-Proxy-Mode: Proxy + provider key only (no Relay key).
// passthrough.unauthenticated.enabled = false → 401.
// ---------------------------------------------------------------------------

func TestProxyMode_ProxyAnonymous_WhenDisabled_401(t *testing.T) {
	requireStack(t)

	restorePassthrough := setPassthrough(t, false, false)
	defer restorePassthrough()

	resp := sendProxyRequest(t, proxyReqOpts{
		proxyModeHeader: "Proxy",
		authzHeader:     "Bearer sk-anon-provider-key",
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_ProxyWithoutProviderKey_400
//
// X-WR-Proxy-Mode: Proxy + Relay key, but no Authorization header.
// Expects 400 (ErrMissingProviderKey).
// ---------------------------------------------------------------------------

func TestProxyMode_ProxyWithoutProviderKey_400(t *testing.T) {
	requireStack(t)

	_, dockerURL := startFakeUpstream(t)
	fx, cleanup := setupFixture(t, dockerURL, uniqueSuffix(t), "token-bucket", 100, 60*time.Second)
	defer cleanup()

	resp := sendProxyRequest(t, proxyReqOpts{
		proxyModeHeader: "Proxy",
		relayKey:        fx.relayKey,
		// No Authorization header — should trigger ErrMissingProviderKey
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_NormalWithoutRelayKey_401
//
// Normal mode (no X-WR-Proxy-Mode), no Relay key anywhere.
// Expects 401.
// ---------------------------------------------------------------------------

func TestProxyMode_NormalWithoutRelayKey_401(t *testing.T) {
	requireStack(t)

	resp := sendProxyRequest(t, proxyReqOpts{
		// No relay key, no proxy mode header, no Authorization
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TestProxyMode_BadHeaderValue_400
//
// X-WR-Proxy-Mode: <garbage> → 400 (ErrInvalidProxyModeHeader).
// ---------------------------------------------------------------------------

func TestProxyMode_BadHeaderValue_400(t *testing.T) {
	requireStack(t)

	resp := sendProxyRequest(t, proxyReqOpts{
		proxyModeHeader: "NotAValidMode",
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}
