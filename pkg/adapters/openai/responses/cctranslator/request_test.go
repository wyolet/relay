package cctranslator

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }
func f64Ptr(f float64) *float64 { return &f }

// ---- helpers ----

func mustParseReq(t *testing.T, body string) *responses.Request {
	t.Helper()
	req, err := responses.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return req
}

func mustMarshalContent(t *testing.T, cc *openai.ChatMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(cc.Content, &s); err != nil {
		// Try array form.
		return string(cc.Content)
	}
	return s
}

// ---- tests ----

func TestRequestToCC_SimpleTextInput(t *testing.T) {
	req := mustParseReq(t, `{"model":"gpt-4o","input":"hello world"}`)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.Model != "gpt-4o" {
		t.Errorf("model: got %q, want %q", cc.Model, "gpt-4o")
	}
	if len(cc.Messages) != 1 {
		t.Fatalf("messages len: got %d, want 1", len(cc.Messages))
	}
	if cc.Messages[0].Role != "user" {
		t.Errorf("role: got %q, want %q", cc.Messages[0].Role, "user")
	}
	content := mustMarshalContent(t, &cc.Messages[0])
	if content != "hello world" {
		t.Errorf("content: got %q, want %q", content, "hello world")
	}
}

func TestRequestToCC_InstructionsPrependedAsSystem(t *testing.T) {
	req := mustParseReq(t, `{"model":"gpt-4o","input":"hi","instructions":"You are a helpful bot."}`)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc.Messages) != 2 {
		t.Fatalf("messages len: got %d, want 2", len(cc.Messages))
	}
	if cc.Messages[0].Role != "system" {
		t.Errorf("first message role: got %q, want system", cc.Messages[0].Role)
	}
	var sys string
	_ = json.Unmarshal(cc.Messages[0].Content, &sys)
	if sys != "You are a helpful bot." {
		t.Errorf("system content: got %q", sys)
	}
}

func TestRequestToCC_MultimodalInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"describe this"},
		{"type":"input_image","image_url":"https://example.com/img.png","detail":"high"}
	]}]}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc.Messages) != 1 {
		t.Fatalf("messages len: got %d, want 1", len(cc.Messages))
	}
	// Content should be an array (mixed).
	var parts []openai.ContentPart
	if err := json.Unmarshal(cc.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content not an array: %v — content: %s", err, cc.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts len: got %d, want 2", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Errorf("part[0]: got %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("part[1]: got %+v", parts[1])
	}
	if parts[1].ImageURL.Detail != "high" {
		t.Errorf("image detail: got %q, want high", parts[1].ImageURL.Detail)
	}
}

func TestRequestToCC_FunctionTools(t *testing.T) {
	body := `{"model":"gpt-4o","input":"call a tool","tools":[{
		"type":"function","name":"get_weather","description":"Get weather",
		"parameters":{"type":"object","properties":{"location":{"type":"string"}}}
	}],"tool_choice":"auto"}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc.Tools) != 1 {
		t.Fatalf("tools len: got %d, want 1", len(cc.Tools))
	}
	tool := cc.Tools[0]
	if tool.Type != "function" {
		t.Errorf("tool type: got %q", tool.Type)
	}
	if tool.Function.Name != "get_weather" {
		t.Errorf("function name: got %q", tool.Function.Name)
	}
	if tool.Function.Description != "Get weather" {
		t.Errorf("function description: got %q", tool.Function.Description)
	}
	var tc string
	_ = json.Unmarshal(cc.ToolChoice, &tc)
	if tc != "auto" {
		t.Errorf("tool_choice: got %q, want auto", tc)
	}
}

func TestRequestToCC_ToolChoiceFunction(t *testing.T) {
	body := `{"model":"gpt-4o","input":"use tool","tools":[{"type":"function","name":"fn","parameters":{}}],"tool_choice":{"type":"function","name":"fn"}}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(cc.ToolChoice, &obj); err != nil {
		t.Fatalf("tool_choice parse: %v — %s", err, cc.ToolChoice)
	}
	if obj.Type != "function" || obj.Function.Name != "fn" {
		t.Errorf("tool_choice object: got %+v", obj)
	}
}

