package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

var tr GeminiTranslator

// ---- ParseRequest tests ----

func TestParseRequest_Simple(t *testing.T) {
	body := `{"model":"gemini-1.5-pro","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) == 0 || req.Model[0] != "gemini-1.5-pro" {
		t.Errorf("model: got %v", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: got %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("item type: %T", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: got %s", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*v1.TextPart)
	if !ok {
		t.Fatalf("part type: %T", msg.Content[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestParseRequest_SystemInstruction(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"You are helpful."}]}}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Instructions != "You are helpful." {
		t.Errorf("instructions: %q", req.Instructions)
	}
}

func TestParseRequest_MultiTurn(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"ping"}]},
		{"role":"model","parts":[{"text":"pong"}]},
		{"role":"user","parts":[{"text":"again"}]}
	]}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	roles := []v1.Role{v1.RoleUser, v1.RoleAssistant, v1.RoleUser}
	for i, role := range roles {
		msg, ok := req.Input[i].(*v1.Message)
		if !ok {
			t.Fatalf("[%d] not a Message: %T", i, req.Input[i])
		}
		if msg.Role != role {
			t.Errorf("[%d] role: got %s want %s", i, msg.Role, role)
		}
	}
}

func TestParseRequest_Sampling(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTok := 512
	body := jsonMust(map[string]any{
		"model": "gemini-1.5-pro",
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
		},
		"generationConfig": map[string]any{
			"temperature":     temp,
			"topP":            topP,
			"maxOutputTokens": maxTok,
			"stopSequences":   []string{"STOP"},
		},
	})
	req, err := tr.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gemini-1.5-pro"]
	if opts == nil {
		t.Fatal("no ModelConfig for model")
	}
	sp := opts.Sampling
	if sp == nil {
		t.Fatal("no sampling")
	}
	if sp.Temperature == nil || *sp.Temperature != temp {
		t.Errorf("temperature: %v", sp.Temperature)
	}
	if sp.TopP == nil || *sp.TopP != topP {
		t.Errorf("topP: %v", sp.TopP)
	}
	if sp.MaxTokens == nil || *sp.MaxTokens != maxTok {
		t.Errorf("maxTokens: %v", sp.MaxTokens)
	}
	if len(sp.Stop) != 1 || sp.Stop[0] != "STOP" {
		t.Errorf("stop: %v", sp.Stop)
	}
}

func TestParseRequest_Tools(t *testing.T) {
	body := `{
		"contents":[{"role":"user","parts":[{"text":"search"}]}],
		"tools":[{"functionDeclarations":[{"name":"search","description":"do search","parameters":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}
	}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["*"]
	if opts == nil {
		t.Fatal("no ModelConfig")
	}
	if opts.Tools == nil || len(opts.Tools.Definitions) != 1 {
		t.Fatal("no tools")
	}
	ft := opts.Tools.Definitions[0].(*v1.FunctionTool)
	if ft.Name != "search" {
		t.Errorf("tool name: %s", ft.Name)
	}
	if opts.Tools.Choice == nil || opts.Tools.Choice.Mode != "required" {
		t.Errorf("choice: %+v", opts.Tools.Choice)
	}
}

func TestParseRequest_ToolResult(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"call search"}]},
		{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"foo"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"search","response":{"result":"bar"}}}]}
	]}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	// Expect: Message(user), FunctionCall, FunctionCallOutput
	if len(req.Input) != 3 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	fc, ok := req.Input[1].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("item[1] type: %T", req.Input[1])
	}
	if fc.Name != "search" {
		t.Errorf("fc name: %s", fc.Name)
	}
	fco, ok := req.Input[2].(*v1.FunctionCallOutput)
	if !ok {
		t.Fatalf("item[2] type: %T", req.Input[2])
	}
	if fco.CallID != "search" {
		t.Errorf("fco call_id: %s", fco.CallID)
	}
}

