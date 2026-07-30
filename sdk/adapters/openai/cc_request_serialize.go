package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// SerializeRequest encodes a canonical *v1.Request to a CC /v1/chat/completions body.
// SerializeRequest encodes a canonical *v1.Request to a CC /v1/chat/completions body.
func (CCTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("cc serialize_request: model is required")
	}
	model := req.Model[0]

	out := &FullChatRequest{
		Model:    model,
		User:     req.User,
		Metadata: req.Metadata,
	}
	if req.CacheConfig != nil {
		out.PromptCacheKey = req.CacheConfig.Key
		out.PromptCacheRetention = openaiCacheRetention(req.CacheConfig)
	}

	// Extract model-specific options.
	if opts, ok := req.ModelConfig[model]; ok && opts != nil {
		if opts.Sampling != nil {
			s := opts.Sampling
			out.Temperature = s.Temperature
			out.TopP = s.TopP
			out.MaxTokens = s.MaxTokens
			out.FrequencyPenalty = s.FrequencyPenalty
			out.PresencePenalty = s.PresencePenalty
			if s.Seed != nil {
				seed := int64(*s.Seed)
				out.Seed = &seed
			}
			if len(s.Stop) > 0 {
				if b, err := json.Marshal(s.Stop); err == nil {
					out.Stop = b
				}
			}
		}
		if opts.Reasoning != nil {
			out.ReasoningEffort = opts.Reasoning.Effort
		}
		if opts.Output != nil && opts.Output.Format != nil {
			rf, err := ccFormatToResponseFormat(opts.Output.Format)
			if err != nil {
				return nil, err
			}
			out.ResponseFormat = rf
		}
	}

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				return nil, fmt.Errorf("cc serialize_request: unsupported tool type %T", tool)
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			out.Tools = append(out.Tools, Tool{
				Type: "function",
				Function: FunctionDef{
					Name:        ft.Name,
					Description: ft.Description,
					Parameters:  params,
					Strict:      ft.Strict,
				},
			})
		}
		out.ParallelToolCalls = tc.Parallel
		if tc.Choice != nil {
			if b, err := json.Marshal(tc.Choice); err == nil {
				out.ToolChoice = b
			}
		}
	}

	// Stream flag + include_usage so the terminal chunk carries token counts.
	if req.OutputMode == v1.OutputModeStream {
		t := true
		out.Stream = &t
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Messages: instructions (+ hoist-flagged system items) → leading system
	// message; other items → messages. Non-hoisted system items stay
	// positional — CC supports the system role anywhere natively.
	input, hoistedSys := v1.SplitHoistedSystem(req.Input)
	instructions := req.Instructions
	if hoistedSys != "" {
		if instructions != "" {
			instructions = instructions + "\n" + hoistedSys
		} else {
			instructions = hoistedSys
		}
	}
	msgs, err := canonicalItemsToCC(instructions, input)
	if err != nil {
		return nil, fmt.Errorf("cc serialize_request: %w", err)
	}
	out.Messages = msgs

	return json.Marshal(out)
}

// hasOpts returns true if any field in opts is set.
func hasOpts(opts *v1.ModelOpts) bool {
	return opts.Sampling != nil || opts.Reasoning != nil || opts.Output != nil
}

// canonicalItemsToCC converts canonical items and instructions to CC messages.
func canonicalItemsToCC(instructions string, items []v1.Item) ([]ChatMessage, error) {
	var msgs []ChatMessage

	if instructions != "" {
		content, _ := json.Marshal(instructions)
		msgs = append(msgs, ChatMessage{Role: "system", Content: content})
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			msg, err := canonicalMessageToCC(v)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)

		case *v1.FunctionCall:
			// Attach to the last assistant message if possible; otherwise synthesize.
			tc := ToolCall{
				ID:   v.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      v.Name,
					Arguments: v.Arguments,
				},
			}
			if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
				msgs[len(msgs)-1].ToolCalls = append(msgs[len(msgs)-1].ToolCalls, tc)
			} else {
				nullContent, _ := json.Marshal(nil)
				msgs = append(msgs, ChatMessage{
					Role:      "assistant",
					Content:   nullContent,
					ToolCalls: []ToolCall{tc},
				})
			}

		case *v1.FunctionCallOutput:
			content := ccSerializeFunctionCallOutput(v)
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				ToolCallID: v.CallID,
				Content:    content,
			})

		case *v1.Reasoning:
			// Drop reasoning items when forwarding to CC upstreams.
		}
	}

	return msgs, nil
}

