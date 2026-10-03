package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// applyPatchTool is the `custom` (freeform) tool shape Codex ships apply_patch
// as, with a grammar constraining the patch envelope.
const applyPatchTool = `{"type":"custom","name":"apply_patch","description":"Edit files.","format":{"type":"grammar","syntax":"lark","definition":"start: \"*** Begin Patch\" /.*/s \"*** End Patch\""}}`

func TestResponsesCustomToolLowersToFunctionTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":"hi","tools":[` + applyPatchTool + `]}`)

	canon, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if canon.Tools == nil || len(canon.Tools.Definitions) != 1 {
		t.Fatalf("canonical tools = %+v, want one", canon.Tools)
	}
	ft, ok := canon.Tools.Definitions[0].(*v1.FunctionTool)
	if !ok {
		t.Fatalf("canonical tool type = %T, want *v1.FunctionTool", canon.Tools.Definitions[0])
	}
	if ft.Name != "apply_patch" {
		t.Errorf("name = %q, want apply_patch", ft.Name)
	}

	var params struct {
		Type       string `json:"type"`
		Properties struct {
			Input struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"input"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(ft.Parameters, &params); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	if params.Type != "object" || params.Properties.Input.Type != "string" ||
		len(params.Required) != 1 || params.Required[0] != "input" {
		t.Errorf("parameters = %s, want an object with one required string input", ft.Parameters)
	}
	if !strings.Contains(params.Properties.Input.Description, "*** Begin Patch") ||
		!strings.Contains(params.Properties.Input.Description, "lark") {
		t.Errorf("input description = %q, want the grammar syntax + definition", params.Properties.Input.Description)
	}

	// Same vendor: the original definition comes back byte-equal.
	out, err := (ResponsesTranslator{}).SerializeRequest(canon)
	if err != nil {
		t.Fatalf("SerializeRequest: %v", err)
	}
	var wire struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("serialized body: %v", err)
	}
	if len(wire.Tools) != 1 || string(wire.Tools[0]) != applyPatchTool {
		t.Errorf("serialized tool = %s, want the original custom definition verbatim", wire.Tools)
	}

	// Without the provider blob (the cross-vendor view) the vendor-neutral
	// function schema is what a non-OpenAI upstream is asked to satisfy.
	ft.ProviderData = nil
	neutral, err := (ResponsesTranslator{}).SerializeRequest(canon)
	if err != nil {
		t.Fatalf("SerializeRequest: %v", err)
	}
	if err := json.Unmarshal(neutral, &wire); err != nil {
		t.Fatalf("serialized body: %v", err)
	}
	var neutralTool struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(wire.Tools[0], &neutralTool); err != nil {
		t.Fatalf("neutral tool: %v", err)
	}
	if neutralTool.Type != "function" || neutralTool.Name != "apply_patch" {
		t.Errorf("neutral tool = %s, want a function tool named apply_patch", wire.Tools[0])
	}
	if string(neutralTool.Parameters) != string(ft.Parameters) {
		t.Errorf("neutral parameters = %s, want the lowered schema", neutralTool.Parameters)
	}
}