func TestRequestToCC_ToolCallHistory(t *testing.T) {
	// Responses input: [user, function_call, function_call_output] →
	// CC: [user, assistant(tool_calls:[X]), tool(tool_call_id:X)]
	body := `{"model":"gpt-4o","input":[
		{"type":"message","role":"user","content":"what is the weather?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"location\":\"NYC\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}
	]}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc.Messages) != 3 {
		t.Fatalf("messages len: got %d, want 3; messages: %+v", len(cc.Messages), cc.Messages)
	}
	// [0] user message
	if cc.Messages[0].Role != "user" {
		t.Errorf("msg[0] role: got %q, want user", cc.Messages[0].Role)
	}
	// [1] assistant with tool_calls
	if cc.Messages[1].Role != "assistant" {
		t.Errorf("msg[1] role: got %q, want assistant", cc.Messages[1].Role)
	}
	if len(cc.Messages[1].ToolCalls) != 1 {
		t.Fatalf("tool_calls len: got %d, want 1", len(cc.Messages[1].ToolCalls))
	}
	tc := cc.Messages[1].ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call: got %+v", tc)
	}
	// [2] tool result
	if cc.Messages[2].Role != "tool" {
		t.Errorf("msg[2] role: got %q, want tool", cc.Messages[2].Role)
	}
	if cc.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool_call_id: got %q, want call_1", cc.Messages[2].ToolCallID)
	}
	var toolContent string
	_ = json.Unmarshal(cc.Messages[2].Content, &toolContent)
	if toolContent != "sunny" {
		t.Errorf("tool content: got %q, want sunny", toolContent)
	}
}

func TestRequestToCC_FunctionCallWithoutPrecedingAssistant(t *testing.T) {
	// A bare function_call item at the start should synthesize an assistant message.
	body := `{"model":"gpt-4o","input":[
		{"type":"function_call","call_id":"c1","name":"fn","arguments":"{}"}
	]}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc.Messages) != 1 {
		t.Fatalf("messages len: got %d, want 1", len(cc.Messages))
	}
	if cc.Messages[0].Role != "assistant" {
		t.Errorf("synthesized role: got %q", cc.Messages[0].Role)
	}
	if len(cc.Messages[0].ToolCalls) != 1 {
		t.Errorf("tool_calls len: got %d, want 1", len(cc.Messages[0].ToolCalls))
	}
}

func TestRequestToCC_ResponseFormatJSONSchema(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","text":{"format":{"type":"json_schema","name":"myschema","schema":{"type":"object"},"strict":true}}}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.ResponseFormat == nil {
		t.Fatal("response_format is nil")
	}
	if cc.ResponseFormat.Type != "json_schema" {
		t.Errorf("type: got %q, want json_schema", cc.ResponseFormat.Type)
	}
	var inner struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict bool            `json:"strict"`
	}
	if err := json.Unmarshal(cc.ResponseFormat.JSONSchema, &inner); err != nil {
		t.Fatalf("parse json_schema inner: %v", err)
	}
	if inner.Name != "myschema" {
		t.Errorf("name: got %q", inner.Name)
	}
	if !inner.Strict {
		t.Error("strict: expected true")
	}
}

func TestRequestToCC_ResponseFormatJSONObject(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","text":{"format":{"type":"json_object"}}}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.ResponseFormat == nil || cc.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format: %+v", cc.ResponseFormat)
	}
}

func TestRequestToCC_ResponseFormatText_Omitted(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","text":{"format":{"type":"text"}}}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.ResponseFormat != nil {
		t.Errorf("expected nil response_format for type=text, got %+v", cc.ResponseFormat)
	}
}

