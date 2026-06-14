package openai

import (
	"encoding/json"
	"fmt"
)

// ResponsesFunctionTool is the only supported tool type in Responses API v1.
type ResponsesFunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema; kept raw to avoid re-encoding user schemas
	Strict      *bool           `json:"strict,omitempty"`
}

func (*ResponsesFunctionTool) isResponsesTool()                     {}
func (*ResponsesFunctionTool) ResponsesToolType() ResponsesToolType { return ResponsesToolTypeFunction }

func (f *ResponsesFunctionTool) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type        ResponsesToolType `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description,omitempty"`
		Parameters  json.RawMessage   `json:"parameters"`
		Strict      *bool             `json:"strict,omitempty"`
	}
	return json.Marshal(wire{
		Type:        ResponsesToolTypeFunction,
		Name:        f.Name,
		Description: f.Description,
		Parameters:  f.Parameters,
		Strict:      f.Strict,
	})
}

// ResponsesTools is a polymorphic slice of ResponsesTool values. Its UnmarshalJSON rejects
// unsupported tool types with an explicit error so callers can map to 400.
type ResponsesTools []ResponsesTool

func (ts ResponsesTools) MarshalJSON() ([]byte, error) {
	raws := make([]json.RawMessage, len(ts))
	for i, t := range ts {
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		raws[i] = b
	}
	return json.Marshal(raws)
}

func (ts *ResponsesTools) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("tools array: %w", err)
	}
	result := make(ResponsesTools, 0, len(raws))
	for _, raw := range raws {
		t, err := responsesUnmarshalTool(raw)
		if err != nil {
			return err
		}
		result = append(result, t)
	}
	*ts = result
	return nil
}

// ResponsesRawTool carries a tool definition whose `type` the adapter does not
// model — the hosted/server-side tools (web_search, file_search, code_interpreter,
// image_generation, computer_use, mcp, local_shell, custom). It exists so an
// unmodeled tool def round-trips verbatim instead of hard-erroring the whole
// request. Cross-shape it is dropped at responsesRequestToCanonical with an
// annotation (it can't be expressed to a non-OpenAI upstream); within-vendor is
// byte-pass and never reaches here.
type ResponsesRawTool struct {
	Type ResponsesToolType
	Raw  json.RawMessage
}

func (*ResponsesRawTool) isResponsesTool()                       {}
func (r *ResponsesRawTool) ResponsesToolType() ResponsesToolType { return r.Type }
func (r *ResponsesRawTool) MarshalJSON() ([]byte, error)         { return r.Raw, nil }

// responsesUnmarshalTool decodes a single tool. Function tools map to canonical;
// every other (hosted-tool) type is captured verbatim as a ResponsesRawTool.
func responsesUnmarshalTool(data []byte) (ResponsesTool, error) {
	var probe struct {
		Type ResponsesToolType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	switch probe.Type {
	case ResponsesToolTypeFunction:
		var v ResponsesFunctionTool
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function tool: %w", err)
		}
		return &v, nil
	default:
		return &ResponsesRawTool{Type: probe.Type, Raw: append(json.RawMessage(nil), data...)}, nil
	}
}

// ResponsesToolChoice is polymorphic: a string shorthand ("auto", "required", "none")
// or a {type:"function", name:"..."} object.
type ResponsesToolChoice struct {
	Mode         string // "auto" | "required" | "none" | "function"
	FunctionName string // only when Mode == "function"
}

func (tc ResponsesToolChoice) MarshalJSON() ([]byte, error) {
	switch tc.Mode {
	case "auto", "required", "none":
		return json.Marshal(tc.Mode)
	case "function":
		return json.Marshal(struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}{Type: "function", Name: tc.FunctionName})
	default:
		return json.Marshal(tc.Mode)
	}
}

func (tc *ResponsesToolChoice) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("tool_choice string: %w", err)
		}
		tc.Mode = s
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("tool_choice object: %w", err)
	}
	tc.Mode = obj.Type
	tc.FunctionName = obj.Name
	return nil
}
