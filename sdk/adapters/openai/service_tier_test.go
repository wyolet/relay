package openai

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestServiceTier_ReachesSummary(t *testing.T) {
	cases := []struct {
		name string
		tr   v1.Translator
		body string
		want string
	}{
		{"cc buffered", CCTranslator{}, `{"id":"c1","object":"chat.completion","model":"gpt-5","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, "priority"},
		{"cc stream", CCTranslator{}, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n" +
			"data: [DONE]\n\n", "flex"},
		{"responses buffered", ResponsesTranslator{}, `{"id":"r1","object":"response","model":"gpt-5","status":"completed","service_tier":"priority","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`, "priority"},
		{"responses stream", ResponsesTranslator{}, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"object\":\"response\",\"model\":\"gpt-5\",\"status\":\"completed\",\"service_tier\":\"flex\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n", "flex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := v1.ExtractSummary(tc.tr, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if s.ServiceTier != tc.want {
				t.Fatalf("service tier = %q, want %q", s.ServiceTier, tc.want)
			}
			if len(s.Tokens) == 0 {
				t.Fatal("tokens lost alongside the service tier")
			}
		})
	}
}

func TestServiceTier_RequestRoundTrip(t *testing.T) {
	for name, tr := range map[string]v1.Translator{"cc": CCTranslator{}, "responses": ResponsesTranslator{}} {
		var body string
		if name == "cc" {
			body = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"service_tier":"flex"}`
		} else {
			body = `{"model":"gpt-5","input":"hi","service_tier":"flex"}`
		}
		req, err := tr.ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := string(req.Extensions[extServiceTier]); got != `"flex"` {
			t.Errorf("%s: extensions[%q] = %s, want \"flex\"", name, extServiceTier, got)
		}
		wire, err := tr.SerializeRequest(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(wire), `"service_tier":"flex"`) {
			t.Errorf("%s: service_tier missing from %s", name, wire)
		}
	}
}

func TestServiceTier_SerializedBack(t *testing.T) {
	resp := &v1.Response{ID: "r1", Model: "gpt-5", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop, ServiceTier: "priority"}
	for name, tr := range map[string]v1.Translator{"cc": CCTranslator{}, "responses": ResponsesTranslator{}} {
		body, err := tr.SerializeResponse(resp, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(body), `"service_tier":"priority"`) {
			t.Errorf("%s: service_tier missing from %s", name, body)
		}
	}

	completed := v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"r1","status":"completed","finish_reason":"stop","service_tier":"flex"}`)}.Bytes()
	for name, tr := range map[string]v1.Translator{"cc": CCTranslator{}, "responses": ResponsesTranslator{}} {
		out, err := tr.NewFromCanonicalStream()(completed)
		if err != nil {
			t.Fatalf("%s stream: %v", name, err)
		}
		if !strings.Contains(string(out), `"service_tier":"flex"`) {
			t.Errorf("%s stream: service_tier missing from %s", name, out)
		}
	}
}
