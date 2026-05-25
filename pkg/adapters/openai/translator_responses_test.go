package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
	"github.com/wyolet/relay/pkg/usage"
)

// --- ParseRequest ---

func TestResponsesParseRequest_SimpleStringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":"hi"}`)
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "gpt-5" {
		t.Errorf("model: %v", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
}

func TestResponsesParseRequest_ArrayInputForm(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "hi",
			}},
		}},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
}

func TestResponsesParseRequest_StatefulFieldsRejected(t *testing.T) {
	type mutation func(map[string]any)
	base := func() map[string]any {
		return map[string]any{
			"model": "gpt-5",
			"input": "hi",
		}
	}

	cases := []struct {
		name    string
		mutate  mutation
		wantErr string
	}{
		{
			"previous_response_id",
			func(m map[string]any) { m["previous_response_id"] = "resp_123" },
			"previous_response_id",
		},
		{
			"store_true",
			func(m map[string]any) { m["store"] = true },
			"store",
		},
		{
			"conversation",
			func(m map[string]any) { m["conversation"] = "conv_123" },
			"conversation",
		},
		{
			"background_true",
			func(m map[string]any) { m["background"] = true },
			"background",
		},
		{
			"truncation",
			func(m map[string]any) { m["truncation"] = "auto" },
			"truncation",
		},
		{
			"service_tier",
			func(m map[string]any) { m["service_tier"] = "premium" },
			"service_tier",
		},
		{
			"safety_identifier",
			func(m map[string]any) { m["safety_identifier"] = "safe_123" },
			"safety_identifier",
		},
		{
			"prompt_cache_key",
			func(m map[string]any) { m["prompt_cache_key"] = "pck_123" },
			"prompt_cache_key",
		},
		{
			"include",
			func(m map[string]any) { m["include"] = []string{"reasoning"} },
			"include",
		},
		{
			"context_management",
			func(m map[string]any) { m["context_management"] = map[string]any{"type": "auto"} },
			"context_management",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := mustJSON(func() map[string]any {
				m := base()
				tc.mutate(m)
				return m
			}())
			_, err := (ResponsesTranslator{}).ParseRequest(body)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestResponsesParseRequest_Tools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": "hi",
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "search",
			"description": "Search the web",
			"parameters":  json.RawMessage(schema),
		}},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools config")
	}
	if len(opts.Tools.Definitions) != 1 {
		t.Fatalf("tools len: %d", len(opts.Tools.Definitions))
	}
	ft, ok := opts.Tools.Definitions[0].(*v1.FunctionTool)
	if !ok {
		t.Fatalf("tool is %T", opts.Tools.Definitions[0])
	}
	if ft.Name != "search" {
		t.Errorf("tool name: %q", ft.Name)
	}
}

func TestResponsesParseRequest_ToolChoice(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	// ResponsesToolChoice wire format: a string shorthand ("auto", "required", "none")
	// or an object {"type":"function","name":"..."}.
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": "hi",
		"tools": []any{map[string]any{
			"type": "function", "name": "f", "parameters": json.RawMessage(schema),
		}},
		"tool_choice": "required",
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil || opts.Tools == nil || opts.Tools.Choice == nil {
		t.Fatal("expected tool choice")
	}
	if opts.Tools.Choice.Mode != "required" {
		t.Errorf("choice mode: %q", opts.Tools.Choice.Mode)
	}
}

func TestResponsesParseRequest_ParallelToolCalls(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": "hi",
		"tools": []any{map[string]any{
			"type": "function", "name": "f", "parameters": json.RawMessage(schema),
		}},
		"parallel_tool_calls": false,
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools")
	}
	if opts.Tools.Parallel == nil || *opts.Tools.Parallel != false {
		t.Errorf("parallel_tool_calls: %v", opts.Tools.Parallel)
	}
}

