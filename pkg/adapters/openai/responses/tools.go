package responses

import (
	"encoding/json"
	"fmt"
)

// FunctionTool is the only supported tool type in v1.
type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema; kept raw to avoid re-encoding user schemas
	Strict      *bool           `json:"strict,omitempty"`
}

func (*FunctionTool) isTool()          {}
func (*FunctionTool) ToolType() ToolType { return ToolTypeFunction }

func (f *FunctionTool) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type        ToolType        `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict,omitempty"`
	}
	return json.Marshal(wire{
		Type:        ToolTypeFunction,
		Name:        f.Name,
		Description: f.Description,
		Parameters:  f.Parameters,
		Strict:      f.Strict,
	})
}

// Tools is a polymorphic slice of Tool values. Its UnmarshalJSON rejects
// unsupported tool types with an explicit error so callers can map to 400.
type Tools []Tool

func (ts Tools) MarshalJSON() ([]byte, error) {
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

func (ts *Tools) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("tools array: %w", err)
	}
	result := make(Tools, 0, len(raws))
	for _, raw := range raws {
		t, err := unmarshalTool(raw)
		if err != nil {
			return err
		}
		result = append(result, t)
	}
	*ts = result
	return nil
}

// unmarshalTool decodes a single tool. Returns an explicit error for types
// outside the v1 supported set.
func unmarshalTool(data []byte) (Tool, error) {
	var probe struct {
		Type ToolType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	switch probe.Type {
	case ToolTypeFunction:
		var v FunctionTool
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function tool: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q; v1 supports only function tools", probe.Type)
	}
}

// ToolChoice is polymorphic: a string shorthand ("auto", "required", "none")
// or a {type:"function", name:"..."} object.
type ToolChoice struct {
	Mode         string // "auto" | "required" | "none" | "function"
	FunctionName string // only when Mode == "function"
}

func (tc ToolChoice) MarshalJSON() ([]byte, error) {
	switch tc.Mode {
	case "auto", "required", "none":
		return json.Marshal(tc.Mode)
	case "function":
		return json.Marshal(struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}{Type: "function", Name: tc.FunctionName})
	default:
		// Fall back to marshaling the mode as a string even if unexpected.
		return json.Marshal(tc.Mode)
	}
}

func (tc *ToolChoice) UnmarshalJSON(data []byte) error {
	// String form: "auto" | "required" | "none"
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("tool_choice string: %w", err)
		}
		tc.Mode = s
		return nil
	}
	// Object form: {type:"function", name:"..."}
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
