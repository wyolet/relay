package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/wyolet/relay/pkg/adapters/openai"
)

// ToOpenAI converts a MessagesRequest into an OpenAI FullChatRequest.
// Fields with no OpenAI equivalent are silently dropped.
func ToOpenAI(r *MessagesRequest) (*openai.FullChatRequest, error) {
	out := &openai.FullChatRequest{
		Model: r.Model,
	}

	// Prepend a system message if system is set.
	if len(r.System) > 0 {
		sys, err := extractSystemText(r.System)
		if err == nil && sys != "" {
			txt, _ := json.Marshal(sys)
			out.Messages = append(out.Messages, openai.ChatMessage{
				Role:    "system",
				Content: txt,
			})
		}
	}

	for _, raw := range r.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		out.Messages = append(out.Messages, openai.ChatMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	if r.MaxTokens > 0 {
		v := r.MaxTokens
		out.MaxTokens = &v
	}
	out.Temperature = r.Temperature
	out.TopP = r.TopP

	if len(r.StopSequences) > 0 {
		b, _ := json.Marshal(r.StopSequences)
		out.Stop = b
	}

	if r.Stream {
		t := true
		out.Stream = &t
	}

	// Convert Anthropic tools → OpenAI tools.
	for _, t := range r.Tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, openai.Tool{
			Type: "function",
			Function: openai.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	if len(r.ToolChoice) > 0 {
		out.ToolChoice = anthropicToolChoiceToOpenAI(r.ToolChoice)
	}

	return out, nil
}

// FromOpenAI converts an OpenAI FullChatRequest into a MessagesRequest.
// Unsupported params are silently dropped per litellm drop_params=True.
func FromOpenAI(r *openai.FullChatRequest) (*MessagesRequest, error) {
	out := &MessagesRequest{
		Model:       r.Model,
		Temperature: r.Temperature,
		TopP:        r.TopP,
	}

	if r.MaxTokens != nil {
		out.MaxTokens = *r.MaxTokens
	} else if r.MaxCompletion != nil {
		// max_completion_tokens → max_tokens
		out.MaxTokens = *r.MaxCompletion
	}

	if r.Stream != nil && *r.Stream {
		out.Stream = true
	}

	// Extract stop sequences.
	if len(r.Stop) > 0 {
		out.StopSequences = parseStopSequences(r.Stop)
	}

	// Separate system messages from conversation messages.
	var systemParts []string
	for _, m := range r.Messages {
		if m.Role == "system" {
			s := extractStringContent(m.Content)
			if s != "" {
				systemParts = append(systemParts, s)
			}
			continue
		}
		b, _ := json.Marshal(map[string]json.RawMessage{
			"role":    mustMarshal(m.Role),
			"content": m.Content,
		})
		out.Messages = append(out.Messages, b)
	}
	if len(systemParts) > 0 {
		joined := strings.Join(systemParts, "\n")
		out.System, _ = json.Marshal(joined)
	}

	// Convert OpenAI tools → Anthropic tools.
	for _, t := range r.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}

	if len(r.ToolChoice) > 0 {
		out.ToolChoice = openaiToolChoiceToAnthropic(r.ToolChoice)
	}

	return out, nil
}

// extractSystemText handles system being either a plain string or an array of
// {type:"text", text:"..."} blocks (Anthropic extended format).
func extractSystemText(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func extractStringContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "")
}

func parseStopSequences(raw json.RawMessage) []string {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}
	}
	var list []string
	_ = json.Unmarshal(raw, &list)
	return list
}

// anthropicToolChoiceToOpenAI maps {type: "tool", name: "..."} → {type: "function", function: {name: "..."}}.
func anthropicToolChoiceToOpenAI(raw json.RawMessage) json.RawMessage {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return raw
	}
	switch tc.Type {
	case "auto":
		b, _ := json.Marshal("auto")
		return b
	case "any":
		// Anthropic "any" = at least one tool must be used; closest OpenAI equiv is "required".
		b, _ := json.Marshal("required")
		return b
	case "tool":
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
		return b
	}
	return raw
}

// openaiToolChoiceToAnthropic maps OpenAI tool_choice values to Anthropic.
func openaiToolChoiceToAnthropic(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			b, _ := json.Marshal(map[string]string{"type": "auto"})
			return b
		case "required":
			// "required" = must use at least one tool → Anthropic "any"
			b, _ := json.Marshal(map[string]string{"type": "any"})
			return b
		case "none":
			// no direct Anthropic equivalent; drop it
			return nil
		}
		return raw
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if obj.Type == "function" && obj.Function.Name != "" {
		b, _ := json.Marshal(map[string]string{"type": "tool", "name": obj.Function.Name})
		return b
	}
	return raw
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