func TestResponsesParseRequest_SamplingFields(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":             "gpt-5",
		"input":             "hi",
		"temperature":       0.8,
		"top_p":             0.95,
		"max_output_tokens": 1024,
		"stop_sequences":    []string{"END"},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil || opts.Sampling == nil {
		t.Fatal("expected sampling")
	}
	if opts.Sampling.Temperature == nil || *opts.Sampling.Temperature != 0.8 {
		t.Errorf("temperature: %v", opts.Sampling.Temperature)
	}
	if opts.Sampling.TopP == nil || *opts.Sampling.TopP != 0.95 {
		t.Errorf("top_p: %v", opts.Sampling.TopP)
	}
	if opts.Sampling.MaxTokens == nil || *opts.Sampling.MaxTokens != 1024 {
		t.Errorf("max_tokens: %v", opts.Sampling.MaxTokens)
	}
	if len(opts.Sampling.Stop) != 1 || opts.Sampling.Stop[0] != "END" {
		t.Errorf("stop: %v", opts.Sampling.Stop)
	}
}

func TestResponsesParseRequest_ReasoningEffort(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":     "o3",
		"input":     "think",
		"reasoning": map[string]any{"effort": "high"},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["o3"]
	if opts == nil || opts.Reasoning == nil {
		t.Fatal("expected reasoning config")
	}
	if opts.Reasoning.Effort != "high" {
		t.Errorf("effort: %q", opts.Reasoning.Effort)
	}
}

func TestResponsesParseRequest_JSONSchemaFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": "hi",
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "my_schema",
				"schema": json.RawMessage(schema),
			},
		},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil || opts.Output == nil || opts.Output.Format == nil {
		t.Fatal("expected output format")
	}
	if opts.Output.Format.Type != "json_schema" {
		t.Errorf("format type: %q", opts.Output.Format.Type)
	}
	if opts.Output.Format.Name != "my_schema" {
		t.Errorf("format name: %q", opts.Output.Format.Name)
	}
}

func TestResponsesParseRequest_StreamTrue(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":  "gpt-5",
		"input":  "hi",
		"stream": true,
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputMode != v1.OutputModeStream {
		t.Errorf("output_mode: %q", req.OutputMode)
	}
}

// --- SerializeRequest ---

func TestResponsesSerializeRequest_SimpleMessage(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-5"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["model"] != "gpt-5" {
		t.Errorf("model: %v", m["model"])
	}
	var inputItems []any
	inputRaw, ok := m["input"]
	if !ok {
		t.Fatal("missing input")
	}
	switch v := inputRaw.(type) {
	case []any:
		inputItems = v
	default:
		t.Fatalf("input is %T", inputRaw)
	}
	if len(inputItems) != 1 {
		t.Fatalf("input len: %d", len(inputItems))
	}
}

func TestResponsesSerializeRequest_WithInstructions(t *testing.T) {
	req := &v1.Request{
		Model:        v1.ModelRefs{"gpt-5"},
		Instructions: "be concise",
		Input:        []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
	}
	b, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["instructions"] != "be concise" {
		t.Errorf("instructions: %v", m["instructions"])
	}
}

func TestResponsesSerializeRequest_MissingModel(t *testing.T) {
	req := &v1.Request{
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
	}
	_, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

// --- ParseResponse ---

func TestResponsesParseResponse_SimpleText(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_01",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "Hello!",
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
			"total_tokens":  15,
		},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_01" {
		t.Errorf("id: %q", resp.ID)
	}
	if resp.Status != v1.StatusCompleted {
		t.Errorf("status: %q", resp.Status)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	if resp.Usage["input"] != 10 {
		t.Errorf("usage: %v", resp.Usage)
	}
}

