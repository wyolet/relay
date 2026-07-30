package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ParseRequest decodes a CC /v1/chat/completions body into canonical *v1.Request.
// Reconstructs canonical Input from CC messages[], mapping each message role/content.
// System messages become canonical instructions (first one wins; subsequent ones
// are prepended as developer-role messages). Tool calls in assistant messages become
// FunctionCall items. Tool role messages become FunctionCallOutput items.
func (CCTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	var wire FullChatRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("cc parse_request: %w", err)
	}
	if wire.Model == "" {
		return nil, fmt.Errorf("cc parse_request: model is required")
	}

	req := &v1.Request{
		Model: v1.ModelRefs{wire.Model},
		User:  wire.User,
	}
	if wire.Metadata != nil {
		req.Metadata = wire.Metadata
	}
	req.CacheConfig = openaiCacheConfigFromWire(wire.PromptCacheKey, wire.PromptCacheRetention)

	// Build model_config for this model.
	opts := &v1.ModelOpts{}
	hasSampling := false
	sampling := &v1.SamplingParams{}
	if wire.Temperature != nil {
		sampling.Temperature = wire.Temperature
		hasSampling = true
	}
	if wire.TopP != nil {
		sampling.TopP = wire.TopP
		hasSampling = true
	}
	if wire.MaxTokens != nil {
		sampling.MaxTokens = wire.MaxTokens
		hasSampling = true
	}
	if wire.MaxCompletion != nil {
		sampling.MaxTokens = wire.MaxCompletion
		hasSampling = true
	}
	if wire.FrequencyPenalty != nil {
		sampling.FrequencyPenalty = wire.FrequencyPenalty
		hasSampling = true
	}
	if wire.PresencePenalty != nil {
		sampling.PresencePenalty = wire.PresencePenalty
		hasSampling = true
	}
	if wire.Seed != nil {
		seed := int(*wire.Seed)
		sampling.Seed = &seed
		hasSampling = true
	}
	if len(wire.Stop) > 0 {
		// Stop is string | []string raw JSON
		var stop []string
		if err := json.Unmarshal(wire.Stop, &stop); err == nil {
			sampling.Stop = stop
			hasSampling = true
		} else {
			var single string
			if err2 := json.Unmarshal(wire.Stop, &single); err2 == nil {
				sampling.Stop = []string{single}
				hasSampling = true
			}
		}
	}
	if hasSampling {
		opts.Sampling = sampling
	}

	// Tools
	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, t := range wire.Tools {
			params := t.Function.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			ft := &v1.FunctionTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
				Strict:      t.Function.Strict,
			}
			tc.Definitions = append(tc.Definitions, ft)
		}
		tc.Parallel = wire.ParallelToolCalls
		if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
			choice := &v1.ToolChoice{}
			if err := json.Unmarshal(wire.ToolChoice, choice); err == nil {
				tc.Choice = choice
			}
		}
		req.Tools = tc
	}

	// Reasoning
	if wire.ReasoningEffort != "" {
		opts.Reasoning = &v1.ReasoningConfig{Effort: wire.ReasoningEffort}
	}

	// ResponseFormat → OutputConfig
	if wire.ResponseFormat != nil {
		oc := &v1.OutputConfig{}
		f := &v1.Format{Type: wire.ResponseFormat.Type}
		if wire.ResponseFormat.JSONSchema != nil {
			// Extract name and schema from the json_schema wrapper object.
			var inner struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			}
			if err := json.Unmarshal(wire.ResponseFormat.JSONSchema, &inner); err == nil {
				f.Name = inner.Name
				f.Schema = inner.Schema
				f.Strict = inner.Strict
			}
		}
		oc.Format = f
		opts.Output = oc
	}

	// Stream mode
	if wire.Stream != nil && *wire.Stream {
		req.OutputMode = v1.OutputModeStream
	} else {
		req.OutputMode = v1.OutputModeSync
	}

	if hasOpts(opts) {
		req.ModelConfig = map[string]*v1.ModelOpts{wire.Model: opts}
	}

	// Build Input from messages.
	var instructions string
	var input []v1.Item
	for _, msg := range wire.Messages {
		switch msg.Role {
		case "system":
			// First system message → instructions; subsequent ones go as developer-role messages.
			if instructions == "" {
				text := ccContentToText(msg.Content)
				instructions = text
			} else {
				text := ccContentToText(msg.Content)
				input = append(input, &v1.Message{
					Role:    v1.RoleDeveloper,
					Content: []v1.Part{&v1.TextPart{Text: text}},
				})
			}
		case "developer":
			text := ccContentToText(msg.Content)
			input = append(input, &v1.Message{
				Role:    v1.RoleDeveloper,
				Content: []v1.Part{&v1.TextPart{Text: text}},
			})
		case "user":
			parts, err := ccContentToParts(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("cc parse_request: user message content: %w", err)
			}
			input = append(input, &v1.Message{Role: v1.RoleUser, Content: parts})
		case "assistant":
			item, err := ccAssistantMessageToItem(&msg)
			if err != nil {
				return nil, fmt.Errorf("cc parse_request: assistant message: %w", err)
			}
			input = append(input, item...)
		case "tool":
			input = append(input, &v1.FunctionCallOutput{
				CallID: msg.ToolCallID,
				Output: ccContentToText(msg.Content),
			})
		}
	}
	req.Instructions = instructions
	req.Input = input

	return req, nil
}