func TestParseRequest_Image(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"text":"describe"},
		{"inlineData":{"mimeType":"image/png","data":"abc123"}}
	]}]}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	msg := req.Input[0].(*v1.Message)
	if len(msg.Content) != 2 {
		t.Fatalf("parts: %d", len(msg.Content))
	}
	img, ok := msg.Content[1].(*v1.ImagePart)
	if !ok {
		t.Fatalf("part[1] type: %T", msg.Content[1])
	}
	if !strings.Contains(img.ImageURL, "image/png") {
		t.Errorf("imageURL: %s", img.ImageURL)
	}
}

// ---- SerializeRequest tests ----

func TestSerializeRequest_RoleMapping(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-1.5-pro"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hello"}}},
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Contents) != 2 {
		t.Fatalf("contents len: %d", len(wire.Contents))
	}
	if wire.Contents[0].Role != "user" {
		t.Errorf("contents[0].role: %s", wire.Contents[0].Role)
	}
	// assistant → model
	if wire.Contents[1].Role != "model" {
		t.Errorf("contents[1].role: %s", wire.Contents[1].Role)
	}
}

func TestSerializeRequest_SystemInstruction(t *testing.T) {
	req := &v1.Request{
		Model:        v1.ModelRefs{"gemini-1.5-pro"},
		Instructions: "Be concise.",
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.SystemInstruction == nil {
		t.Fatal("no systemInstruction")
	}
	if len(wire.SystemInstruction.Parts) == 0 || wire.SystemInstruction.Parts[0].Text != "Be concise." {
		t.Errorf("systemInstruction: %+v", wire.SystemInstruction)
	}
}

func TestSerializeRequest_ToolChoiceRequired_ANY(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-1.5-pro"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
		},
		ModelConfig: map[string]*v1.ModelOpts{
			"gemini-1.5-pro": {
				Tools: &v1.ToolsConfig{
					Definitions: v1.Tools{&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)}},
					Choice:      &v1.ToolChoice{Mode: "required"},
				},
			},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ToolConfig == nil || wire.ToolConfig.FunctionCallingConfig == nil {
		t.Fatal("no toolConfig")
	}
	if wire.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Errorf("mode: %s", wire.ToolConfig.FunctionCallingConfig.Mode)
	}
}

func TestSerializeRequest_Reasoning_ThinkingConfig(t *testing.T) {
	budget := 1024
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-2.0-flash-thinking"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "think"}}},
		},
		ModelConfig: map[string]*v1.ModelOpts{
			"gemini-2.0-flash-thinking": {
				Reasoning: &v1.ReasoningConfig{BudgetTokens: &budget},
			},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.GenerationConfig == nil || wire.GenerationConfig.ThinkingConfig == nil {
		t.Fatal("no thinkingConfig")
	}
	if wire.GenerationConfig.ThinkingConfig.ThinkingBudget != 1024 {
		t.Errorf("budget: %d", wire.GenerationConfig.ThinkingConfig.ThinkingBudget)
	}
	if !wire.GenerationConfig.ThinkingConfig.IncludeThoughts {
		t.Error("includeThoughts should be true")
	}
}

// ---- ParseResponse tests ----

func TestParseResponse_Text(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"Hello!"}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusCompleted {
		t.Errorf("status: %s", resp.Status)
	}
	if resp.FinishReason != v1.FinishReasonStop {
		t.Errorf("finishReason: %s", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] type: %T", resp.Output[0])
	}
	otp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] type: %T", msg.Content[0])
	}
	if otp.Text != "Hello!" {
		t.Errorf("text: %q", otp.Text)
	}
	if resp.Usage["input"] != 10 {
		t.Errorf("input tokens: %d", resp.Usage["input"])
	}
	if resp.Usage["output"] != 5 {
		t.Errorf("output tokens: %d", resp.Usage["output"])
	}
}

func TestParseResponse_FunctionCall_FinishReasonToolCalls(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"test"}}}]},"finishReason":"STOP","index":0}]
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finishReason: %s", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	fc, ok := resp.Output[0].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("output[0] type: %T", resp.Output[0])
	}
	if fc.Name != "search" {
		t.Errorf("name: %s", fc.Name)
	}
	// Arguments should be the stringified args object.
	if !strings.Contains(fc.Arguments, "test") {
		t.Errorf("arguments: %s", fc.Arguments)
	}
}