func TestResponsesParseResponse_ToolCall(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_02",
		"object":     "response",
		"created_at": int64(1700000001),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":      "function_call",
			"id":        "fc_01",
			"call_id":   "call_abc",
			"name":      "search",
			"arguments": `{"q":"golang"}`,
			"status":    "completed",
		}},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var foundFC bool
	for _, item := range resp.Output {
		if fc, ok := item.(*v1.FunctionCall); ok {
			if fc.CallID != "call_abc" {
				t.Errorf("call_id: %q", fc.CallID)
			}
			if fc.Name != "search" {
				t.Errorf("name: %q", fc.Name)
			}
			foundFC = true
		}
	}
	if !foundFC {
		t.Error("expected FunctionCall in output")
	}
}

func TestResponsesParseResponse_ReasoningItem(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_03",
		"object":     "response",
		"created_at": int64(1700000002),
		"model":      "o3",
		"status":     "completed",
		"output": []any{
			map[string]any{
				"type":   "reasoning",
				"id":     "rs_0",
				"status": "completed",
				"summary": []any{map[string]any{
					"type": "summary_text",
					"text": "Let me think step by step.",
				}},
			},
			map[string]any{
				"type":   "message",
				"id":     "msg_0",
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "The answer is 42.",
				}},
			},
		},
		"usage": map[string]any{"input_tokens": 50, "output_tokens": 30, "total_tokens": 80},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*v1.Reasoning); !ok {
		t.Errorf("output[0] is %T, want *v1.Reasoning", resp.Output[0])
	}
	if _, ok := resp.Output[1].(*v1.Message); !ok {
		t.Errorf("output[1] is %T, want *v1.Message", resp.Output[1])
	}
}

func TestResponsesParseResponse_Refusal(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_ref",
		"object":     "response",
		"created_at": int64(1700000003),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":    "refusal",
				"refusal": "I cannot help with that.",
			}},
		}},
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 8, "total_tokens": 13},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) == 0 {
		t.Fatal("expected output items")
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	// Refusal part maps to OutputTextPart (canonical rule 9).
	if len(msg.Content) == 0 {
		t.Error("expected refusal text in content")
	}
	tp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if tp.Text != "I cannot help with that." {
		t.Errorf("refusal text: %q", tp.Text)
	}
}

func TestResponsesParseResponse_UsageDetails(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_u",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output":     []any{},
		"usage": map[string]any{
			"input_tokens":          200,
			"output_tokens":         100,
			"total_tokens":          300,
			"input_tokens_details":  map[string]any{"cached_tokens": 150},
			"output_tokens_details": map[string]any{"reasoning_tokens": 40},
		},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Usage) == 0 {
		t.Fatal("usage is nil")
	}
	// Responses input_tokens=200 includes cached=150; canonical "input"
	// is non-cached only. Sum gives 200 back.
	if resp.Usage["input"] != 50 {
		t.Errorf("non-cached input: %d", resp.Usage["input"])
	}
	if resp.Usage["cache_read"] != 150 {
		t.Errorf("cache_read: %d", resp.Usage["cache_read"])
	}
	if resp.Usage["reasoning"] != 40 {
		t.Errorf("reasoning: %d", resp.Usage["reasoning"])
	}
}

// --- SerializeResponse ---

func TestResponsesSerializeResponse_SimpleText(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_01",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Message{
				ID:      "msg_0",
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Hello!"}},
			},
		},
		Usage: usage.Tokens{"input": 10, "output": 5},
	}
	req := &v1.Request{Model: v1.ModelRefs{"gpt-5"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["object"] != "response" {
		t.Errorf("object: %v", m["object"])
	}
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	output, ok := m["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output: %v", m["output"])
	}
}

