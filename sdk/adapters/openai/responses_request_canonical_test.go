package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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

func TestResponsesCacheKeyRoundTrip(t *testing.T) {
	// Inbound: prompt_cache_key / prompt_cache_retention map to canonical
	// CacheConfig.Key / CacheConfig.TTL.
	body := mustJSON(map[string]any{
		"model":                  "gpt-5",
		"input":                  "hi",
		"prompt_cache_key":       "conv-abc123",
		"prompt_cache_retention": "24h",
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.CacheConfig == nil || req.CacheConfig.Key != "conv-abc123" {
		t.Fatalf("CacheConfig = %+v, want Key=conv-abc123", req.CacheConfig)
	}
	if req.CacheConfig.TTL != "24h" {
		t.Errorf("CacheConfig.TTL = %q, want 24h", req.CacheConfig.TTL)
	}

	// Outbound: canonical Key/TTL are emitted as prompt_cache_key/_retention.
	out, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("SerializeRequest: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["prompt_cache_key"] != "conv-abc123" {
		t.Errorf("wire prompt_cache_key = %v, want conv-abc123", wire["prompt_cache_key"])
	}
	if wire["prompt_cache_retention"] != "24h" {
		t.Errorf("wire prompt_cache_retention = %v, want 24h", wire["prompt_cache_retention"])
	}

	// Short TTL maps to the in-memory tier and round-trips.
	req.CacheConfig.TTL = "5m"
	out, err = (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("SerializeRequest (5m): %v", err)
	}
	wire = nil
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["prompt_cache_retention"] != "in_memory" {
		t.Errorf("wire prompt_cache_retention = %v, want in_memory", wire["prompt_cache_retention"])
	}

	// No key → field absent on the wire.
	req.CacheConfig = nil
	out, err = (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("SerializeRequest (no key): %v", err)
	}
	wire = nil
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if _, present := wire["prompt_cache_key"]; present {
		t.Errorf("prompt_cache_key present without CacheConfig.Key: %v", wire["prompt_cache_key"])
	}
	if _, present := wire["prompt_cache_retention"]; present {
		t.Errorf("prompt_cache_retention present without CacheConfig.TTL: %v", wire["prompt_cache_retention"])
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
	if req.Tools == nil {
		t.Fatal("expected tools config")
	}
	if len(req.Tools.Definitions) != 1 {
		t.Fatalf("tools len: %d", len(req.Tools.Definitions))
	}
	ft, ok := req.Tools.Definitions[0].(*v1.FunctionTool)
	if !ok {
		t.Fatalf("tool is %T", req.Tools.Definitions[0])
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
	if req.Tools == nil || req.Tools.Choice == nil {
		t.Fatal("expected tool choice")
	}
	if req.Tools.Choice.Mode != "required" {
		t.Errorf("choice mode: %q", req.Tools.Choice.Mode)
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
	if req.Tools == nil {
		t.Fatal("expected tools")
	}
	if req.Tools.Parallel == nil || *req.Tools.Parallel != false {
		t.Errorf("parallel_tool_calls: %v", req.Tools.Parallel)
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

// --- Hosted-tool raw passthrough (PR2) ---

// TestResponsesParseRequest_HostedToolDefNoError locks the request-side fix:
// a hosted-tool definition must not 400 the request; the function tool alongside
// it survives to canonical.
func TestResponsesParseRequest_HostedToolDefNoError(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-5", "input": "hi",
		"tools": []any{
			map[string]any{"type": "web_search"},
			map[string]any{"type": "function", "name": "f", "parameters": json.RawMessage(`{"type":"object"}`)},
		},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("hosted-tool def must not error the request: %v", err)
	}
	if req.Tools == nil || len(req.Tools.Definitions) != 1 {
		t.Fatalf("want 1 surviving function tool, got %v", req.Tools)
	}
	if ft, ok := req.Tools.Definitions[0].(*v1.FunctionTool); !ok || ft.Name != "f" {
		t.Fatalf("surviving tool: %v", req.Tools.Definitions[0])
	}
}

// TestResponsesParseRequest_HostedToolInputItemNoError: a round-tripped hosted-tool
// item in the input array must not 400 the request.
func TestResponsesParseRequest_HostedToolInputItemNoError(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{"type": "web_search_call", "id": "ws_0", "status": "completed"},
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("hosted-tool input item must not error: %v", err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: %d (web_search_call drops, message survives)", len(req.Input))
	}
}

// TestResponsesToolChoice_ObjectModes locks the fix for object-form tool_choice:
// anything other than auto/required/none must serialize as an OBJECT, never a
// bare string (OpenAI 400s "Invalid value: 'file_search'").
func TestResponsesToolChoice_ObjectModes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // canonical JSON we expect back out
	}{
		{"auto", `"auto"`, `"auto"`},
		{"required", `"required"`, `"required"`},
		{"none", `"none"`, `"none"`},
		{"function", `{"type":"function","name":"f"}`, `{"type":"function","name":"f"}`},
		{"hosted file_search", `{"type":"file_search"}`, `{"type":"file_search"}`},
		{"allowed_tools verbatim", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}`,
			`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}`},
		{"mcp verbatim", `{"type":"mcp","server_label":"s"}`, `{"type":"mcp","server_label":"s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ResponsesToolChoice
			if err := json.Unmarshal([]byte(tc.in), &c); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("round-trip:\n got %s\nwant %s", out, tc.want)
			}
		})
	}
}

// TestResponsesToolChoice_HostedReconstructedFromCanonical: a hosted-tool choice
// rebuilt from canonical (Mode only, Raw lost) must still serialize as {type:...},
// not a bare string — the realistic Responses->canonical->Responses path to a
// non-"openai" host.
func TestResponsesToolChoice_HostedReconstructedFromCanonical(t *testing.T) {
	c := ResponsesToolChoice{Mode: "file_search"} // no Raw
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"type":"file_search"}` {
		t.Errorf("got %s, want {\"type\":\"file_search\"}", out)
	}
}

// TestResponsesSerializeRequest_HostedToolChoiceObject: end-to-end through the
// translator — an inbound hosted-tool tool_choice must leave SerializeRequest as
// an object, not a bare string.
func TestResponsesSerializeRequest_HostedToolChoiceObject(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-5", "input": "hi",
		"tools":       []any{map[string]any{"type": "function", "name": "f", "parameters": json.RawMessage(`{"type":"object"}`)}},
		"tool_choice": map[string]any{"type": "file_search"},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	tcv, ok := m["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice must be an object, got %T: %v", m["tool_choice"], m["tool_choice"])
	}
	if tcv["type"] != "file_search" {
		t.Errorf("tool_choice.type: %v", tcv["type"])
	}
}

// --- P1 request-config fidelity (PR4) ---

// TestResponsesParseRequest_VerbosityReasoningSummaryDescription locks the inbound
// mapping of text.verbosity, reasoning.summary, and text.format.json_schema.description
// into canonical — all three were previously silently dropped on parse.
func TestResponsesParseRequest_VerbosityReasoningSummaryDescription(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":     "gpt-5",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "high", "summary": "detailed"},
		"text": map[string]any{
			"verbosity": "low",
			"format": map[string]any{
				"type": "json_schema", "name": "S", "description": "a schema",
				"schema": json.RawMessage(`{"type":"object"}`),
			},
		},
	})
	req, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-5"]
	if opts == nil {
		t.Fatal("no model config")
	}
	if opts.Reasoning == nil || opts.Reasoning.Summary != "detailed" {
		t.Errorf("reasoning.summary: %+v", opts.Reasoning)
	}
	if opts.Output == nil || opts.Output.Verbosity != "low" {
		t.Errorf("verbosity: %+v", opts.Output)
	}
	if opts.Output.Format == nil || opts.Output.Format.Description != "a schema" {
		t.Errorf("format.description: %+v", opts.Output.Format)
	}
}

// TestResponsesSerializeRequest_VerbosityDescription locks the outbound mapping:
// canonical verbosity + format.description must reach the Responses wire body.
func TestResponsesSerializeRequest_VerbosityDescription(t *testing.T) {
	strict := true
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-5"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		ModelConfig: map[string]*v1.ModelOpts{"gpt-5": {
			Output: &v1.OutputConfig{
				Verbosity: "high",
				Format:    &v1.Format{Type: "json_schema", Name: "S", Description: "d", Schema: json.RawMessage(`{}`), Strict: &strict},
			},
		}},
	}
	b, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	text, ok := m["text"].(map[string]any)
	if !ok {
		t.Fatalf("text missing: %v", m["text"])
	}
	if text["verbosity"] != "high" {
		t.Errorf("verbosity: %v", text["verbosity"])
	}
	f, ok := text["format"].(map[string]any)
	if !ok || f["description"] != "d" {
		t.Errorf("format.description: %v", text["format"])
	}
}

// TestResponsesParseRequest_PromptRejected: a stored prompt-template ref is
// stateful and must fail loud cross-shape, not silently drop the instructions.
func TestResponsesParseRequest_PromptRejected(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":  "gpt-5",
		"input":  "hi",
		"prompt": map[string]any{"id": "pmpt_123", "version": "2"},
	})
	_, err := (ResponsesTranslator{}).ParseRequest(body)
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("expected prompt rejection, got %v", err)
	}
}

// Cross-provider replay: canonical transcripts carry relay-scoped item ids
// (the anthropic adapter mints t_0/m_0/r_0...), but the Responses API treats
// input[N].id as a reference to an object IT minted and 400s on foreign ids
// ("Invalid 'input[5].id': 't_0'. Expected an ID that begins with 'rs'").
// Serialize must strip ids from message/function_call input items, drop
// foreign reasoning items (provider-signed, no cross-vendor round-trip), and
// keep OpenAI-native reasoning (encrypted_content) with its original rs_ id.
func TestResponsesSerializeRequest_ForeignItemIDs(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-5.5"},
		OutputMode: v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{ID: "m_0", Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
			&v1.Reasoning{ID: "r_0", Content: "thinking...", Status: v1.StatusCompleted,
				ProviderData: json.RawMessage(`{"type":"thinking","thinking":"...","signature":"anthropic-signed"}`)},
			&v1.FunctionCall{ID: "t_0", CallID: "toolu_01", Name: "search", Arguments: `{"q":"x"}`, Status: v1.StatusCompleted},
			&v1.FunctionCallOutput{CallID: "toolu_01", Output: "result"},
			&v1.Reasoning{ID: "rs_native1", Status: v1.StatusCompleted,
				ProviderData: json.RawMessage(`{"encrypted_content":"BLOB123"}`)},
		},
	}

	body, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	var wire struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}

	var types []string
	for _, item := range wire.Input {
		typ, _ := item["type"].(string)
		types = append(types, typ)
		id, _ := item["id"].(string)
		switch typ {
		case "message", "function_call":
			if id != "" {
				t.Errorf("%s input item carries id %q — foreign ids must be stripped", typ, id)
			}
		case "reasoning":
			if id != "rs_native1" {
				t.Errorf("reasoning input item id: got %q want rs_native1", id)
			}
			if ec, _ := item["encrypted_content"].(string); ec != "BLOB123" {
				t.Errorf("reasoning encrypted_content: got %q", ec)
			}
		}
	}

	want := []string{"message", "function_call", "function_call_output", "reasoning"}
	if len(types) != len(want) {
		t.Fatalf("input items: got %v want %v (foreign reasoning must be dropped)", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("input item order: got %v want %v", types, want)
		}
	}

	// FunctionCall linkage must survive via call_id.
	if wire.Input[1]["call_id"] != "toolu_01" || wire.Input[2]["call_id"] != "toolu_01" {
		t.Errorf("call_id linkage lost: %v / %v", wire.Input[1]["call_id"], wire.Input[2]["call_id"])
	}
}

// A media-carrying tool result (image part from e.g. a file read of a PNG)
// must reach the Responses wire as a content array with input_image — not be
// dropped, and never yield a bodiless function_call_output.
func TestResponsesSerializeRequest_ToolResultWithImage(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-5.5"},
		Input: []v1.Item{
			&v1.FunctionCall{CallID: "call_img", Name: "fs_read", Arguments: `{"path":"a.png"}`, Status: v1.StatusCompleted},
			&v1.FunctionCallOutput{CallID: "call_img", Content: []v1.Part{
				&v1.ImagePart{ImageURL: "data:image/png;base64,AAAA"},
			}},
		},
	}
	body, err := (ResponsesTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	var wire struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, item := range wire.Input {
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ != "function_call_output" {
			continue
		}
		content, hasContent := item["content"]
		_, hasOutput := item["output"]
		if !hasContent && !hasOutput {
			t.Fatalf("bodiless function_call_output: %v", item)
		}
		if !hasContent || !strings.Contains(string(content), "input_image") {
			t.Fatalf("image part lost from tool result: content=%s", content)
		}
		return
	}
	t.Fatal("no function_call_output in input")
}
