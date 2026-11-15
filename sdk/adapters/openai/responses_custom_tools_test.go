package openai

import (
	"encoding/json"
	"slices"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// applyPatchTool is the `custom` (freeform) tool shape Codex ships apply_patch as, with a grammar constraining the patch envelope.
const applyPatchTool = `{"name":"apply_patch","type":"custom","description":"Edit files.","format":{"type":"grammar","syntax":"lark","definition":"start: \"*** Begin Patch\" /.*/s \"*** End Patch\""}}`

// applyPatchParams is the lowered schema a non-OpenAI model is asked to satisfy: one required string, the grammar in its description.
const applyPatchParams = `{"type":"object","properties":{"input":{"type":"string","description":"Free-form text input for this tool. It must match this lark grammar:\nstart: \"*** Begin Patch\" /.*/s \"*** End Patch\""}},"required":["input"]}`

func respCustomToolRequest(t *testing.T) *v1.Request {
	t.Helper()
	return respParseRequest(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"f","parameters":{}},`+applyPatchTool+`,{"type":"custom","name":"note"}]}`)
}

func TestResponsesCustomToolDefinition(t *testing.T) {
	req := respCustomToolRequest(t)
	// A custom tool without a grammar still lowers to a string input.
	const noteTool = `{"type":"function","name":"note","parameters":{"type":"object","properties":{"input":{"type":"string","description":"Free-form text input for this tool."}},"required":["input"]},"provider_data":{"type":"custom","name":"note"}}`
	respJSONEq(t, req.Tools, `{"definitions":[{"type":"function","name":"f","parameters":{}},
		{"type":"function","name":"apply_patch","description":"Edit files.","parameters":`+applyPatchParams+`,"provider_data":`+applyPatchTool+`},`+noteTool+`]}`)

	// Same vendor: the original definitions go back out byte for byte.
	if got, want := string(respMember(t, respSerializeRequest(t, req), "tools")), `[{"type":"function","name":"f","parameters":{}},`+applyPatchTool+`,{"type":"custom","name":"note"}]`; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}

	// Without the provider blob a non-OpenAI view of the tool is a plain function tool.
	req.Tools.Definitions[1].(*v1.FunctionTool).ProviderData = nil
	respJSONEq(t, respMember(t, respSerializeRequest(t, req), "tools"), `[{"type":"function","name":"f","parameters":{}},
		{"type":"function","name":"apply_patch","description":"Edit files.","parameters":`+applyPatchParams+`},{"type":"custom","name":"note"}]`)
}

// Only a grammar format that has a definition reaches the lowered argument's description.
func TestResponsesCustomToolDefinition_FormatWithoutGrammar(t *testing.T) {
	for _, format := range []string{`{"type":"grammar","syntax":"lark"}`, `{"type":"text","definition":"x"}`, `{"definition":"x"}`} {
		req := respParseRequest(t, `{"model":"m","input":"hi","tools":[{"type":"custom","name":"note","format":`+format+`}]}`)
		respJSONEq(t, req.Tools.Definitions[0].(*v1.FunctionTool).Parameters, `{"type":"object","properties":{"input":{"type":"string","description":"Free-form text input for this tool."}},"required":["input"]}`)
	}
}

// A custom call and its result parse to an ordinary function call pair and go back to the same vendor in custom form, the result following its call by call id.
func TestResponsesCustomToolCallInput(t *testing.T) {
	req := respParseRequest(t, `{"model":"gpt-5","input":[
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch","status":"completed"},
		{"type":"custom_tool_call","id":"x_2","call_id":"call_2","name":"apply_patch","input":"again"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"Done"},
		{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"ok"}]}]}`)
	respJSONEq(t, req.Input, `[
		{"type":"function_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}","status":"completed","provider_data":{"type":"custom_tool_call"}},
		{"type":"function_call","id":"x_2","call_id":"call_2","name":"apply_patch","arguments":"{\"input\":\"again\"}","provider_data":{"type":"custom_tool_call"}},
		{"type":"function_call_output","call_id":"call_1","output":"Done"},
		{"type":"function_call_output","call_id":"call_2","output":"[{\"type\":\"input_text\",\"text\":\"ok\"}]"}]`)
	respJSONEq(t, respMember(t, respSerializeRequest(t, req), "input"), `[
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch"},
		{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"again"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"Done"},
		{"type":"custom_tool_call_output","call_id":"call_2","output":"[{\"type\":\"input_text\",\"text\":\"ok\"}]"}]`)
}

func TestResponsesCustomToolCallResponse(t *testing.T) {
	req := respCustomToolRequest(t)
	for name, tc := range map[string]struct{ args, input string }{
		"lowered arguments": {`{"input":"*** Begin Patch\n*** End Patch"}`, "*** Begin Patch\n*** End Patch"},
		// A model that ignored the lowered schema still gets its text through.
		"raw text":         {"*** Begin Patch", "*** Begin Patch"},
		"no input key":     {`{"patch":"x"}`, `{"patch":"x"}`},
		"non-string input": {`{"input":5}`, `{"input":5}`},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &v1.Response{ID: "r", Model: "gpt-5", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonToolCalls, Output: []v1.Item{
				&v1.FunctionCall{ID: "ctc_1", CallID: "call_1", Name: "apply_patch", Arguments: tc.args, Status: v1.StatusCompleted},
			}}
			wire, err := ResponsesTranslator{}.SerializeResponse(resp, req)
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(tc.input)
			respJSONEq(t, respMember(t, wire, "output"), `[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":`+string(input)+`,"status":"completed"}]`)
		})
	}
}