func TestParseResponse_MaxTokens_Incomplete(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"..."}]},"finishReason":"MAX_TOKENS","index":0}]
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete {
		t.Errorf("status: %s", resp.Status)
	}
	if resp.FinishReason != v1.FinishReasonLength {
		t.Errorf("finishReason: %s", resp.FinishReason)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_tokens" {
		t.Errorf("incompleteDetails: %+v", resp.IncompleteDetails)
	}
}

func TestParseResponse_Thought_Reasoning(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[
			{"text":"I think...","thought":true},
			{"text":"Answer."}
		]},"finishReason":"STOP","index":0}]
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] type: %T", resp.Output[0])
	}
	if r.Content != "I think..." {
		t.Errorf("reasoning content: %q", r.Content)
	}
	_, ok = resp.Output[1].(*v1.Message)
	if !ok {
		t.Fatalf("output[1] type: %T", resp.Output[1])
	}
}

func TestParseResponse_UsageMapping(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"cachedContentTokenCount":20,"thoughtsTokenCount":30}
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage["input"] != 100 {
		t.Errorf("input: %d", resp.Usage["input"])
	}
	if resp.Usage["output"] != 50 {
		t.Errorf("output: %d", resp.Usage["output"])
	}
	if resp.Usage["cache_read"] != 20 {
		t.Errorf("cache_read: %d", resp.Usage["cache_read"])
	}
	if resp.Usage["reasoning"] != 30 {
		t.Errorf("reasoning: %d", resp.Usage["reasoning"])
	}
}

// ---- SerializeResponse tests ----

func TestSerializeResponse_Text(t *testing.T) {
	resp := &v1.Response{
		ID:           "gemini-123",
		Model:        "gemini-1.5-pro",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}},
		},
	}
	body, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	if wire.Candidates[0].FinishReason != "STOP" {
		t.Errorf("finishReason: %s", wire.Candidates[0].FinishReason)
	}
	if len(wire.Candidates[0].Content.Parts) == 0 || wire.Candidates[0].Content.Parts[0].Text != "hi" {
		t.Errorf("text: %+v", wire.Candidates[0].Content.Parts)
	}
}

// ---- Stream Gemini→canonical tests ----

func makeGeminiSSEChunk(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return []byte("data: " + string(b) + "\n\n")
}

func TestStreamGeminiToCanonical_TextSequence(t *testing.T) {
	fn := tr.NewToCanonicalStream()

	chunk1 := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "Hello"}}},
			"index":   0,
		}},
		"modelVersion": "gemini-1.5-pro",
	})
	chunk2 := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": " world"}}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 2},
	})

	out1, err := fn(chunk1)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := fn(chunk2)
	if err != nil {
		t.Fatal(err)
	}

	combined := string(out1) + string(out2)
	if !strings.Contains(combined, v1.EventGenerationCreated) {
		t.Error("missing generation.created")
	}
	if !strings.Contains(combined, v1.EventItemStarted) {
		t.Error("missing item.started")
	}
	if !strings.Contains(combined, v1.EventItemDelta) {
		t.Error("missing item.delta")
	}
	if !strings.Contains(combined, v1.EventItemCompleted) {
		t.Error("missing item.completed")
	}
	if !strings.Contains(combined, v1.EventGenerationCompleted) {
		t.Error("missing generation.completed")
	}
}

func TestStreamGeminiToCanonical_FunctionCall(t *testing.T) {
	fn := tr.NewToCanonicalStream()

	chunk := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "NYC"}}},
			}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": "gemini-1.5-pro",
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "get_weather") {
		t.Errorf("function name missing from stream output: %s", s)
	}
	if !strings.Contains(s, v1.EventItemStarted) {
		t.Error("missing item.started")
	}
	if !strings.Contains(s, v1.EventGenerationCompleted) {
		t.Error("missing generation.completed")
	}
}