func TestResponsesSerializeResponse_RequestEchoFields(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_echo",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}
	req := &v1.Request{
		Model:        v1.ModelRefs{"gpt-5"},
		Instructions: "be helpful",
		User:         "user-123",
		Metadata:     map[string]string{"k": "v"},
		ModelConfig: map[string]*v1.ModelOpts{
			"gpt-5": {
				Sampling: &v1.SamplingParams{
					Temperature: floatPtr(0.7),
				},
			},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	// Spec requires echo fields on the response.
	if m["instructions"] != "be helpful" {
		t.Errorf("instructions: %v", m["instructions"])
	}
}

func TestResponsesSerializeResponse_NilRequestAllowed(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_nil",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}
	_, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatalf("unexpected error with nil req: %v", err)
	}
}

func TestResponsesSerializeResponse_IncompleteStatus(t *testing.T) {
	resp := &v1.Response{
		ID:                "resp_inc",
		Model:             "gpt-5",
		Status:            v1.StatusIncomplete,
		FinishReason:      v1.FinishReasonLength,
		IncompleteDetails: &v1.IncompleteDetails{Reason: "max_output_tokens"},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["status"] != "incomplete" {
		t.Errorf("status: %v", m["status"])
	}
}

// --- ParseResponse / SerializeResponse round-trip ---

func TestResponsesSerializeResponse_ToolCallOutput(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_fc",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
		Output: []v1.Item{
			&v1.FunctionCall{
				ID:        "fc_0",
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
				Status:    v1.StatusCompleted,
			},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason: %v", m["finish_reason"])
	}
	output := m["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len: %d", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Errorf("item type: %v", item["type"])
	}
}

func TestResponsesRoundTrip_Response(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_rt",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "Round trip.",
			}},
		}},
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
	})

	tr := ResponsesTranslator{}
	req := &v1.Request{Model: v1.ModelRefs{"gpt-5"}}
	resp, err := tr.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := tr.SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b2)
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	output := m["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len: %d", len(output))
	}
}

// --- NewToCanonicalStream (Responses → canonical) ---

func responsesSSEChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func TestResponsesNewToCanonicalStream_TextSequence(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		responsesSSEChunk(ResponsesEventCreated, ResponsesCreatedEvent{
			Response: &ResponsesResponse{
				ID:     "resp_s1",
				Object: "response",
				Model:  "gpt-5",
				Status: ResponsesStatusInProgress,
				Output: []ResponsesItem{},
			},
		}),
		responsesSSEChunk(ResponsesEventOutputItemAdded, ResponsesItemAddedEvent{
			OutputIndex: 0,
			Item:        &ResponsesMessage{ID: "msg_0", Role: ResponsesRoleAssistant, Status: ResponsesStatusInProgress},
		}),
		responsesSSEChunk(ResponsesEventOutputTextDelta, ResponsesOutputTextDeltaEvent{
			ItemID: "msg_0", OutputIndex: 0, ContentIndex: 0, Delta: "Hello",
		}),
		responsesSSEChunk(ResponsesEventOutputTextDelta, ResponsesOutputTextDeltaEvent{
			ItemID: "msg_0", OutputIndex: 0, ContentIndex: 0, Delta: " world",
		}),
		responsesSSEChunk(ResponsesEventOutputItemDone, ResponsesOutputItemDoneEvent{
			OutputIndex: 0,
			Item: &ResponsesMessage{
				ID:      "msg_0",
				Role:    ResponsesRoleAssistant,
				Status:  ResponsesStatusCompleted,
				Content: []ResponsesPart{&ResponsesOutputTextPart{Text: "Hello world"}},
			},
		}),
		responsesSSEChunk(ResponsesEventCompleted, ResponsesCompletedEvent{
			Response: &ResponsesResponse{
				ID:           "resp_s1",
				Object:       "response",
				Model:        "gpt-5",
				Status:       ResponsesStatusCompleted,
				FinishReason: ResponsesFinishReasonStop,
				Output:       []ResponsesItem{},
			},
		}),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	wantContains := []string{
		v1.EventGenerationCreated,
		v1.EventItemStarted,
		v1.EventItemDelta,
		v1.EventItemCompleted,
		v1.EventGenerationCompleted,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestResponsesNewToCanonicalStream_FunctionCallDelta(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		responsesSSEChunk(ResponsesEventCreated, ResponsesCreatedEvent{
			Response: &ResponsesResponse{ID: "resp_fc", Model: "gpt-5", Status: ResponsesStatusInProgress, Output: []ResponsesItem{}},
		}),
		responsesSSEChunk(ResponsesEventOutputItemAdded, ResponsesItemAddedEvent{
			OutputIndex: 0,
			Item:        &ResponsesFunctionCall{ID: "fc_0", CallID: "call_abc", Name: "search", Status: ResponsesStatusInProgress},
		}),
		responsesSSEChunk(ResponsesEventFunctionCallArgumentsDelta, ResponsesFunctionCallArgumentsDeltaEvent{
			ItemID: "fc_0", OutputIndex: 0, CallID: "call_abc", Delta: `{"q":"golang"}`,
		}),
		responsesSSEChunk(ResponsesEventOutputItemDone, ResponsesOutputItemDoneEvent{
			OutputIndex: 0,
			Item: &ResponsesFunctionCall{
				ID: "fc_0", CallID: "call_abc", Name: "search",
				Arguments: `{"q":"golang"}`, Status: ResponsesStatusCompleted,
			},
		}),
		responsesSSEChunk(ResponsesEventCompleted, ResponsesCompletedEvent{
			Response: &ResponsesResponse{ID: "resp_fc", Model: "gpt-5", Status: ResponsesStatusCompleted, Output: []ResponsesItem{}},
		}),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	wantContains := []string{v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestResponsesNewToCanonicalStream_UnknownEventsDropped(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	// Unknown event type should produce no output without error.
	chunk := responsesSSEChunk("response.unknown_future_event", map[string]any{"type": "unknown"})
	out, err := fn(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := extractCanonicalEvents(out)
	if len(events) != 0 {
		t.Errorf("expected no events for unknown type, got %v", events)
	}
}

// --- NewFromCanonicalStream (canonical → Responses) ---

func canonicalChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func TestResponsesNewFromCanonicalStream_TextSequence(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewFromCanonicalStream()
	if fn == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}

	chunks := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{
			ID: "resp_s1", Model: "gpt-5",
		}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{
			ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello",
		}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{
			ItemID: "msg_0",
			Index:  0,
			Item: &v1.Message{
				ID:      "msg_0",
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Hello"}},
			},
		}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID:           "resp_s1",
			Status:       v1.StatusCompleted,
			FinishReason: v1.FinishReasonStop,
			Usage:        usage.Tokens{"input": 5, "output": 3},
		}),
	}

	var responsesEvents []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		responsesEvents = append(responsesEvents, extractResponsesEvents(out)...)
	}

	wantContains := []string{
		ResponsesEventCreated,
		ResponsesEventOutputItemAdded,
		ResponsesEventOutputTextDelta,
		ResponsesEventOutputItemDone,
		ResponsesEventCompleted,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range responsesEvents {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in %v", want, responsesEvents)
		}
	}
}