func TestResponsesCustomToolCallFromCanonical(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-5"},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			responsesCustomToolToCanonical(mustCustomTool(t, applyPatchTool)),
		}},
	}
	resp := &v1.Response{
		ID:     "resp_1",
		Model:  "gpt-5",
		Status: v1.StatusCompleted,
		Output: []v1.Item{&v1.FunctionCall{
			ID:        "ctc_1",
			CallID:    "call_1",
			Name:      "apply_patch",
			Arguments: `{"input":"*** Begin Patch\n*** End Patch"}`,
			Status:    v1.StatusCompleted,
		}},
	}

	out, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatalf("SerializeResponse: %v", err)
	}
	var wire struct {
		Output []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input string `json:"input"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("serialized response: %v", err)
	}
	if len(wire.Output) != 1 || wire.Output[0].Type != "custom_tool_call" {
		t.Fatalf("output = %s, want one custom_tool_call", out)
	}
	if wire.Output[0].Name != "apply_patch" || wire.Output[0].Input != "*** Begin Patch\n*** End Patch" {
		t.Errorf("custom_tool_call = %+v, want the freeform patch as input", wire.Output[0])
	}

	// A model that ignored the lowered schema still gets its text through.
	resp.Output[0].(*v1.FunctionCall).Arguments = "*** Begin Patch"
	out, err = (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatalf("SerializeResponse: %v", err)
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("serialized response: %v", err)
	}
	if wire.Output[0].Input != "*** Begin Patch" {
		t.Errorf("input = %q, want the raw arguments passed through", wire.Output[0].Input)
	}
}

func TestResponsesCustomToolCallStreamFromCanonical(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-5"},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			responsesCustomToolToCanonical(mustCustomTool(t, applyPatchTool)),
		}},
	}
	fn := (ResponsesTranslator{}).NewFromCanonicalStreamFor(req)

	chunks := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_1", Model: "gpt-5"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{
			ItemID: "ctc_1", ItemType: v1.ItemTypeFunctionCall, Name: "apply_patch", Index: 0,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "ctc_1", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"input":"*** Begin`,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "ctc_1", Index: 0, Kind: v1.DeltaKindArguments, Delta: ` Patch"}`,
		}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{
			ItemID: "ctc_1",
			Index:  0,
			Item: &v1.FunctionCall{
				ID: "ctc_1", CallID: "call_1", Name: "apply_patch",
				Arguments: `{"input":"*** Begin Patch"}`, Status: v1.StatusCompleted,
			},
		}),
	}

	var events []string
	var lastData []byte
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		for _, frame := range splitCanonicalFrames(out) {
			event, data, ok := ParseResponsesSSEChunk(frame)
			if !ok {
				continue
			}
			events = append(events, event)
			lastData = data
		}
	}

	want := []string{
		ResponsesEventCreated,
		ResponsesEventInProgress,
		ResponsesEventOutputItemAdded,
		ResponsesEventCustomToolCallInputDelta,
		ResponsesEventCustomToolCallInputDone,
		ResponsesEventOutputItemDone,
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}

	var done struct {
		Item struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input string `json:"input"`
		} `json:"item"`
	}
	if err := json.Unmarshal(lastData, &done); err != nil {
		t.Fatalf("output_item.done: %v", err)
	}
	if done.Item.Type != "custom_tool_call" || done.Item.Input != "*** Begin Patch" {
		t.Errorf("final item = %+v, want a custom_tool_call carrying the patch", done.Item)
	}
}

func TestResponsesCustomToolCallOutputToCanonical(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"Done"}
	]}`)

	canon, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(canon.Input) != 2 {
		t.Fatalf("input items = %d, want 2", len(canon.Input))
	}
	call, ok := canon.Input[0].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("item 0 = %T, want *v1.FunctionCall", canon.Input[0])
	}
	if call.Arguments != `{"input":"*** Begin Patch"}` {
		t.Errorf("arguments = %s, want the input lowered into the argument object", call.Arguments)
	}
	out, ok := canon.Input[1].(*v1.FunctionCallOutput)
	if !ok {
		t.Fatalf("item 1 = %T, want *v1.FunctionCallOutput", canon.Input[1])
	}
	if out.CallID != "call_1" || out.Output != "Done" {
		t.Errorf("tool result = %+v, want call_1/Done", out)
	}

	// Same vendor: both items go back out in their custom form, the output
	// following its call by call id.
	wire, err := (ResponsesTranslator{}).SerializeRequest(canon)
	if err != nil {
		t.Fatalf("SerializeRequest: %v", err)
	}
	var body2 struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(wire, &body2); err != nil {
		t.Fatalf("serialized body: %v", err)
	}
	if len(body2.Input) != 2 || body2.Input[0].Type != "custom_tool_call" ||
		body2.Input[1].Type != "custom_tool_call_output" {
		t.Errorf("input items = %+v, want the custom pair", body2.Input)
	}
}

func TestResponsesNamespaceToolFlattens(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"namespace","name":"multi_agent_v1","description":"","tools":[
		{"type":"function","name":"create_goal","description":"c","parameters":{"type":"object"}},
		{"type":"function","name":"update_goal","description":"u","parameters":{"type":"object"}}
	]}]}`)

	canon, err := (ResponsesTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if canon.Tools == nil || len(canon.Tools.Definitions) != 2 {
		t.Fatalf("canonical tools = %+v, want the two inner tools", canon.Tools)
	}
	for i, want := range []string{"create_goal", "update_goal"} {
		ft, ok := canon.Tools.Definitions[i].(*v1.FunctionTool)
		if !ok || ft.Name != want {
			t.Errorf("tool %d = %+v, want a function tool named %s", i, canon.Tools.Definitions[i], want)
		}
	}
}

func mustCustomTool(t *testing.T, raw string) *ResponsesCustomTool {
	t.Helper()
	tool, err := responsesUnmarshalTool([]byte(raw))
	if err != nil {
		t.Fatalf("parse custom tool: %v", err)
	}
	ct, ok := tool.(*ResponsesCustomTool)
	if !ok {
		t.Fatalf("tool type = %T, want *ResponsesCustomTool", tool)
	}
	return ct
}
