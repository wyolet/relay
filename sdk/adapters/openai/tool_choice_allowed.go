package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// allowedEntry is one allowed_tools entry, flattened to its type and name.
type allowedEntry struct {
	Type string
	Name string
}

// allowedChoiceToCanonical maps an allowed_tools choice onto v1.ToolChoice.Allowed. Canonical names function tools only; any other entry is refused rather than dropped, since dropping it would change which tools the model may call.
func allowedChoiceToCanonical(mode string, entries []allowedEntry) (*v1.ToolChoice, error) {
	if mode != "auto" && mode != "required" {
		return nil, fmt.Errorf("tool_choice allowed_tools: mode %q, want auto or required", mode)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("tool_choice allowed_tools: tools is empty")
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		if e.Type != "function" {
			return nil, fmt.Errorf("tool_choice allowed_tools: tools[%d]: %q entries %w", i, e.Type, v1.ErrUnsupportedRequest)
		}
		if e.Name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools: tools[%d]: name is required", i)
		}
		names[i] = e.Name
	}
	return &v1.ToolChoice{Mode: mode, Allowed: names}, nil
}

// hasAllowedList reports whether c restricts the model to c.Allowed.
func hasAllowedList(c *v1.ToolChoice) bool {
	return len(c.Allowed) > 0 && (c.Mode == "auto" || c.Mode == "required")
}

// ccAllowedChoice is the Chat Completions allowed_tools choice; its entries nest the name under "function" like CC tool definitions.
type ccAllowedChoice struct {
	Type         string `json:"type"`
	AllowedTools struct {
		Mode  string         `json:"mode"`
		Tools []ccToolChoice `json:"tools"`
	} `json:"allowed_tools"`
}

func ccAllowedChoiceFromCanonical(c *v1.ToolChoice) (json.RawMessage, error) {
	w := ccAllowedChoice{Type: "allowed_tools"}
	w.AllowedTools.Mode = c.Mode
	w.AllowedTools.Tools = make([]ccToolChoice, len(c.Allowed))
	for i, name := range c.Allowed {
		w.AllowedTools.Tools[i].Type = "function"
		w.AllowedTools.Tools[i].Function.Name = name
	}
	return json.Marshal(w)
}

func ccAllowedChoiceToCanonical(w *ccAllowedChoice) (*v1.ToolChoice, error) {
	entries := make([]allowedEntry, len(w.AllowedTools.Tools))
	for i, t := range w.AllowedTools.Tools {
		entries[i] = allowedEntry{Type: t.Type, Name: t.Function.Name}
	}
	return allowedChoiceToCanonical(w.AllowedTools.Mode, entries)
}

// responsesAllowedChoice is the Responses allowed_tools choice; its entries are flat {type, name} objects.
type responsesAllowedChoice struct {
	Type  string                 `json:"type"`
	Mode  string                 `json:"mode"`
	Tools []responsesAllowedTool `json:"tools"`
}

type responsesAllowedTool struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// responsesAllowedChoiceToCanonical reads the allowed_tools object kept in raw. A custom entry names a tool lowered onto a canonical function tool, so it is allowed under that name.
func responsesAllowedChoiceToCanonical(raw json.RawMessage) (*v1.ToolChoice, error) {
	var w responsesAllowedChoice
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("tool_choice allowed_tools: %w", err)
	}
	entries := make([]allowedEntry, len(w.Tools))
	for i, t := range w.Tools {
		entries[i] = allowedEntry(t)
		if t.Type == string(ResponsesToolTypeCustom) {
			entries[i].Type = "function"
		}
	}
	return allowedChoiceToCanonical(w.Mode, entries)
}

// responsesAllowedChoiceFromCanonical builds the allowed_tools object; a name defined as a custom tool goes out as a custom entry, matching its definition.
func responsesAllowedChoiceFromCanonical(c *v1.ToolChoice, custom *responsesCustomLowering) (*ResponsesToolChoice, error) {
	w := responsesAllowedChoice{Type: "allowed_tools", Mode: c.Mode, Tools: make([]responsesAllowedTool, len(c.Allowed))}
	for i, name := range c.Allowed {
		w.Tools[i] = responsesAllowedTool{Type: "function", Name: name}
		if custom.isCustomName(name) {
			w.Tools[i].Type = string(ResponsesToolTypeCustom)
		}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	return &ResponsesToolChoice{Mode: "allowed_tools", Raw: raw}, nil
}