// canonicalMessageToCC converts a canonical *v1.Message to a CC ChatMessage.
func canonicalMessageToCC(m *v1.Message) (ChatMessage, error) {
	msg := ChatMessage{Role: string(m.Role)}
	if m.Role == v1.RoleDeveloper {
		msg.Role = "system"
	}

	if len(m.Content) == 0 {
		nullContent, _ := json.Marshal(nil)
		msg.Content = nullContent
		return msg, nil
	}

	content, err := canonicalPartsToCC(m.Content)
	if err != nil {
		return ChatMessage{}, err
	}
	msg.Content = content
	return msg, nil
}

// canonicalPartsToCC serializes canonical []v1.Part into a CC content field.
// All-text → compact string. Mixed → array of ContentParts.
func canonicalPartsToCC(parts []v1.Part) (json.RawMessage, error) {
	allText := true
	for _, p := range parts {
		switch p.PartType() {
		case v1.PartTypeInputText, v1.PartTypeOutputText:
		default:
			allText = false
		}
	}

	if allText {
		var buf []byte
		for _, p := range parts {
			switch v := p.(type) {
			case *v1.TextPart:
				buf = append(buf, v.Text...)
			case *v1.OutputTextPart:
				buf = append(buf, v.Text...)
			}
		}
		b, _ := json.Marshal(string(buf))
		return b, nil
	}

	ccParts := make([]ContentPart, 0, len(parts))
	for _, p := range parts {
		cp, err := canonicalPartToCC(p)
		if err != nil {
			return nil, err
		}
		ccParts = append(ccParts, cp)
	}
	b, err := json.Marshal(ccParts)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// canonicalPartToCC maps one canonical v1.Part to a CC ContentPart.
func canonicalPartToCC(p v1.Part) (ContentPart, error) {
	switch v := p.(type) {
	case *v1.TextPart:
		return ContentPart{Type: "text", Text: v.Text}, nil
	case *v1.OutputTextPart:
		return ContentPart{Type: "text", Text: v.Text}, nil
	case *v1.ImagePart:
		return ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: v.ImageURL, Detail: v.Detail},
		}, nil
	case *v1.FilePart:
		fileObj := map[string]string{}
		if v.FileID != "" {
			fileObj["file_id"] = v.FileID
		}
		if v.FileData != "" {
			fileObj["file_data"] = v.FileData
		}
		if v.Filename != "" {
			fileObj["filename"] = v.Filename
		}
		b, err := json.Marshal(fileObj)
		if err != nil {
			return ContentPart{}, err
		}
		return ContentPart{Type: "file", File: b}, nil
	default:
		return ContentPart{}, fmt.Errorf("unsupported part type %T", p)
	}
}

// ccSerializeFunctionCallOutput serializes a canonical FunctionCallOutput to CC content.
func ccSerializeFunctionCallOutput(f *v1.FunctionCallOutput) json.RawMessage {
	if f.Output != "" {
		b, _ := json.Marshal(f.Output)
		return b
	}
	if len(f.Content) > 0 {
		var buf []byte
		for _, p := range f.Content {
			if tp, ok := p.(*v1.TextPart); ok {
				buf = append(buf, tp.Text...)
			}
		}
		b, _ := json.Marshal(string(buf))
		return b
	}
	b, _ := json.Marshal("")
	return b
}

// ccFormatToResponseFormat converts a canonical v1.Format to a CC ResponseFormat.
func ccFormatToResponseFormat(f *v1.Format) (*ResponseFormat, error) {
	switch f.Type {
	case "text":
		return nil, nil
	case "json_object":
		return &ResponseFormat{Type: "json_object"}, nil
	case "json_schema":
		inner := map[string]any{
			"name":   f.Name,
			"schema": f.Schema,
		}
		if f.Strict != nil {
			inner["strict"] = *f.Strict
		}
		b, err := json.Marshal(inner)
		if err != nil {
			return nil, fmt.Errorf("cc serialize_request: json_schema format: %w", err)
		}
		return &ResponseFormat{Type: "json_schema", JSONSchema: b}, nil
	default:
		return &ResponseFormat{Type: f.Type}, nil
	}
}

// --- CC → canonical stream ---
