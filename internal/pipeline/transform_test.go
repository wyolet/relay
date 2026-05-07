package pipeline

// These tests use minimal inline adapters that satisfy TransformAdapter to
// avoid import cycles (internal/api/anthropic and internal/api/openai both
// import internal/pipeline, so tests in this package cannot import them).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/transport"
)

// --- minimal stub adapters ---

// stubRequest / stubResponse are the "native" types for stub adapters.
type stubRequest struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
}

type stubResponse struct {
	Text string `json:"text"`
}

// hubRequest / hubResponse are our stand-in for openai.FullChatRequest /
// openai.ChatResponse. In real usage the Shim wrappers handle this conversion;
// here we use plain maps so the test stays self-contained.
type hubReq map[string]any
type hubResp map[string]any

// alphaAdapter is a test shape named "alpha".
type alphaAdapter struct{}

func (alphaAdapter) Name() string { return "alpha" }
func (alphaAdapter) ParseRequest(body []byte) (any, error) {
	var r stubRequest
	return &r, json.Unmarshal(body, &r)
}
func (alphaAdapter) ToOpenAI(req any) (any, error) {
	r := req.(*stubRequest)
	return hubReq{"model": r.Model, "messages": r.Messages, "_from": "alpha"}, nil
}
func (alphaAdapter) FromOpenAI(hub any) (any, error) {
	h := hub.(hubReq)
	return &stubRequest{Model: h["model"].(string)}, nil
}
func (alphaAdapter) ToOpenAIResponse(resp any) (any, error) {
	r := resp.(*stubResponse)
	return hubResp{"text": r.Text, "_from": "alpha"}, nil
}
func (alphaAdapter) FromOpenAIResponse(hub any) (any, error) {
	h := hub.(hubResp)
	txt, _ := h["text"].(string)
	return &stubResponse{Text: txt}, nil
}
func (alphaAdapter) ParseResponse(body []byte) (any, error) {
	var r stubResponse
	return &r, json.Unmarshal(body, &r)
}

// betaAdapter is a test shape named "beta".
type betaAdapter struct{}

func (betaAdapter) Name() string { return "beta" }
func (betaAdapter) ParseRequest(body []byte) (any, error) {
	var r stubRequest
	return &r, json.Unmarshal(body, &r)
}
func (betaAdapter) ToOpenAI(req any) (any, error) {
	r := req.(*stubRequest)
	return hubReq{"model": r.Model, "messages": r.Messages, "_from": "beta"}, nil
}
func (betaAdapter) FromOpenAI(hub any) (any, error) {
	h := hub.(hubReq)
	return &stubRequest{Model: h["model"].(string)}, nil
}
func (betaAdapter) ToOpenAIResponse(resp any) (any, error) {
	r := resp.(*stubResponse)
	return hubResp{"text": r.Text, "_from": "beta"}, nil
}
func (betaAdapter) FromOpenAIResponse(hub any) (any, error) {
	h := hub.(hubResp)
	txt, _ := h["text"].(string)
	return &stubResponse{Text: txt}, nil
}
func (betaAdapter) ParseResponse(body []byte) (any, error) {
	var r stubResponse
	return &r, json.Unmarshal(body, &r)
}

var (
	alpha TransformAdapter = alphaAdapter{}
	beta  TransformAdapter = betaAdapter{}
)

var reqBody = []byte(`{"model":"m1","messages":[]}`)
var streamBody = []byte(`{"model":"m1","messages":[],"stream":true}`)
var respBody = []byte(`{"text":"hello"}`)

func TestApplyTransform_SameShape_Alpha(t *testing.T) {
	tr, err := ApplyTransform(alpha, alpha, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Same-shape: zero-copy — same underlying array.
	if len(tr.Body) != len(reqBody) || &tr.Body[0] != &reqBody[0] {
		t.Error("expected zero-copy passthrough for same-shape")
	}
	if tr.Finisher != nil {
		t.Error("Finisher must be nil for same-shape")
	}
}

func TestApplyTransform_SameShape_Beta(t *testing.T) {
	tr, err := ApplyTransform(beta, beta, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if &tr.Body[0] != &reqBody[0] {
		t.Error("expected zero-copy passthrough for same-shape beta→beta")
	}
	if tr.Finisher != nil {
		t.Error("Finisher must be nil")
	}
}

func TestApplyTransform_CrossShape_AlphaIn_BetaUp_NonStream(t *testing.T) {
	tr, err := ApplyTransform(alpha, beta, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.Finisher == nil {
		t.Fatal("Finisher must be non-nil for cross-shape")
	}

	// Upstream body should be marshalled beta-native request.
	var upReq stubRequest
	if err := json.Unmarshal(tr.Body, &upReq); err != nil {
		t.Fatalf("upstream body not valid: %v", err)
	}
	if upReq.Model != "m1" {
		t.Errorf("model: want m1, got %q", upReq.Model)
	}

	// Apply finisher to a beta-native response.
	msg := &transport.Message{
		Headers: map[string]string{"X-Relay-Status": "200"},
		Body:    respBody,
	}
	out, err := tr.Finisher(msg)
	if err != nil {
		t.Fatalf("finisher error: %v", err)
	}
	var resp stubResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		t.Fatalf("finisher output invalid: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("text: want hello, got %q", resp.Text)
	}
}

func TestApplyTransform_CrossShape_BetaIn_AlphaUp_NonStream(t *testing.T) {
	tr, err := ApplyTransform(beta, alpha, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.Finisher == nil {
		t.Fatal("Finisher must be non-nil for cross-shape")
	}
	msg := &transport.Message{Body: respBody}
	out, err := tr.Finisher(msg)
	if err != nil {
		t.Fatalf("finisher error: %v", err)
	}
	var resp stubResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		t.Fatalf("finisher output invalid: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("text: want hello, got %q", resp.Text)
	}
}

func TestApplyTransform_CrossShape_Streaming_Error(t *testing.T) {
	_, err := ApplyTransform(alpha, beta, streamBody)
	if err == nil {
		t.Fatal("expected error for cross-shape streaming")
	}
	if !strings.Contains(err.Error(), "streaming") {
		t.Errorf("error should mention streaming, got: %v", err)
	}
}

func TestApplyTransform_NilAdapters_Passthrough(t *testing.T) {
	tr, err := ApplyTransform(nil, nil, reqBody)
	if err != nil {
		t.Fatalf("unexpected error with nil adapters: %v", err)
	}
	if tr.Finisher != nil {
		t.Error("Finisher should be nil when adapters are nil")
	}
}

func TestApplyTransform_Finisher_EmptyBody_PassesThrough(t *testing.T) {
	tr, err := ApplyTransform(alpha, beta, reqBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg := &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	out, err := tr.Finisher(msg)
	if err != nil {
		t.Fatalf("finisher on empty body: %v", err)
	}
	if out != msg {
		t.Error("expected same message pointer for empty body")
	}
}