func TestResponsesCustomToolCallStreamFromCanonical(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewFromCanonicalStreamFor(respCustomToolRequest(t)),
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_1", Model: "gpt-5"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "ctc_1", ItemType: v1.ItemTypeFunctionCall, Name: "apply_patch", CallID: "call_1"}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "ctc_1", Kind: v1.DeltaKindArguments, Delta: `{"input":"*** Begin`}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "ctc_1", Kind: v1.DeltaKindArguments, Delta: ` Patch"}`}),
		canonicalChunk(v1.EventItemCompleted, map[string]any{"item_id": "ctc_1", "item": map[string]any{"type": "function_call", "status": "completed"}}),
	)
	want := []string{ResponsesEventCreated, ResponsesEventInProgress, ResponsesEventOutputItemAdded, ResponsesEventCustomToolCallInputDelta, ResponsesEventCustomToolCallInputDone, ResponsesEventOutputItemDone}
	if got := respEvents(frames); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	respJSONEq(t, frames[2].Data, `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"","status":"in_progress"}}`)
	respJSONEq(t, frames[3].Data, `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"item_id":"ctc_1","output_index":0,"delta":"*** Begin Patch"}`)
	respJSONEq(t, frames[4].Data, `{"type":"response.custom_tool_call_input.done","sequence_number":4,"item_id":"ctc_1","output_index":0,"input":"*** Begin Patch"}`)
	respJSONEq(t, frames[5].Data, `{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch","status":"completed"}}`)
}

func respCustomCallStream() [][]byte {
	return [][]byte{
		respSSE(ResponsesEventOutputItemAdded, `{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":""}}`),
		respSSE(ResponsesEventCustomToolCallInputDelta, `{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"*** Begin Patch"}`),
		respSSE(ResponsesEventCustomToolCallInputDone, `{"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":"*** Begin Patch"}`),
		respSSE(ResponsesEventOutputItemDone, `{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch","status":"completed"}}`),
	}
}

func TestResponsesCustomToolCallStreamToCanonical(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(), respCustomCallStream()...)
	respJSONEq(t, frames, `[
		{"event":"item.started","data":{"item_id":"ctc_1","item_type":"function_call","index":0,"name":"apply_patch","call_id":"call_1"}},
		{"event":"item.delta","data":{"item_id":"ctc_1","index":0,"kind":"arguments","delta":"{\"input\":\"*** Begin Patch"}},
		{"event":"item.delta","data":{"item_id":"ctc_1","index":0,"kind":"arguments","delta":"\"}"}},
		{"event":"item.completed","data":{"item_id":"ctc_1","index":0,"item":{"type":"function_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}","status":"completed","provider_data":{"type":"custom_tool_call"}}}}]`)
}

// Deltas are escaped as JSON string bodies, and a done with no preceding delta carries the whole input.
func TestResponsesCustomToolCallStreamToCanonical_ArgumentDeltaEdges(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(),
		respSSE(ResponsesEventCustomToolCallInputDelta, `{"item_id":"a","output_index":0,"delta":"say \"<hi>\"\n"}`),
		respSSE(ResponsesEventCustomToolCallInputDone, `{"item_id":"a","output_index":0,"input":"say \"<hi>\"\n"}`),
		respSSE(ResponsesEventCustomToolCallInputDone, `{"item_id":"b","output_index":1,"input":""}`),
	)
	respJSONEq(t, frames, `[
		{"event":"item.delta","data":{"item_id":"a","index":0,"kind":"arguments","delta":"{\"input\":\"say \\\"\\u003chi\\u003e\\\"\\n"}},
		{"event":"item.delta","data":{"item_id":"a","index":0,"kind":"arguments","delta":"\"}"}},
		{"event":"item.delta","data":{"item_id":"b","index":1,"kind":"arguments","delta":"{\"input\":\"\"}"}}]`)
}

// A consumer that builds tool calls from argument deltas (every vendor stream serializer) must see the arguments the completed item carries.
func TestResponsesCustomToolCallStreamToCanonical_ArgumentDeltas(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(), respCustomCallStream()...)
	var args string
	for _, f := range frames {
		var d v1.ItemDeltaEvent
		if f.Event == v1.EventItemDelta && json.Unmarshal(f.Data, &d) == nil && d.Kind == v1.DeltaKindArguments {
			args += d.Delta
		}
	}
	if args != `{"input":"*** Begin Patch"}` {
		t.Errorf("argument deltas concatenate to %q, want the completed item's arguments", args)
	}
}
