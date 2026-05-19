package anthropic

import (
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai"
)

func TestAnthropicAdapter(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)

	a := AnthropicAdapter{}
	if a.Name() != "anthropic" {
		t.Fatalf("Name() = %q, want anthropic", a.Name())
	}

	raw, err := a.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	mr, ok := raw.(*MessagesRequest)
	if !ok {
		t.Fatalf("ParseRequest returned %T, want *MessagesRequest", raw)
	}
	if mr.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("Model = %q", mr.Model)
	}

	oai, err := a.ToOpenAI(raw)
	if err != nil {
		t.Fatalf("ToOpenAI: %v", err)
	}
	if oai.Model != mr.Model {
		t.Errorf("ToOpenAI model: want %q got %q", mr.Model, oai.Model)
	}

	back, err := a.FromOpenAI(oai)
	if err != nil {
		t.Fatalf("FromOpenAI: %v", err)
	}
	if back.(*MessagesRequest).Model != mr.Model {
		t.Errorf("FromOpenAI model mismatch")
	}

	text := "hello"
	oaiResp := &openai.ChatResponse{
		ID:    "id1",
		Model: mr.Model,
		Choices: []openai.Choice{
			{Message: openai.ChatResponseMessage{Role: "assistant", Content: &text}, FinishReason: "stop"},
		},
		Usage: &openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}

	anthResp, err := a.FromOpenAIResponse(oaiResp)
	if err != nil {
		t.Fatalf("FromOpenAIResponse: %v", err)
	}
	mr2 := anthResp.(*MessagesResponse)

	oaiResp2, err := a.ToOpenAIResponse(mr2)
	if err != nil {
		t.Fatalf("ToOpenAIResponse: %v", err)
	}
	if oaiResp2.ID != "id1" {
		t.Errorf("ToOpenAIResponse id: want id1 got %s", oaiResp2.ID)
	}
}