func TestStreamGeminiToCanonical_TerminalUsageAndFinishReason(t *testing.T) {
	fn := tr.NewToCanonicalStream()

	chunk := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}},
			"finishReason": "MAX_TOKENS",
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     50,
			"candidatesTokenCount": 100,
		},
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, v1.EventGenerationCompleted) {
		t.Error("missing generation.completed")
	}
	// Check that usage is present in the completed event.
	if !strings.Contains(s, `"input"`) {
		t.Error("usage input missing from generation.completed")
	}
}

// ---- Stream canonical→Gemini tests ----

func makeCanonSSEChunk(t *testing.T, event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

func TestStreamCanonicalToGemini_TextFlow(t *testing.T) {
	fn := tr.NewFromCanonicalStream()

	// generation.created
	out, err := fn(makeCanonSSEChunk(t, v1.EventGenerationCreated, v1.GenerationCreatedEvent{
		ID: "resp-1", Model: "gemini-1.5-pro",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Gemini has no open frame; should return nil.
	if out != nil {
		t.Errorf("expected nil for generation.created, got %q", out)
	}

	// item.started — should return nil
	out, err = fn(makeCanonSSEChunk(t, v1.EventItemStarted, v1.ItemStartedEvent{
		ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("expected nil for item.started, got %q", out)
	}

	// item.delta — should emit a Gemini SSE frame with text part
	out, err = fn(makeCanonSSEChunk(t, v1.EventItemDelta, v1.ItemDeltaEvent{
		ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Hello") {
		t.Errorf("text delta not in Gemini frame: %s", out)
	}

	// generation.completed — should emit terminal frame with finishReason
	out, err = fn(makeCanonSSEChunk(t, v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
		ID:           "resp-1",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "STOP") {
		t.Errorf("finishReason STOP missing from terminal frame: %s", out)
	}
}

// ---- regression tests for fidelity-audit fixes ----

// Parallel calls to the same function must get distinct CallIDs, and the bare
// function name must be recoverable for the functionResponse round-trip.
func TestParseResponse_ParallelSameFunction_UniqueCallIDs(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"search","args":{"q":"a"}}},
		{"functionCall":{"name":"search","args":{"q":"b"}}}
	]},"index":0}]}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range resp.Output {
		fc, ok := it.(*v1.FunctionCall)
		if !ok {
			t.Fatalf("item type %T", it)
		}
		ids = append(ids, fc.CallID)
		if fc.Name != "search" {
			t.Errorf("name: %s", fc.Name)
		}
		if got := geminiFuncNameFromCallID(fc.CallID); got != "search" {
			t.Errorf("name not recoverable from CallID %q: got %q", fc.CallID, got)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("CallIDs must be unique, got %v", ids)
	}
}

// A FunctionCallOutput whose CallID carries our synthesized suffix must
// serialize to a Gemini functionResponse with the BARE function name.
func TestSerializeRequest_FunctionResponse_RecoversBareName(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-1.5-pro"},
		Input: []v1.Item{
			&v1.FunctionCallOutput{CallID: geminiCallID("search", 1), Output: `{"result":"ok"}`},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), callIDSep) {
		t.Errorf("synthesized CallID suffix leaked to wire: %s", body)
	}
	if !strings.Contains(string(body), `"name":"search"`) {
		t.Errorf("functionResponse name not bare: %s", body)
	}
}

// Safety/policy finishReasons must surface as content_filter + incomplete,
// never as a normal stop.
func TestParseResponse_SafetyFinishReason_NotStop(t *testing.T) {
	for _, reason := range []string{"SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII"} {
		body := `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"` + reason + `","index":0}]}`
		resp, err := tr.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if resp.FinishReason != v1.FinishReasonContentFilter {
			t.Errorf("%s: finish_reason = %s, want content_filter", reason, resp.FinishReason)
		}
		if resp.Status != v1.StatusIncomplete {
			t.Errorf("%s: status = %s, want incomplete", reason, resp.Status)
		}
	}
}

// Structured-output (Output.Format) must populate Gemini responseSchema/MIME.
func TestSerializeRequest_OutputFormat_JSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-1.5-pro"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			"gemini-1.5-pro": {Output: &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: schema}}},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire geminiRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.GenerationConfig == nil || wire.GenerationConfig.ResponseMIMEType != "application/json" {
		t.Fatalf("responseMimeType not set: %s", body)
	}
	if wire.GenerationConfig.ResponseSchema == nil {
		t.Errorf("responseSchema not set: %s", body)
	}
}

