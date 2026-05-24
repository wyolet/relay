package v1

import (
	"strings"
	"testing"
)

func TestIdentityTranslator_ParseRequestRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","cache_config":{"instructions":true,"tools":true},` +
		`"input":[{"type":"message","role":"user","content":"hi","cache_config":{"anchor":true}}]}`)

	req, err := IdentityTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.CacheConfig == nil || !req.CacheConfig.Instructions || !req.CacheConfig.Tools {
		t.Errorf("request cache_config lost through identity parse: %+v", req.CacheConfig)
	}
	if m, ok := req.Input[0].(*Message); !ok || m.CacheConfig == nil || !m.CacheConfig.Anchor {
		t.Errorf("item cache_config lost: %+v", req.Input[0])
	}
}

func TestIdentityTranslator_SerializeResponse(t *testing.T) {
	resp := &Response{ID: "resp_1", Object: "response", Status: StatusCompleted, FinishReason: FinishReasonStop}
	out, err := IdentityTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"id":"resp_1"`) || !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Errorf("canonical response not marshalled as-is: %s", out)
	}
}

func TestIdentityTranslator_StreamFactoriesAreIdentity(t *testing.T) {
	if (IdentityTranslator{}).NewToCanonicalStream() != nil {
		t.Error("NewToCanonicalStream should be nil (identity)")
	}
	if (IdentityTranslator{}).NewFromCanonicalStream() != nil {
		t.Error("NewFromCanonicalStream should be nil (identity)")
	}
}

func TestIdentityTranslator_UpstreamSideMethodsError(t *testing.T) {
	// Canonical is inbound-only; the upstream-side methods must not pretend.
	if _, err := (IdentityTranslator{}).SerializeRequest(&Request{}); err == nil {
		t.Error("SerializeRequest should error (canonical is inbound-only)")
	}
	if _, err := (IdentityTranslator{}).ParseResponse([]byte(`{}`)); err == nil {
		t.Error("ParseResponse should error (canonical is inbound-only)")
	}
}
