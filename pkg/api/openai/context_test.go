package openai

import (
	"context"
	"testing"
)

func TestContextRoundTrip(t *testing.T) {
	cr := &ChatRequest{Model: "gpt-4", Stream: true}
	ctx := ContextWithChatRequest(context.Background(), cr)
	got, ok := ChatRequestFromContext(ctx)
	if !ok {
		t.Fatal("ChatRequestFromContext: want ok=true")
	}
	if got != cr {
		t.Fatal("ChatRequestFromContext: returned different pointer")
	}
}

func TestContextMissing(t *testing.T) {
	_, ok := ChatRequestFromContext(context.Background())
	if ok {
		t.Fatal("ChatRequestFromContext on empty context: want ok=false")
	}
}