// Streamed function-call arguments must accumulate into ONE functionCall frame
// whose args are a JSON object (not a %q-quoted string) emitted on completion.
func TestStreamCanonicalToGemini_FunctionArgsBuffered(t *testing.T) {
	fn := tr.NewFromCanonicalStream()
	_, _ = fn(makeCanonSSEChunk(t, v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "r", Model: "gemini-1.5-pro"}))
	if out, _ := fn(makeCanonSSEChunk(t, v1.EventItemStarted, v1.ItemStartedEvent{
		ItemID: "fc_0", ItemType: v1.ItemTypeFunctionCall, Index: 0, Name: "search",
	})); out != nil {
		t.Errorf("item.started should emit nothing, got %s", out)
	}
	// Arguments arrive in two partial deltas; neither should emit a frame.
	if out, _ := fn(makeCanonSSEChunk(t, v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Kind: v1.DeltaKindArguments, Delta: `{"q":`})); out != nil {
		t.Errorf("partial args delta should emit nothing, got %s", out)
	}
	if out, _ := fn(makeCanonSSEChunk(t, v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Kind: v1.DeltaKindArguments, Delta: `"hi"}`})); out != nil {
		t.Errorf("partial args delta should emit nothing, got %s", out)
	}
	out, err := fn(makeCanonSSEChunk(t, v1.EventItemCompleted, v1.ItemCompletedEvent{
		ItemID: "fc_0", Index: 0, Item: &v1.FunctionCall{CallID: "search__relay_call_0", Name: "search", Arguments: `{"q":"hi"}`},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Parse the emitted Gemini frame and verify args is an OBJECT, not a string.
	_, data, ok := v1.ParseSSEChunk(out)
	if !ok {
		t.Fatalf("no SSE frame emitted on completion: %q", out)
	}
	var frame geminiResponse
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("frame not valid JSON: %v (%s)", err, data)
	}
	if len(frame.Candidates) == 0 || frame.Candidates[0].Content == nil || len(frame.Candidates[0].Content.Parts) == 0 {
		t.Fatalf("no functionCall part: %s", data)
	}
	fc := frame.Candidates[0].Content.Parts[0].FunctionCall
	if fc == nil || fc.Name != "search" {
		t.Fatalf("functionCall name wrong: %s", data)
	}
	var args map[string]any
	if err := json.Unmarshal(fc.Args, &args); err != nil {
		t.Fatalf("args is not a JSON object (the %%q bug): %s", fc.Args)
	}
	if args["q"] != "hi" {
		t.Errorf("args content wrong: %v", args)
	}
}

// ---- thoughtSignature round-trip tests ----

// ParseResponse with a functionCall+thoughtSignature → ProviderData carries it.
func TestParseResponse_FunctionCall_ThoughtSignature(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"lookup","args":{"x":1}},"thoughtSignature":"sig-fc-abc"}
		]},"finishReason":"STOP","index":0}]
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	fc, ok := resp.Output[0].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("output[0] type: %T", resp.Output[0])
	}
	if got := thoughtSignatureFrom(fc.ProviderData); got != "sig-fc-abc" {
		t.Errorf("thoughtSignature in ProviderData: got %q, want %q", got, "sig-fc-abc")
	}
}