func TestRequestToCC_ReasoningEffort(t *testing.T) {
	body := `{"model":"o4-mini","input":"think hard","reasoning":{"effort":"high"}}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort: got %q, want high", cc.ReasoningEffort)
	}
}

func TestRequestToCC_ScalarFields(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","temperature":0.7,"top_p":0.9,"max_output_tokens":100,"user":"u1","stream":true,"parallel_tool_calls":false}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.Temperature == nil || *cc.Temperature != 0.7 {
		t.Errorf("temperature: %v", cc.Temperature)
	}
	if cc.TopP == nil || *cc.TopP != 0.9 {
		t.Errorf("top_p: %v", cc.TopP)
	}
	if cc.MaxTokens == nil || *cc.MaxTokens != 100 {
		t.Errorf("max_tokens: %v", cc.MaxTokens)
	}
	if cc.User != "u1" {
		t.Errorf("user: %q", cc.User)
	}
	if cc.Stream == nil || !*cc.Stream {
		t.Errorf("stream: %v", cc.Stream)
	}
	if cc.ParallelToolCalls == nil || *cc.ParallelToolCalls {
		t.Errorf("parallel_tool_calls: %v", cc.ParallelToolCalls)
	}
}

func TestRequestToCC_TopKDropped(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","top_k":10}`
	req := mustParseReq(t, body)
	// top_k is parsed into req but ignored in translation — should not error.
	_, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequestToCC_StopSequencesDropped(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","stop_sequences":["STOP"]}`
	req := mustParseReq(t, body)
	// stop_sequences have no CC equivalent — silently dropped (no error).
	_, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequestToCC_ReasoningItemDropped(t *testing.T) {
	// Reasoning input items should be silently dropped.
	body := `{"model":"gpt-4o","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","id":"r1","summary":[{"text":"I am thinking"}]}
	]}`
	req := mustParseReq(t, body)
	cc, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only user message — reasoning dropped.
	if len(cc.Messages) != 1 {
		t.Errorf("messages len: got %d, want 1 (reasoning should be dropped)", len(cc.Messages))
	}
}

// ---- rejection tests ----

func TestRequestToCC_Reject_PreviousResponseID(t *testing.T) {
	req := &responses.Request{
		Model:              "gpt-4o",
		Input:              []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		PreviousResponseID: "resp_abc",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "previous_response_id")
}

func TestRequestToCC_Reject_StoreTrue(t *testing.T) {
	req := &responses.Request{
		Model: "gpt-4o",
		Input: []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Store: boolPtr(true),
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "store")
}

func TestRequestToCC_StoreFalse_OK(t *testing.T) {
	// store=false is effectively the default — must NOT be rejected.
	req := &responses.Request{
		Model: "gpt-4o",
		Input: []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Store: boolPtr(false),
	}
	_, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("store=false should not be rejected: %v", err)
	}
}

func TestRequestToCC_Reject_Conversation(t *testing.T) {
	req := &responses.Request{
		Model:        "gpt-4o",
		Input:        []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Conversation: "conv_abc",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "conversation")
}

func TestRequestToCC_Reject_BackgroundTrue(t *testing.T) {
	req := &responses.Request{
		Model:      "gpt-4o",
		Input:      []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Background: boolPtr(true),
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "background")
}

func TestRequestToCC_BackgroundFalse_OK(t *testing.T) {
	req := &responses.Request{
		Model:      "gpt-4o",
		Input:      []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Background: boolPtr(false),
	}
	_, err := RequestToCC(req)
	if err != nil {
		t.Fatalf("background=false should not be rejected: %v", err)
	}
}

func TestRequestToCC_Reject_Truncation(t *testing.T) {
	req := &responses.Request{
		Model:      "gpt-4o",
		Input:      []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Truncation: "auto",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "truncation")
}

func TestRequestToCC_Reject_ServiceTier(t *testing.T) {
	req := &responses.Request{
		Model:       "gpt-4o",
		Input:       []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		ServiceTier: "flex",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "service_tier")
}

func TestRequestToCC_Reject_SafetyIdentifier(t *testing.T) {
	req := &responses.Request{
		Model:            "gpt-4o",
		Input:            []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		SafetyIdentifier: "sid_abc",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "safety_identifier")
}

func TestRequestToCC_Reject_PromptCacheKey(t *testing.T) {
	req := &responses.Request{
		Model:          "gpt-4o",
		Input:          []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		PromptCacheKey: "pk_abc",
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "prompt_cache_key")
}

func TestRequestToCC_Reject_ContextManagement(t *testing.T) {
	req := &responses.Request{
		Model:             "gpt-4o",
		Input:             []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		ContextManagement: json.RawMessage(`{"type":"window","max_tokens":1000}`),
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "context_management")
}

func TestRequestToCC_Reject_Include(t *testing.T) {
	req := &responses.Request{
		Model:   "gpt-4o",
		Input:   []responses.Item{&responses.Message{Role: responses.RoleUser, Content: []responses.Part{&responses.TextPart{Text: "x"}}}},
		Include: []string{"file_search_results"},
	}
	_, err := RequestToCC(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertRejection(t, err, "include")
}

func assertRejection(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection for field %q, got nil error", field)
	}
	msg := err.Error()
	if len(msg) < len("responses_unsupported_for_cc") {
		t.Errorf("error message too short: %q", msg)
		return
	}
	if msg[:len("responses_unsupported_for_cc")] != "responses_unsupported_for_cc" {
		t.Errorf("error should start with responses_unsupported_for_cc: %q", msg)
	}
	// Check field name appears in error.
	found := false
	for i := 0; i <= len(msg)-len(field); i++ {
		if msg[i:i+len(field)] == field {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("field %q not mentioned in error: %q", field, msg)
	}
}
