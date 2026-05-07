package anthropic

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/internal/api"
)

func TestAnthropicAdapterStubs(t *testing.T) {
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

	if _, err := a.ToOpenAI(raw); !errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("ToOpenAI: want ErrNotImplemented, got %v", err)
	}
	if _, err := a.FromOpenAI(nil); !errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("FromOpenAI: want ErrNotImplemented, got %v", err)
	}
	if _, err := a.ToOpenAIResponse(nil); !errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("ToOpenAIResponse: want ErrNotImplemented, got %v", err)
	}
	if _, err := a.FromOpenAIResponse(nil); !errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("FromOpenAIResponse: want ErrNotImplemented, got %v", err)
	}
}