// ccContentToText extracts plain text from a CC content field (string or array).
func ccContentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	// Array form: concatenate text parts.
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var buf []byte
	for _, p := range parts {
		if p.Type == "text" {
			buf = append(buf, p.Text...)
		}
	}
	return string(buf)
}

// ccContentToParts converts a CC content field to canonical []v1.Part.
func ccContentToParts(raw json.RawMessage) ([]v1.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []v1.Part{&v1.TextPart{Text: s}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	var out []v1.Part
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, &v1.TextPart{Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				out = append(out, &v1.ImagePart{ImageURL: p.ImageURL.URL, Detail: p.ImageURL.Detail})
			}
		case "file":
			// Decode file object if present.
			if len(p.File) > 0 {
				var f struct {
					FileID   string `json:"file_id"`
					FileData string `json:"file_data"`
					Filename string `json:"filename"`
				}
				if err := json.Unmarshal(p.File, &f); err == nil {
					out = append(out, &v1.FilePart{
						FileID:   f.FileID,
						FileData: f.FileData,
						Filename: f.Filename,
					})
				}
			}
		}
	}
	return out, nil
}

// ccAssistantMessageToItem converts a CC assistant message to canonical items.
// Text content → Message item. Tool calls → FunctionCall items. Refusal → Message
// with finish_reason="refusal" (set on response, not here).
func ccAssistantMessageToItem(msg *ChatMessage) ([]v1.Item, error) {
	var items []v1.Item

	// Text content (may be absent when only tool_calls present).
	var textContent string
	if len(msg.Content) > 0 && string(msg.Content) != "null" {
		textContent = ccContentToText(msg.Content)
	}
	refusal := msg.Refusal

	hasContent := textContent != "" || refusal != ""
	if hasContent || len(msg.ToolCalls) == 0 {
		m := &v1.Message{Role: v1.RoleAssistant}
		if textContent != "" {
			m.Content = []v1.Part{&v1.OutputTextPart{Text: textContent}}
		}
		// Note: refusal in input messages is preserved as text for round-trip.
		if refusal != "" {
			m.Content = append(m.Content, &v1.OutputTextPart{Text: refusal})
		}
		items = append(items, m)
	}

	// Tool calls → FunctionCall items.
	for _, tc := range msg.ToolCalls {
		items = append(items, &v1.FunctionCall{
			ID:        tc.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return items, nil
}