func TestResponsesNewFromCanonicalStream_FunctionCall(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewFromCanonicalStream()

	chunks := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_fc", Model: "gpt-5"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{
			ItemID: "fc_0", ItemType: v1.ItemTypeFunctionCall, Index: 0,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"q":"golang"}`,
		}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{
			ItemID: "fc_0",
			Index:  0,
			Item: &v1.FunctionCall{
				ID:        "fc_0",
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
				Status:    v1.StatusCompleted,
			},
		}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID: "resp_fc", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonToolCalls,
		}),
	}

	var responsesEvents []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		responsesEvents = append(responsesEvents, extractResponsesEvents(out)...)
	}

	wantContains := []string{
		ResponsesEventFunctionCallArgumentsDelta,
		ResponsesEventFunctionCallArgumentsDone,
		ResponsesEventOutputItemDone,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range responsesEvents {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in %v", want, responsesEvents)
		}
	}
}

// extractResponsesEvents parses concatenated Responses SSE bytes and collects event names.
func extractResponsesEvents(b []byte) []string {
	var names []string
	for _, frame := range splitCanonicalFrames(b) {
		event, _, ok := ParseResponsesSSEChunk(frame)
		if ok && event != "" {
			names = append(names, event)
		}
	}
	return names
}

// --- E2E: Responses → canonical → CC wire → canonical → Responses ---

func TestE2E_ResponsesViaCC(t *testing.T) {
	// 1. Responses wire request.
	inputBytes := mustJSON(map[string]any{
		"model":       "gpt-4o",
		"input":       "hello",
		"temperature": 0.5,
	})

	// 2. Parse to canonical via ResponsesTranslator.
	rt := ResponsesTranslator{}
	canonReq, err := rt.ParseRequest(inputBytes)
	if err != nil {
		t.Fatalf("ResponsesTranslator.ParseRequest: %v", err)
	}
	if len(canonReq.Model) != 1 || canonReq.Model[0] != "gpt-4o" {
		t.Errorf("model: %v", canonReq.Model)
	}

	// 3. Serialize to CC wire via CCTranslator.
	ccT := CCTranslator{}
	ccBody, err := ccT.SerializeRequest(canonReq)
	if err != nil {
		t.Fatalf("CCTranslator.SerializeRequest: %v", err)
	}
	ccMap := decodeMap(t, ccBody)
	if ccMap["model"] != "gpt-4o" {
		t.Errorf("cc model: %v", ccMap["model"])
	}
	// Should have messages array (not input array).
	if _, ok := ccMap["messages"]; !ok {
		t.Error("cc body missing messages field")
	}

	// 4. Mock CC response.
	ccResp := mustJSON(map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Hello there!"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})

	// 5. Parse CC response to canonical.
	canonResp, err := ccT.ParseResponse(ccResp)
	if err != nil {
		t.Fatalf("CCTranslator.ParseResponse: %v", err)
	}

	// 6. Serialize canonical → Responses wire.
	respBytes, err := rt.SerializeResponse(canonResp, canonReq)
	if err != nil {
		t.Fatalf("ResponsesTranslator.SerializeResponse: %v", err)
	}

	// 7. Verify well-formed Responses response with echo fields.
	m := decodeMap(t, respBytes)
	if m["object"] != "response" {
		t.Errorf("object: %v", m["object"])
	}
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	if m["model"] != "gpt-4o" {
		t.Errorf("model: %v", m["model"])
	}
	// Temperature was in original Responses request → should echo.
	if m["temperature"] != 0.5 {
		t.Errorf("temperature echo: %v", m["temperature"])
	}
	output, ok := m["output"].([]any)
	if !ok || len(output) == 0 {
		t.Errorf("output: %v", m["output"])
	}
}

func TestE2E_StreamingCCToResponses(t *testing.T) {
	ccToCanon := (CCTranslator{}).NewToCanonicalStream()
	canonToResp := (ResponsesTranslator{}).NewFromCanonicalStream()
	if canonToResp == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}

	cs := NewComposedStream(ccToCanon, canonToResp)

	ccChunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-e2e",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Hello"},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-e2e",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": " world"},
			}},
		}),
		ccDoneChunk(),
	}

	var allFrames []ResponsesSSEFrame
	for _, c := range ccChunks {
		frames, err := cs.Translate(c)
		if err != nil {
			t.Fatalf("ComposedStream.Translate: %v", err)
		}
		allFrames = append(allFrames, frames...)
	}

	// Composed stream must produce at least response.created and response.completed.
	var eventNames []string
	for _, f := range allFrames {
		eventNames = append(eventNames, f.Event)
	}

	wantContains := []string{ResponsesEventCreated, ResponsesEventCompleted}
	for _, want := range wantContains {
		found := false
		for _, e := range eventNames {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in composed stream output %v", want, eventNames)
		}
	}

	// Every frame must have non-empty data.
	for i, f := range allFrames {
		if len(f.Data) == 0 {
			t.Errorf("frame[%d] event=%q has empty data", i, f.Event)
		}
	}
}

// R-1: encrypted_content round-trips through ProviderData.
func TestResponsesParseResponse_EncryptedContentRoundTrip(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":            "resp_enc",
		"object":        "response",
		"created_at":    1000,
		"model":         "o1",
		"status":        "completed",
		"finish_reason": "stop",
		"output": []any{map[string]any{
			"type":              "reasoning",
			"id":                "rs_abc",
			"status":            "completed",
			"encrypted_content": "ENCBLOB",
			"summary":           []any{map[string]any{"text": "the answer is 42"}},
		}},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if len(r.ProviderData) == 0 {
		t.Fatal("ProviderData must not be empty when encrypted_content is present")
	}
	var pd struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(r.ProviderData, &pd); err != nil {
		t.Fatalf("ProviderData unmarshal: %v", err)
	}
	if pd.EncryptedContent != "ENCBLOB" {
		t.Errorf("encrypted_content: %q", pd.EncryptedContent)
	}

	// Serialize back and confirm encrypted_content is restored in the wire body.
	req := &v1.Request{Model: v1.ModelRefs{"o1"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	var wireResp struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(b, &wireResp); err != nil {
		t.Fatal(err)
	}
	if len(wireResp.Output) != 1 {
		t.Fatalf("serialized output len: %d", len(wireResp.Output))
	}
	var wireItem struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(wireResp.Output[0], &wireItem); err != nil {
		t.Fatal(err)
	}
	if wireItem.EncryptedContent != "ENCBLOB" {
		t.Errorf("round-tripped encrypted_content: %q", wireItem.EncryptedContent)
	}
}

// R-2: response.refusal.delta events map to text item.delta in canonical stream.
func TestResponsesNewToCanonicalStream_RefusalDelta(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(responsesSSEChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(ResponsesEventCreated, ResponsesCreatedEvent{Response: &ResponsesResponse{
		ID: "resp_ref", Model: "gpt-4o", Status: ResponsesStatusInProgress,
	}})
	feed(ResponsesEventOutputItemAdded, map[string]any{
		"output_index": 0,
		"item":         map[string]any{"type": "message", "id": "msg_0", "role": "assistant"},
	})
	feed(ResponsesEventRefusalDelta, ResponsesRefusalDeltaEvent{
		ItemID: "msg_0", OutputIndex: 0, Delta: "I cannot help with that",
	})
	feed(ResponsesEventRefusalDone, ResponsesRefusalDoneEvent{
		ItemID: "msg_0", OutputIndex: 0, Refusal: "I cannot help with that",
	})
	feed(ResponsesEventCompleted, ResponsesCompletedEvent{Response: &ResponsesResponse{
		ID: "resp_ref", Model: "gpt-4o", Status: ResponsesStatusCompleted,
		FinishReason: ResponsesFinishReasonStop,
	}})

	events := extractCanonicalEvents(allOut)
	hasDelta := false
	for _, e := range events {
		if e == v1.EventItemDelta {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Errorf("expected item.delta for refusal text, got: %v", events)
	}
	if !strings.Contains(string(allOut), "I cannot help with that") {
		t.Errorf("refusal text missing from output")
	}
}

// R-2: response.failed must emit generation.completed so the consumer isn't hung.
func TestResponsesNewToCanonicalStream_ResponseFailed(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()

	feed := func(event string, data any) []byte {
		out, err := fn(responsesSSEChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		return out
	}

	var allOut []byte
	allOut = append(allOut, feed(ResponsesEventCreated, ResponsesCreatedEvent{Response: &ResponsesResponse{
		ID: "resp_fail", Model: "gpt-4o", Status: ResponsesStatusInProgress,
	}})...)
	allOut = append(allOut, feed(ResponsesEventFailed, ResponsesFailedEvent{Response: &ResponsesResponse{
		ID: "resp_fail", Model: "gpt-4o", Status: ResponsesStatusFailed,
	}})...)

	events := extractCanonicalEvents(allOut)
	hasCompleted := false
	for _, e := range events {
		if e == v1.EventGenerationCompleted {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Errorf("expected generation.completed after response.failed, got: %v", events)
	}
}

// R-3: canonical→Responses streaming emits non-empty call_id and name on function call events.
func TestResponsesNewFromCanonicalStream_FunctionCallHasNameAndCallID(t *testing.T) {
	fn := (ResponsesTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_r3", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_r3", ItemType: v1.ItemTypeFunctionCall, Index: 0, Name: "search"})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_r3", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"q":"go"}`})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{
		ItemID: "fc_r3",
		Index:  0,
		Item: &v1.FunctionCall{
			ID:        "fc_r3",
			CallID:    "call_r3",
			Name:      "search",
			Arguments: `{"q":"go"}`,
			Status:    v1.StatusCompleted,
		},
	})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
		ID:           "resp_r3",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
	})

	allStr := string(allOut)
	if !strings.Contains(allStr, "search") {
		t.Errorf("function name 'search' missing from output")
	}
	// Arguments delta events must carry a non-empty call_id.
	var foundArgsDelta bool
	for _, raw := range splitSSEFrames(allOut) {
		evtName, data, ok := ParseResponsesSSEChunk(raw)
		if !ok || evtName != ResponsesEventFunctionCallArgumentsDelta {
			continue
		}
		foundArgsDelta = true
		var delta ResponsesFunctionCallArgumentsDeltaEvent
		if err := json.Unmarshal(data, &delta); err != nil {
			t.Fatalf("unmarshal delta: %v", err)
		}
		if delta.CallID == "" {
			t.Errorf("call_id must not be empty in function_call_arguments.delta")
		}
	}
	if !foundArgsDelta {
		t.Errorf("no function_call_arguments.delta event found")
	}
}

