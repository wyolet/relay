package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// CC knobs with no canonical field ride Request.Extensions and come back out
// on a CC upstream.
func TestCCRequest_ExtensionKnobsRoundTrip(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":        "gpt-4o",
		"messages":     []any{map[string]any{"role": "user", "content": "hi"}},
		"logit_bias":   map[string]int{"1234": -100},
		"logprobs":     true,
		"top_logprobs": 3,
		"service_tier": "flex",
		"store":        true,
		"verbosity":    "low",
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "out", "description": "the answer", "schema": map[string]any{"type": "object"},
		}},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{extLogitBias, extLogprobs, extTopLogprobs, extServiceTier, extStore} {
		if _, ok := req.Extensions[k]; !ok {
			t.Errorf("extension %q not carried", k)
		}
	}
	out := req.ModelConfig["gpt-4o"].Output
	if out.Verbosity != "low" || out.Format.Description != "the answer" {
		t.Errorf("output config = %+v / %+v", out, out.Format)
	}

	wire, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"logit_bias":{"1234":-100}`, `"logprobs":true`, `"top_logprobs":3`, `"service_tier":"flex"`, `"store":true`, `"verbosity":"low"`, `"description":"the answer"`} {
		if !strings.Contains(string(wire), want) {
			t.Errorf("serialized request missing %s: %s", want, wire)
		}
	}
}

func TestCCRequest_MalformedOwnedExtensionErrors(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-4o"},
		Input:      []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		Extensions: map[string]json.RawMessage{extLogprobs: json.RawMessage(`"yes"`)},
	}
	if _, err := (CCTranslator{}).SerializeRequest(req); err == nil {
		t.Error("want an error for a non-bool openai.logprobs")
	}
}

func TestCCRequest_ToolResultImageKept(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function", "function": map[string]any{"name": "shot", "arguments": "{}"},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": []any{
				map[string]any{"type": "text", "text": "screenshot:"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			}},
		},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var out *v1.FunctionCallOutput
	for _, it := range req.Input {
		if o, ok := it.(*v1.FunctionCallOutput); ok {
			out = o
		}
	}
	if out == nil || len(out.Content) != 2 {
		t.Fatalf("tool result = %+v, want two typed parts", out)
	}
	if img, ok := out.Content[1].(*v1.ImagePart); !ok || img.ImageURL != "https://example.com/a.png" {
		t.Errorf("image part = %#v", out.Content[1])
	}
}

func TestCCResponse_LogprobsRoundTrip(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "hi"},
			"finish_reason": "stop",
			"logprobs": map[string]any{"content": []any{map[string]any{
				"token": "hi", "logprob": -0.25,
			}}},
		}},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Extensions[extLogprobs]; !ok {
		t.Fatalf("logprobs not carried: %v", resp.Extensions)
	}
	out, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"logprob":-0.25`) {
		t.Errorf("logprobs not re-emitted: %s", out)
	}
}
