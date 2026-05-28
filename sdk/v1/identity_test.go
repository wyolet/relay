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

func TestIdentityTranslator_RequestRoundTrip(t *testing.T) {
	// Client-side use: SerializeRequest then ParseRequest must round-trip.
	req := &Request{
		Model:       ModelRefs{"m"},
		CacheConfig: &CacheConfig{Tools: true},
		Input:       []Item{&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "hi"}}}},
	}
	wire, err := IdentityTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := IdentityTranslator{}.ParseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheConfig == nil || !got.CacheConfig.Tools {
		t.Errorf("cache_config lost in round-trip: %+v", got.CacheConfig)
	}
	if len(got.Input) != 1 {
		t.Errorf("input lost: %+v", got.Input)
	}
}

func TestIdentityTranslator_ParseResponseRoundTrip(t *testing.T) {
	resp := &Response{ID: "r1", Object: "response", Status: StatusCompleted,
		Output: []Item{&Message{Role: RoleAssistant, Content: []Part{&OutputTextPart{Text: "hi"}}}}}
	wire, err := IdentityTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := IdentityTranslator{}.ParseResponse(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "r1" || len(got.Output) != 1 {
		t.Errorf("response round-trip lost data: %+v", got)
	}
}
