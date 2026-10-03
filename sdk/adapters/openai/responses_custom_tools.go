package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Responses `custom` (freeform) and `namespace` (grouping) tool definitions, the custom_tool_call / custom_tool_call_output items they produce, and the lowering that carries them across canonical. A custom tool takes free text instead of JSON arguments, which no other vendor models, so canonical sees a function tool with one required string `input`; the original definition rides v1.FunctionTool.ProviderData for a byte-exact same-vendor round-trip.
//
// Deliberately out of scope: the hosted tools (web_search, mcp, …), which stay ResponsesRawTool and drop cross-shape.

// ResponsesCustomTool is a `custom` tool definition: free-form text input, optionally constrained by a grammar in Format.
type ResponsesCustomTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"`
	Raw         json.RawMessage `json:"-"` // verbatim definition, for same-vendor re-emit
}

func (*ResponsesCustomTool) isResponsesTool()                     {}
func (*ResponsesCustomTool) ResponsesToolType() ResponsesToolType { return ResponsesToolTypeCustom }

func (c *ResponsesCustomTool) MarshalJSON() ([]byte, error) {
	if len(c.Raw) > 0 {
		return c.Raw, nil
	}
	type wire struct {
		Type        ResponsesToolType `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description,omitempty"`
		Format      json.RawMessage   `json:"format,omitempty"`
	}
	return json.Marshal(wire{
		Type:        ResponsesToolTypeCustom,
		Name:        c.Name,
		Description: c.Description,
		Format:      c.Format,
	})
}

// ResponsesNamespaceTool groups function/custom tools under a shared name.
type ResponsesNamespaceTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Tools       ResponsesTools  `json:"tools,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

func (*ResponsesNamespaceTool) isResponsesTool() {}
func (*ResponsesNamespaceTool) ResponsesToolType() ResponsesToolType {
	return ResponsesToolTypeNamespace
}

func (n *ResponsesNamespaceTool) MarshalJSON() ([]byte, error) {
	if len(n.Raw) > 0 {
		return n.Raw, nil
	}
	type wire struct {
		Type        ResponsesToolType `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Tools       ResponsesTools    `json:"tools"`
	}
	return json.Marshal(wire{
		Type:        ResponsesToolTypeNamespace,
		Name:        n.Name,
		Description: n.Description,
		Tools:       n.Tools,
	})
}

