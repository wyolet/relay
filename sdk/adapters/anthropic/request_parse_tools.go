package anthropic

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// anthropicParseTool decodes one raw tool JSON into a canonical v1.Tool.
// Anthropic server tools (web_search_20250305 etc.) are mapped to ServerTool.
func anthropicParseTool(raw json.RawMessage) (v1.Tool, error) {
	var probe struct {
		Name        string          `json:"name"`
		Type        string          `json:"type"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	// Anthropic server tools have type != "" (e.g. "web_search_20250305").
	if probe.Type != "" && probe.Type != "function" {
		return &v1.ServerTool{Name: probe.Name}, nil
	}
	schema := probe.InputSchema
	if schema == nil {
		schema = json.RawMessage(`{}`)
	}
	// canonical: eager_input_streaming dropped — eager arg delivery is the
	// canonical default (other vendors never buffer), and serialize-side
	// re-adds it on every streaming anthropic request, so the intent survives
	// both cross-vendor and anthropic→anthropic round trips.
	return &v1.FunctionTool{
		Name:        probe.Name,
		Description: probe.Description,
		Parameters:  schema,
	}, nil
}

// anthropicParseToolChoice decodes Anthropic tool_choice JSON into canonical *v1.ToolChoice.
func anthropicParseToolChoice(raw json.RawMessage) *v1.ToolChoice {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return &v1.ToolChoice{Mode: "auto"}
	case "any":
		return &v1.ToolChoice{Mode: "required"}
	case "none":
		return &v1.ToolChoice{Mode: "none"}
	case "tool":
		return &v1.ToolChoice{Mode: "function", FunctionName: tc.Name}
	default:
		return &v1.ToolChoice{Mode: tc.Type}
	}
}
