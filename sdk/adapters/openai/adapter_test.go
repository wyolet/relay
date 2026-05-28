package openai

import (
	"testing"
)

func TestOpenAIAdapterIdentity(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	a := OpenAIAdapter{}
	if a.Name() != "openai" {
		t.Fatalf("Name() = %q, want openai", a.Name())
	}

	raw, err := a.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	cr, ok := raw.(*ChatRequest)
	if !ok {
		t.Fatalf("ParseRequest returned %T, want *ChatRequest", raw)
	}
	if cr.Model != "gpt-4o" {
		t.Fatalf("Model = %q, want gpt-4o", cr.Model)
	}

	hub, err := a.ToOpenAI(raw)
	if err != nil {
		t.Fatalf("ToOpenAI: %v", err)
	}
	if hub.Model != "gpt-4o" {
		t.Fatalf("hub.Model = %q, want gpt-4o", hub.Model)
	}

	back, err := a.FromOpenAI(hub)
	if err != nil {
		t.Fatalf("FromOpenAI: %v", err)
	}
	if back != hub {
		t.Fatal("FromOpenAI should return the same *FullChatRequest")
	}

	resp := &ChatResponse{ID: "r1", Model: "gpt-4o"}
	hubResp, err := a.ToOpenAIResponse(resp)
	if err != nil {
		t.Fatalf("ToOpenAIResponse: %v", err)
	}
	if hubResp != resp {
		t.Fatal("ToOpenAIResponse should return the same *ChatResponse")
	}

	backResp, err := a.FromOpenAIResponse(resp)
	if err != nil {
		t.Fatalf("FromOpenAIResponse: %v", err)
	}
	if backResp != resp {
		t.Fatal("FromOpenAIResponse should return the same *ChatResponse")
	}
}
