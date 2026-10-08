//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/wyolet/relay/pkg/httpheader"
)

// A token never forwards a caller's upstream key: proxy mode needs the
// passthrough grant only a key row carries. That keeps the proxy path out of
// reach of a token entirely, revoked or not, so its jti denylist need not
// ride the proxy reservation.
func TestIntegration_TokensCannotUseProxyMode(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	f.enableProxyMode(false)
	hostSlug, _ := f.seedProxyHost(f.upstream.URL)
	us := f.login(t, f.username, f.password)
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	req, _ := http.NewRequest(http.MethodPost, f.inference.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
	req.Header.Set(httpheader.HeaderRelayAPIKey, token)
	req.Header.Set(httpheader.HeaderUpstreamHost, hostSlug)
	req.Header.Set("Authorization", "Bearer sk-caller-upstream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusForbidden || out.Error.Code != "passthrough_forbidden" {
		t.Fatalf("token on proxy mode = %d %s, want 403 passthrough_forbidden", resp.StatusCode, body)
	}
}