// R-4: file_citation annotations are preserved as v1.RawAnnotation and round-trip.
func TestResponsesAnnotation_FileCitationPreserved(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_fc",
		"object":     "response",
		"created_at": 1000,
		"model":      "gpt-4o",
		"status":     "completed",
		"output": []any{map[string]any{
			"type": "message",
			"id":   "msg_fc",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "See file [1].",
				"annotations": []any{map[string]any{
					"type":    "file_citation",
					"file_id": "file_abc",
					"index":   3,
				}},
				"logprobs": []any{},
			}},
		}},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	otp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if len(otp.Annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(otp.Annotations))
	}
	raw, ok := otp.Annotations[0].(*v1.RawAnnotation)
	if !ok {
		t.Fatalf("annotation is %T, want *v1.RawAnnotation", otp.Annotations[0])
	}
	if raw.Type != "file_citation" {
		t.Errorf("annotation type: %q", raw.Type)
	}
	if !strings.Contains(string(raw.JSON), "file_abc") {
		t.Errorf("file_id missing from RawAnnotation JSON: %s", raw.JSON)
	}

	// Round-trip: confirm file_citation survives SerializeResponse.
	req := &v1.Request{Model: v1.ModelRefs{"gpt-4o"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "file_citation") {
		t.Errorf("file_citation missing after round-trip SerializeResponse")
	}
}

// R-5: reasoning.summary is mapped to the Responses wire request field.
func TestResponsesSerializeRequest_ReasoningSummary(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"o3"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "think"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			"o3": {
				Reasoning: &v1.ReasoningConfig{Effort: "high", Summary: "detailed"},
			},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := wire["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning field missing or wrong type: %v", wire["reasoning"])
	}
	if reasoning["summary"] != "detailed" {
		t.Errorf("reasoning.summary: %v", reasoning["summary"])
	}
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort: %v", reasoning["effort"])
	}
}

// R-3 also needs splitSSEFrames — it's already defined in translator_responses.go
// but the test uses ParseResponsesSSEChunk which is in the same package.
