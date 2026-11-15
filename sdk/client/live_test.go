package client

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// These tests call real endpoints and skip unless their environment variables are set.

func liveReq(model string) *v1.Request {
	mt := 64
	return &v1.Request{
		Model: v1.ModelRefs{model},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "Reply with the single word: pong"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			model: {Sampling: &v1.SamplingParams{MaxTokens: &mt}},
		},
	}
}

func outputText(resp *Response) string {
	var b strings.Builder
	for _, it := range resp.Output {
		if m, ok := it.(*v1.Message); ok {
			for _, p := range m.Content {
				if tp, ok := p.(*v1.OutputTextPart); ok {
					b.WriteString(tp.Text)
				}
			}
		}
	}
	return b.String()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Set OPENAI_API_KEY (and optionally OPENAI_MODEL, default gpt-4o-mini) to run.
func TestLive_OpenAI(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("set OPENAI_API_KEY to run the live OpenAI test")
	}
	model := envOr("OPENAI_MODEL", "gpt-4o-mini")
	c := OpenAI("https://api.openai.com", key, WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	resp, err := c.Generate(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	if outputText(resp) == "" {
		t.Errorf("empty output: %+v", resp)
	}
}

// Set OLLAMA_BASE_URL (e.g. http://localhost:11434) and optionally OLLAMA_MODEL to run.
func TestLive_Ollama(t *testing.T) {
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		t.Skip("set OLLAMA_BASE_URL (and OLLAMA_MODEL) to run the live Ollama test")
	}
	model := envOr("OLLAMA_MODEL", "llama3.2")
	c := OpenAI(base, "ollama", WithHTTPClient(&http.Client{Timeout: 60 * time.Second}))
	resp, err := c.Generate(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	if outputText(resp) == "" {
		t.Errorf("empty output: %+v", resp)
	}

	stream, err := c.GenerateStream(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	events := eventTypes(recvAll(t, stream))
	if len(events) == 0 || events[len(events)-1] != v1.EventGenerationCompleted {
		t.Errorf("stream did not end with generation.completed: %v", events)
	}
}
