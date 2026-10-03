package anthropic

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// anthropicMessagesToCanonical converts Anthropic messages to canonical []v1.Item.
// Each message role maps directly. Content blocks within each message are parsed.
func anthropicMessagesToCanonical(raws []json.RawMessage) ([]v1.Item, error) {
	var items []v1.Item
	for _, raw := range raws {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, err
		}

		switch msg.Role {
		case "user":
			parts, err := anthropicContentToCanonicalParts(msg.Content)
			if err != nil {
				return nil, err
			}
			// tool_result blocks inside user messages become FunctionCallOutput items.
			toolResults, textParts := splitToolResults(parts, msg.Content)
			for _, tr := range toolResults {
				items = append(items, tr)
			}
			if sysItem := unwrapSystemUserTurn(toolResults, textParts); sysItem != nil {
				items = append(items, sysItem)
			} else if len(textParts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: textParts})
			}

		case "system":
			// Mid-conversation system message (role:system inside the messages
			// array, distinct from the top-level system field) — kept positional.
			parts, err := anthropicContentToCanonicalParts(msg.Content)
			if err != nil {
				return nil, err
			}
			if len(parts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleSystem, Content: parts})
			}

		case "assistant":
			msgItem, toolCalls, err := anthropicAssistantContentToItems(msg.Content)
			if err != nil {
				return nil, err
			}
			if msgItem != nil {
				items = append(items, msgItem)
			}
			items = append(items, toolCalls...)

		default:
			// Unknown roles become user messages.
			parts, _ := anthropicContentToCanonicalParts(msg.Content)
			if len(parts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: parts})
			}
		}
	}
	return items, nil
}

// anthropicContentToCanonicalParts converts Anthropic content (string or []block) to canonical []Part.
func anthropicContentToCanonicalParts(raw json.RawMessage) ([]v1.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Plain string
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []v1.Part{&v1.TextPart{Text: s}}, nil
	}
	// Array of blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var parts []v1.Part
	for _, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		// tool_result blocks are handled separately in splitToolResults.
		if p := anthropicBlockToPart(b, probe.Type); p != nil {
			parts = append(parts, p)
		}
	}
	return parts, nil
}

// anthropicBlockToPart maps a text or image content block to a canonical part.
// Any other block type (or an image with no usable source) yields nil.
func anthropicBlockToPart(raw json.RawMessage, blockType string) v1.Part {
	switch blockType {
	case "text":
		var block struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &block)
		return &v1.TextPart{Text: block.Text}
	case "image":
		if url := anthropicImageBlockToURL(raw); url != "" {
			return &v1.ImagePart{ImageURL: url}
		}
	}
	return nil
}

// splitToolResults extracts tool_result blocks from raw content and returns them as
// FunctionCallOutput items + remaining text/image parts.
func splitToolResults(parts []v1.Part, raw json.RawMessage) ([]*v1.FunctionCallOutput, []v1.Part) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, parts
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, parts
	}
	var toolResults []*v1.FunctionCallOutput
	var textParts []v1.Part
	for _, b := range blocks {
		var probe struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		if probe.Type == "tool_result" {
			output := ""
			var mediaParts []v1.Part
			if len(probe.Content) > 0 {
				// content can be string or array of blocks
				if probe.Content[0] == '"' {
					_ = json.Unmarshal(probe.Content, &output)
				} else {
					var contentParts []v1.Part
					contentParts, _ = anthropicContentToCanonicalParts(probe.Content)
					hasMedia := false
					for _, p := range contentParts {
						if tp, ok := p.(*v1.TextPart); ok {
							output += tp.Text
						} else {
							hasMedia = true
						}
					}
					// Media-carrying tool results (image blocks — e.g. a file
					// read returning a PNG) keep the full part list on Content
					// so downstream serializers can emit it; text-only results
					// stay on the plain Output string as before.
					if hasMedia {
						mediaParts = contentParts
					}
				}
			}
			toolResults = append(toolResults, &v1.FunctionCallOutput{
				CallID:  probe.ToolUseID,
				Output:  output,
				Content: mediaParts,
			})
		} else if p := anthropicBlockToPart(b, probe.Type); p != nil {
			textParts = append(textParts, p)
		}
	}
	return toolResults, textParts
}

// anthropicAssistantContentToItems converts an assistant message content to canonical items.
// Returns a Message item (for text content) and FunctionCall items (for tool_use blocks).
func anthropicAssistantContentToItems(raw json.RawMessage) (*v1.Message, []v1.Item, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return &v1.Message{Role: v1.RoleAssistant}, nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, err
		}
		return &v1.Message{
			Role:    v1.RoleAssistant,
			Content: []v1.Part{&v1.OutputTextPart{Text: s}},
		}, nil, nil
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, err
	}

	var textParts []v1.Part
	var toolItems []v1.Item
	for _, b := range blocks {
		var probe struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
			Signature string          `json:"signature,omitempty"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			textParts = append(textParts, &v1.OutputTextPart{Text: probe.Text})
		case "tool_use":
			args := "{}"
			if len(probe.Input) > 0 {
				args = string(probe.Input)
			}
			toolItems = append(toolItems, &v1.FunctionCall{
				ID:        probe.ID,
				CallID:    probe.ID,
				Name:      probe.Name,
				Arguments: args,
			})
		case "thinking":
			var pd json.RawMessage
			if probe.Signature != "" {
				pdMap := map[string]string{
					"type":      "thinking",
					"thinking":  probe.Thinking,
					"signature": probe.Signature,
				}
				pd, _ = json.Marshal(pdMap)
			}
			toolItems = append(toolItems, &v1.Reasoning{
				Content:      probe.Thinking,
				Summary:      []v1.SummaryText{{Text: probe.Thinking}},
				ProviderData: pd,
			})
		}
	}

	var msgItem *v1.Message
	if len(textParts) > 0 || len(toolItems) == 0 {
		msgItem = &v1.Message{Role: v1.RoleAssistant, Content: textParts}
	}
	return msgItem, toolItems, nil
}

// anthropicImageBlockToURL converts an Anthropic image content block to a URL string.
func anthropicImageBlockToURL(raw json.RawMessage) string {
	var block struct {
		Source struct {
			Type      string `json:"type"`
			URL       string `json:"url,omitempty"`
			MediaType string `json:"media_type,omitempty"`
			Data      string `json:"data,omitempty"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return ""
	}
	switch block.Source.Type {
	case "url":
		return block.Source.URL
	case "base64":
		mt := block.Source.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		return "data:" + mt + ";base64," + block.Source.Data
	}
	return ""
}