// ParseResponse with a thought+thoughtSignature → ProviderData carries it.
func TestParseResponse_Thought_ThoughtSignature(t *testing.T) {
	body := `{
		"candidates":[{"content":{"role":"model","parts":[
			{"text":"thinking...","thought":true,"thoughtSignature":"sig-rs-xyz"},
			{"text":"Answer."}
		]},"finishReason":"STOP","index":0}]
	}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] type: %T", resp.Output[0])
	}
	if got := thoughtSignatureFrom(r.ProviderData); got != "sig-rs-xyz" {
		t.Errorf("thoughtSignature in ProviderData: got %q, want %q", got, "sig-rs-xyz")
	}
}

// SerializeRequest with a FunctionCall carrying ProviderData → wire emits thoughtSignature.
func TestSerializeRequest_FunctionCall_ThoughtSignatureRoundTrip(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-2.0-flash-thinking"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
			&v1.FunctionCall{
				CallID:       geminiCallID("lookup", 0),
				Name:         "lookup",
				Arguments:    `{"x":1}`,
				ProviderData: thoughtSignatureJSON("sig-fc-abc"),
			},
			&v1.FunctionCallOutput{
				CallID: geminiCallID("lookup", 0),
				Output: `{"result":"ok"}`,
			},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"thoughtSignature":"sig-fc-abc"`) {
		t.Errorf("thoughtSignature not in wire body: %s", body)
	}
}

// SerializeRequest with a Reasoning carrying ProviderData → wire emits thought part with thoughtSignature.
func TestSerializeRequest_Reasoning_ThoughtSignatureRoundTrip(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-2.0-flash-thinking"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "think"}}},
			&v1.Reasoning{
				Content:      "some thoughts",
				ProviderData: thoughtSignatureJSON("sig-rs-xyz"),
			},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"thoughtSignature":"sig-rs-xyz"`) {
		t.Errorf("thoughtSignature not in wire body: %s", body)
	}
	if !strings.Contains(string(body), `"thought":true`) {
		t.Errorf("thought flag not in wire body: %s", body)
	}
}

// Streamed functionCall part with thoughtSignature → completed item ProviderData carries it.
func TestStreamGeminiToCanonical_FunctionCall_ThoughtSignature(t *testing.T) {
	fn := tr.NewToCanonicalStream()

	chunk := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{
					"functionCall":     map[string]any{"name": "lookup", "args": map[string]any{"x": 1}},
					"thoughtSignature": "sig-stream-fc",
				},
			}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": "gemini-2.0-flash",
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	// Find the item.completed event and decode the FunctionCall item.
	found := false
	for _, line := range strings.Split(string(out), "\n\n") {
		if !strings.Contains(line, v1.EventItemCompleted) {
			continue
		}
		for _, l := range strings.Split(line, "\n") {
			if !strings.HasPrefix(l, "data: ") {
				continue
			}
			// Item is an interface; decode into a raw struct to read provider_data.
			var ev struct {
				Item struct {
					ProviderData json.RawMessage `json:"provider_data"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(l[6:]), &ev); err != nil {
				t.Fatalf("unmarshal ItemCompletedEvent: %v", err)
			}
			if got := thoughtSignatureFrom(ev.Item.ProviderData); got != "sig-stream-fc" {
				t.Errorf("thoughtSignature: got %q, want %q", got, "sig-stream-fc")
			}
			found = true
		}
	}
	if !found {
		t.Errorf("item.completed not found in output: %s", out)
	}
}

// Streamed thought part with thoughtSignature → completed Reasoning ProviderData carries it.
func TestStreamGeminiToCanonical_Thought_ThoughtSignature(t *testing.T) {
	fn := tr.NewToCanonicalStream()

	chunk := makeGeminiSSEChunk(t, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{
					"text":             "reasoning...",
					"thought":          true,
					"thoughtSignature": "sig-stream-rs",
				},
			}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": "gemini-2.0-flash",
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n\n") {
		if !strings.Contains(line, v1.EventItemCompleted) {
			continue
		}
		for _, l := range strings.Split(line, "\n") {
			if !strings.HasPrefix(l, "data: ") {
				continue
			}
			var ev struct {
				Item struct {
					ProviderData json.RawMessage `json:"provider_data"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(l[6:]), &ev); err != nil {
				t.Fatalf("unmarshal ItemCompletedEvent: %v", err)
			}
			if got := thoughtSignatureFrom(ev.Item.ProviderData); got != "sig-stream-rs" {
				t.Errorf("thoughtSignature: got %q, want %q", got, "sig-stream-rs")
			}
			found = true
		}
	}
	if !found {
		t.Errorf("item.completed not found in output: %s", out)
	}
}

// ---- helpers ----

func jsonMust(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
