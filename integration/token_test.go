//go:build integration

// token_test.go walks an inference token across both planes: minted from a
// session on the control plane, spent on the data plane, and taken away
// again by the per-user version bump and by disabling the account it names.
// The signing key's own lifecycle is the composition root's and is not
// exercised from here.
package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tokenFixture is a stack seeded for token traffic: a project the caller can
// mint against, bound to the policy the happy path wired, and a login.
type tokenFixture struct {
	*stack
	project  string
	userID   string
	username string
	password string
	upstream *httptest.Server
}

const tokenUpstreamResponse = `{"id":"chatcmpl-token","object":"chat.completion","model":"test-model",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tokenUpstreamResponse)
	}))
	t.Cleanup(upstream.Close)

	st := newStack(t)
	st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")

	teamID := st.mkTeam(t, "tokens")
	projectID := st.mkProject(t, "token-project", teamID)

	// The token resolves its policy through a project binding, which is the
	// only route a token has: it carries no key to read one off.
	code, raw := st.adminDo(http.MethodGet, "/api/policies/test-policy", "")
	if code != http.StatusOK {
		t.Fatalf("GET policy = %d: %s", code, raw)
	}
	var pol tenancyRow
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	body := `{"metadata":{"name":"token-project-everyone"},"spec":{"projectId":"` + projectID + `",` +
		`"policyId":"` + pol.Metadata.ID + `","subjects":[{"kind":"group","name":"system:authenticated"}]}}`
	if code, raw := st.adminDo(http.MethodPost, "/api/policy-bindings", body); code != http.StatusCreated {
		t.Fatalf("POST /api/policy-bindings = %d: %s", code, raw)
	}

	const username, password = "token-minter", "correct-horse-battery"
	userID := st.seedLogin(t, username, password)

	if err := st.cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog.Reload: %v", err)
	}
	return &tokenFixture{
		stack: st, project: "token-project",
		userID: userID, username: username, password: password,
		upstream: upstream,
	}
}

// mint asks the control plane for a token as the logged-in user.
func (f *tokenFixture) mint(us *userSession) (int, string, []byte) {
	f.t.Helper()
	code, raw := us.do(http.MethodPost, "/api/auth/token", `{"project":"`+f.project+`"}`)
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &out)
	return code, out.Token, raw
}

// chat spends a bearer on the data plane and returns the status plus the
// error code the body carries (empty on success).
func (f *tokenFixture) chat(bearer string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.inference.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusOK && body.Error.Code == "" {
		f.t.Logf("chat %d: %s", resp.StatusCode, raw)
	}
	return resp.StatusCode, body.Error.Code
}

// waitForRejection polls the data plane until the bearer stops being
// accepted, and reports the code it was rejected with.
func (f *tokenFixture) waitForRejection(bearer string, within time.Duration) (int, string) {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, reason := f.chat(bearer)
		if code != http.StatusOK {
			return code, reason
		}
		if time.Now().After(deadline) {
			return code, reason
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMintedTokenAuthenticatesAnInferenceRequest(t *testing.T) {
	f := newTokenFixture(t)
	us := f.login(t, f.username, f.password)

	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if token == "" {
		t.Fatalf("mint returned no token: %s", raw)
	}
	if got, reason := f.chat(token); got != http.StatusOK {
		t.Fatalf("chat with a fresh token = %d (%s)", got, reason)
	}
}

// revoke-all bumps the user's token version. The bump reaches the data plane
// over NOTIFY, so the promise is a bound, not an instant.
func TestRevokeAllInvalidatesTokensWithinTwoSeconds(t *testing.T) {
	f := newTokenFixture(t)
	us := f.login(t, f.username, f.password)

	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if got, reason := f.chat(token); got != http.StatusOK {
		t.Fatalf("chat before revoke = %d (%s)", got, reason)
	}

	if code, raw := us.do(http.MethodPost, "/api/auth/token/revoke-all", ""); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("revoke-all = %d: %s", code, raw)
	}

	got, reason := f.waitForRejection(token, 2*time.Second)
	if got != http.StatusUnauthorized {
		t.Fatalf("chat after revoke-all = %d (%s), want 401 within 2s", got, reason)
	}
	if reason != "token_revoked" {
		t.Errorf("rejection code = %q, want token_revoked", reason)
	}
}

// Disabling an account has to close every door it opened: the tokens it
// already holds, minting new ones, and logging back in.
func TestDisablingAUserRevokesTokensAndRefusesMintAndLogin(t *testing.T) {
	f := newTokenFixture(t)
	us := f.login(t, f.username, f.password)

	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}

	if code, raw := f.adminDo(http.MethodPut, "/api/users/by-id/"+f.userID,
		`{"disabled":true}`); code != http.StatusOK {
		t.Fatalf("disable user = %d: %s", code, raw)
	}

	got, reason := f.waitForRejection(token, 2*time.Second)
	if got != http.StatusUnauthorized {
		t.Fatalf("chat after the account was disabled = %d (%s), want 401", got, reason)
	}
	if reason != "token_revoked" {
		t.Errorf("rejection code = %q, want token_revoked", reason)
	}

	if code, _, raw := f.mint(us); code != http.StatusForbidden {
		t.Errorf("mint on a disabled account = %d: %s, want 403", code, raw)
	}

	fresh := f.freshSession()
	if code, raw := fresh.do(http.MethodPost, "/api/auth/login",
		`{"username":"`+f.username+`","password":"`+f.password+`"}`); code != http.StatusUnauthorized {
		t.Errorf("login on a disabled account = %d: %s, want 401", code, raw)
	}
}

// freshSession is a client with no cookie of its own — a fresh browser
// attempting a login rather than reusing the one already established.
func (f *tokenFixture) freshSession() *userSession {
	return &userSession{t: f.t, base: f.control.URL, client: &http.Client{}}
}