// ResponsesCustomToolCall is an output item: the model's freeform call of a custom tool. Input replaces function_call's JSON `arguments`.
type ResponsesCustomToolCall struct {
	ID     string          `json:"id,omitempty"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Input  string          `json:"input"`
	Status ResponsesStatus `json:"status,omitempty"`
}

func (*ResponsesCustomToolCall) isResponsesItem() {}
func (*ResponsesCustomToolCall) ResponsesItemType() ResponsesItemType {
	return ResponsesItemTypeCustomToolCall
}

// status is output-only — see ResponsesMessage.MarshalJSON. Never emit it.
func (c *ResponsesCustomToolCall) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type   ResponsesItemType `json:"type"`
		ID     string            `json:"id,omitempty"`
		CallID string            `json:"call_id"`
		Name   string            `json:"name"`
		Input  string            `json:"input"`
	}
	return json.Marshal(wire{
		Type:   ResponsesItemTypeCustomToolCall,
		ID:     c.ID,
		CallID: c.CallID,
		Name:   c.Name,
		Input:  c.Input,
	})
}

// ResponsesCustomToolCallOutput is an input item delivering a custom tool's result.
type ResponsesCustomToolCallOutput struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func (*ResponsesCustomToolCallOutput) isResponsesItem() {}
func (*ResponsesCustomToolCallOutput) ResponsesItemType() ResponsesItemType {
	return ResponsesItemTypeCustomToolCallOutput
}

// UnmarshalJSON tolerates the content-list form of `output` by keeping its raw JSON as the string: dropping an unexpected shape here would silently lose a tool result (rule 11).
func (c *ResponsesCustomToolCallOutput) UnmarshalJSON(data []byte) error {
	var raw struct {
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.CallID = raw.CallID
	if len(raw.Output) > 0 && json.Unmarshal(raw.Output, &c.Output) != nil {
		c.Output = string(raw.Output)
	}
	return nil
}

func (c *ResponsesCustomToolCallOutput) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type   ResponsesItemType `json:"type"`
		CallID string            `json:"call_id"`
		Output string            `json:"output"`
	}
	return json.Marshal(wire{
		Type:   ResponsesItemTypeCustomToolCallOutput,
		CallID: c.CallID,
		Output: c.Output,
	})
}

// ResponsesCustomToolCallInputDeltaEvent carries an incremental freeform-input delta.
type ResponsesCustomToolCallInputDeltaEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
}

// ResponsesCustomToolCallInputDoneEvent carries the complete freeform input.
type ResponsesCustomToolCallInputDoneEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Input       string `json:"input"`
}

// responsesCustomInputArg is the single canonical argument a lowered custom tool takes; both directions agree on this key.
const responsesCustomInputArg = "input"

// responsesCustomToolToCanonical lowers a `custom` tool onto a canonical function tool taking one required string argument. A grammar-constrained format has no schema equivalent, so its syntax and definition go into the argument's description — the only place a non-OpenAI model will read them.
func responsesCustomToolToCanonical(t *ResponsesCustomTool) *v1.FunctionTool {
	var format struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	}
	if len(t.Format) > 0 {
		_ = json.Unmarshal(t.Format, &format)
	}
	desc := "Free-form text input for this tool."
	if format.Type == "grammar" && format.Definition != "" {
		desc = fmt.Sprintf("Free-form text input for this tool. It must match this %s grammar:\n%s",
			format.Syntax, format.Definition)
	}
	type property struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	params, err := json.Marshal(struct {
		Type       string              `json:"type"`
		Properties map[string]property `json:"properties"`
		Required   []string            `json:"required"`
	}{
		Type:       "object",
		Properties: map[string]property{responsesCustomInputArg: {Type: "string", Description: desc}},
		Required:   []string{responsesCustomInputArg},
	})
	if err != nil {
		return nil
	}
	raw := t.Raw
	if len(raw) == 0 {
		raw, _ = json.Marshal(t)
	}
	return &v1.FunctionTool{
		Name:         t.Name,
		Description:  t.Description,
		Parameters:   params,
		ProviderData: raw,
	}
}

// responsesCustomToolFromCanonical recovers the verbatim `custom` definition a function tool was lowered from, or nil when it was never a custom tool.
func responsesCustomToolFromCanonical(ft *v1.FunctionTool) ResponsesTool {
	if len(ft.ProviderData) == 0 {
		return nil
	}
	var probe struct {
		Type ResponsesToolType `json:"type"`
	}
	if json.Unmarshal(ft.ProviderData, &probe) != nil || probe.Type != ResponsesToolTypeCustom {
		return nil
	}
	return &ResponsesRawTool{Type: ResponsesToolTypeCustom, Raw: ft.ProviderData}
}

// responsesCustomCallMarker rides v1.FunctionCall.ProviderData so a custom_tool_call parsed out of a request's history re-emits as one rather than degrading to a function_call on the way back to the same vendor.
var responsesCustomCallMarker = json.RawMessage(`{"type":"custom_tool_call"}`)

// responsesCustomLowering decides which canonical function calls must go back out as custom_tool_call items. Canonical carries no such distinction, so the signals are the originating request's tool definitions (cross-vendor) and the same-vendor provider_data marker. Call ids are learned as calls are emitted, which is what lets the matching output item follow — a function_call_output carries neither name nor provider data.
type responsesCustomLowering struct {
	names   map[string]bool
	callIDs map[string]bool
}

// newResponsesCustomLowering collects the tool names lowered from `custom`.
func newResponsesCustomLowering(req *v1.Request) *responsesCustomLowering {
	c := &responsesCustomLowering{}
	if req == nil || req.Tools == nil {
		return c
	}
	for _, tool := range req.Tools.Definitions {
		ft, ok := tool.(*v1.FunctionTool)
		if !ok || responsesCustomToolFromCanonical(ft) == nil {
			continue
		}
		if c.names == nil {
			c.names = map[string]bool{}
		}
		c.names[ft.Name] = true
	}
	return c
}

// isCustomName reports whether a tool of this name was defined as custom.
func (c *responsesCustomLowering) isCustomName(name string) bool {
	return c != nil && c.names[name]
}

// noteCall records a call id as belonging to a custom tool and reports whether the call itself is one.
func (c *responsesCustomLowering) noteCall(fc *v1.FunctionCall) bool {
	if c == nil {
		return false
	}
	custom := c.names[fc.Name] || string(fc.ProviderData) == string(responsesCustomCallMarker)
	if custom && fc.CallID != "" {
		if c.callIDs == nil {
			c.callIDs = map[string]bool{}
		}
		c.callIDs[fc.CallID] = true
	}
	return custom
}

// isCustomOutput reports whether a call id was produced by a custom call.
func (c *responsesCustomLowering) isCustomOutput(callID string) bool {
	return c != nil && c.callIDs[callID]
}

// responsesCustomToolInput pulls the freeform text back out of the lowered argument object.
//
// canonical: a model that ignored the lowered schema (non-JSON arguments, or no `input` key) has its arguments passed through verbatim as the freeform input — the text IS the payload here, so dropping it would lose the tool call, and there is nothing to validate it against.
func responsesCustomToolInput(arguments string) string {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return arguments
	}
	raw, ok := args[responsesCustomInputArg]
	if !ok {
		return arguments
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return arguments
	}
	return s
}
