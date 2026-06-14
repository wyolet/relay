package v1

import (
	"encoding/json"
	"fmt"
)

// ToolsConfig is the task-level tool spec carried on Request.Tools. It groups
// the tool definitions with their selection knobs (choice/parallel). One spec
// applies to every model in a multiplex request — tools are a property of the
// task, not the model, and each upstream adapter renders this onto its own wire
// shape.
type ToolsConfig struct {
	Definitions Tools       `json:"definitions,omitempty"`
	Choice      *ToolChoice `json:"choice,omitempty"`
	Parallel    *bool       `json:"parallel,omitempty"`
}

// FunctionTool is a caller-executed tool. The caller's code receives the
// tool_call item and submits a tool_result in the next request.
type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema; kept raw to avoid re-encoding user schemas
	Strict      *bool           `json:"strict,omitempty"`
}

func (*FunctionTool) isTool()            {}
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

// ServerTool is a relay-executed tool (v2). Wire-modeled in v1; runtime
// rejects it with tool_kind_not_implemented so future support is additive.
type ServerTool struct {
	Name string `json:"name"`
}

func (*ServerTool) isTool()            {}
func (*ServerTool) ToolType() ToolType { return ToolTypeServer }

func (s *ServerTool) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type ToolType `json:"type"`
		Name string   `json:"name"`
	}
	return json.Marshal(wire{Type: ToolTypeServer, Name: s.Name})
}

// MCPTool is a relay-proxied MCP tool (v2). Wire-modeled in v1; runtime
// rejects it with tool_kind_not_implemented so future support is additive.
// ServerURL and Headers are for per-request ad-hoc MCP servers; Name alone
// references an operator-configured MCP server from the catalog.
type MCPTool struct {
	Name      string            `json:"name,omitempty"`
	ServerURL string            `json:"server_url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

func (*MCPTool) isTool()            {}
func (*MCPTool) ToolType() ToolType { return ToolTypeMCP }

func (m *MCPTool) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      ToolType          `json:"type"`
		Name      string            `json:"name,omitempty"`
		ServerURL string            `json:"server_url,omitempty"`
		Headers   map[string]string `json:"headers,omitempty"`
	}
	return json.Marshal(wire{
		Type:      ToolTypeMCP,
		Name:      m.Name,
		ServerURL: m.ServerURL,
		Headers:   m.Headers,
	})
}

// Tools is a polymorphic slice of Tool values.
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

// unmarshalTool decodes a single tool.
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
	case ToolTypeServer:
		var v ServerTool
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("server tool: %w", err)
		}
		return &v, nil
	case ToolTypeMCP:
		var v MCPTool
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("mcp tool: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q", probe.Type)
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
		return json.Marshal(tc.Mode)
	}
}

func (tc *ToolChoice) UnmarshalJSON(data []byte) error {
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
