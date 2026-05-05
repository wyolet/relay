package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/transport"
)

// --- parseMetadata tests ---

func TestParseMetadata_Valid(t *testing.T) {
	m, err := parseMetadata("k1=v1,k2=v2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["k1"] != "v1" || m["k2"] != "v2" {
		t.Errorf("unexpected map: %v", m)
	}
}

func TestParseMetadata_Empty(t *testing.T) {
	m, err := parseMetadata("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestParseMetadata_TooManyKeys(t *testing.T) {
	pairs := make([]string, 17)
	for i := range pairs {
		pairs[i] = "k=v"
	}
	_, err := parseMetadata(strings.Join(pairs, ","))
	if err == nil {
		t.Fatal("expected error for too many keys")
	}
}

func TestParseMetadata_KeyTooLong(t *testing.T) {
	key := strings.Repeat("a", 129)
	_, err := parseMetadata(key + "=v")
	if err == nil {
		t.Fatal("expected error for key too long")
	}
}

func TestParseMetadata_ValueTooLong(t *testing.T) {
	val := strings.Repeat("v", 513)
	_, err := parseMetadata("k=" + val)
	if err == nil {
		t.Fatal("expected error for value too long")
	}
}

func TestParseMetadata_Malformed(t *testing.T) {
	_, err := parseMetadata("no_equals")
	if err == nil {
		t.Fatal("expected error for malformed entry")
	}
}

// --- ChatCompletions handler tests ---

func fakeResolve(name string) (string, bool) {
	known := map[string]string{"gpt-4": "gpt-4", "mymodel": "upstream-model"}
	up, ok := known[name]
	return up, ok
}

func TestChatCompletions_HappyPath(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{
				"X-Relay-Status": "200",
				"Content-Type":   "application/json",
			},
		}
		ch.Out <- &transport.Message{Body: []byte(`{"hello":"world"}`)}
		ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolve, runPipeline)(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestChatCompletions_StreamingPath(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "text/event-stream"},
		}
		ch.Out <- &transport.Message{Body: []byte("data: chunk1\n")}
		ch.Out <- &transport.Message{Body: []byte("data: chunk2\n")}
		ch.Out <- &transport.Message{Body: []byte("data: chunk3\n")}
		ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolve, runPipeline)(rec, req)

	want := "data: chunk1\ndata: chunk2\ndata: chunk3\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestChatCompletions_ModelNotFound(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel) error { return nil }

	body := `{"model":"unknown-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolve, runPipeline)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var env errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", env.Error.Code)
	}
}

func TestChatCompletions_BadJSON(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel) error { return nil }

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolve, runPipeline)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
