package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseRequest ----

// ParseRequest decodes an Anthropic /v1/messages request body into canonical *v1.Request.
func (AnthropicTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	var wire struct {
		Model         string            `json:"model"`
		System        json.RawMessage   `json:"system"`
		Messages      []json.RawMessage `json:"messages"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
		MaxTokens     *int              `json:"max_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		TopK          *int              `json:"top_k"`
		StopSequences []string          `json:"stop_sequences"`
		Stream        bool              `json:"stream"`
		Metadata      json.RawMessage   `json:"metadata"`
		Thinking      json.RawMessage   `json:"thinking"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("anthropic parse_request: %w", err)
	}
	if wire.Model == "" {
		return nil, fmt.Errorf("anthropic parse_request: model is required")
	}

	req := &v1.Request{
		Model: v1.ModelRefs{wire.Model},
	}

	if wire.Stream {
		req.OutputMode = v1.OutputModeStream
	} else {
		req.OutputMode = v1.OutputModeSync
	}

	// system → Instructions
	if len(wire.System) > 0 && string(wire.System) != "null" {
		req.Instructions = anthropicExtractSystemText(wire.System)
	}

	// metadata.user_id → User
	if len(wire.Metadata) > 0 && string(wire.Metadata) != "null" {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(wire.Metadata, &meta); err == nil && meta.UserID != "" {
			req.User = meta.UserID
		}
	}

	// Build model opts.
	opts := &v1.ModelOpts{}
	hasOpts := false

	// Sampling
	sp := &v1.SamplingParams{}
	hasSampling := false
	if wire.Temperature != nil {
		sp.Temperature = wire.Temperature
		hasSampling = true
	}
	if wire.TopP != nil {
		sp.TopP = wire.TopP
		hasSampling = true
	}
	if wire.TopK != nil {
		sp.TopK = wire.TopK
		hasSampling = true
	}
	if wire.MaxTokens != nil {
		sp.MaxTokens = wire.MaxTokens
		hasSampling = true
	}
	if len(wire.StopSequences) > 0 {
		sp.Stop = wire.StopSequences
		hasSampling = true
	}
	if hasSampling {
		opts.Sampling = sp
		hasOpts = true
	}

	// Tools
	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, raw := range wire.Tools {
			tool, err := anthropicParseTool(raw)
			if err != nil {
				return nil, fmt.Errorf("anthropic parse_request: tool: %w", err)
			}
			if tool != nil {
				tc.Definitions = append(tc.Definitions, tool)
			}
		}
		if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
			tc.Choice = anthropicParseToolChoice(wire.ToolChoice)
		}
		req.Tools = tc
	}

	// Thinking → ReasoningConfig
	if len(wire.Thinking) > 0 && string(wire.Thinking) != "null" {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
			Effort       string `json:"effort"`
			Display      string `json:"display"`
		}
		if err := json.Unmarshal(wire.Thinking, &thinking); err == nil {
			switch thinking.Type {
			case "enabled":
				rc := &v1.ReasoningConfig{}
				if thinking.BudgetTokens > 0 {
					rc.BudgetTokens = &thinking.BudgetTokens
				}
				if thinking.Effort != "" {
					rc.Effort = thinking.Effort
				}
				opts.Reasoning = rc
				hasOpts = true
			case "adaptive":
				// Adaptive round-trips as a budget-less ReasoningConfig; display
				// "summarized" surfaces as the canonical Summary request.
				rc := &v1.ReasoningConfig{Effort: thinking.Effort}
				if thinking.Display == "summarized" {
					rc.Summary = "auto"
				}
				opts.Reasoning = rc
				hasOpts = true
			}
		}
	}

	if hasOpts {
		req.ModelConfig = map[string]*v1.ModelOpts{wire.Model: opts}
	}

	// Build Input from messages.
	input, err := anthropicMessagesToCanonical(wire.Messages)
	if err != nil {
		return nil, fmt.Errorf("anthropic parse_request: messages: %w", err)
	}
	req.Input = input

	return req, nil
}

// anthropicExtractSystemText handles system being a plain string or an array
// of {type:"text", text:"..."} blocks.
func anthropicExtractSystemText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

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
		switch probe.Type {
		case "text":
			var block struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &block)
			parts = append(parts, &v1.TextPart{Text: block.Text})
		case "image":
			url := anthropicImageBlockToURL(b)
			if url != "" {
				parts = append(parts, &v1.ImagePart{ImageURL: url})
			}
		case "tool_result":
			// handled separately in splitToolResults
		}
	}
	return parts, nil
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
		} else {
			// Re-extract as part
			switch probe.Type {
			case "text":
				var block struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(b, &block)
				textParts = append(textParts, &v1.TextPart{Text: block.Text})
			case "image":
				url := anthropicImageBlockToURL(b)
				if url != "" {
					textParts = append(textParts, &v1.ImagePart{ImageURL: url})
				}
			}
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
