// Package cctranslator translates between the OpenAI Responses API shape and
// the Chat Completions (CC) shape, enabling Responses inbound requests to be
// forwarded to any CC-compatible upstream (Ollama, Bedrock OpenAI-compat, Groq,
// Together, Fireworks, Google AI Studio compat, etc.).
//
// Only the forward direction is implemented: Responses inbound → CC upstream.
// The reverse (CC inbound → Responses upstream) is not a real routing case.
//
// Expected kv ops: none — this package is pure CPU transform with no I/O.
package cctranslator

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// RequestToCC translates a Responses API request to a Chat Completions
// FullChatRequest. Fields that have no CC equivalent are rejected with an
// explicit error so the caller can map to HTTP 400.
func RequestToCC(req *responses.Request) (*openai.FullChatRequest, error) {
	if err := rejectUnsupportedFields(req); err != nil {
		return nil, err
	}

	out := &openai.FullChatRequest{
		Model:             req.Model,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		ParallelToolCalls: req.ParallelToolCalls,
		Metadata:          req.Metadata,
		User:              req.User,
		Stream:            req.Stream,
		Logprobs:          req.Logprobs,
		TopLogprobs:       req.TopLogprobs,
	}

	if req.MaxOutputTokens != nil {
		v := *req.MaxOutputTokens
		out.MaxTokens = &v
	}

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.ReasoningEffort = req.Reasoning.Effort
	}

	// response_format from text.format
	if req.Text != nil && req.Text.Format != nil {
		rf, err := translateFormat(req.Text.Format)
		if err != nil {
			return nil, err
		}
		out.ResponseFormat = rf
	}

	// tools
	for _, t := range req.Tools {
		ft, ok := t.(*responses.FunctionTool)
		if !ok {
			return nil, fmt.Errorf("responses_unsupported_for_cc: tool type %q has no Chat Completions equivalent", t.ToolType())
		}
		params := ft.Parameters
		if params == nil {
			params = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, openai.Tool{
			Type: "function",
			Function: openai.FunctionDef{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  params,
				Strict:      ft.Strict,
			},
		})
	}

	// tool_choice
	if req.ToolChoice != nil {
		tc, err := translateToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	}

	// messages: system instruction prepend + items
	msgs, err := itemsToMessages(req.Instructions, req.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs

	return out, nil
}

// rejectUnsupportedFields returns an error for any field that requests
// stateful or Responses-only behavior that CC upstreams cannot honor.
func rejectUnsupportedFields(req *responses.Request) error {
	if req.PreviousResponseID != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "previous_response_id")
	}
	if req.Store != nil && *req.Store {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "store")
	}
	if req.Conversation != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "conversation")
	}
	if req.Background != nil && *req.Background {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "background")
	}
	if req.Truncation != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "truncation")
	}
	if req.ServiceTier != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "service_tier")
	}
	if req.SafetyIdentifier != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "safety_identifier")
	}
	if req.PromptCacheKey != "" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "prompt_cache_key")
	}
	if len(req.ContextManagement) > 0 && string(req.ContextManagement) != "null" {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "context_management")
	}
	if len(req.Include) > 0 {
		return fmt.Errorf("responses_unsupported_for_cc: field %q has no Chat Completions equivalent", "include")
	}
	return nil
}

// translateFormat maps responses.Format → openai.ResponseFormat.
func translateFormat(f *responses.Format) (*openai.ResponseFormat, error) {
	switch f.Type {
	case "text":
		// CC default — omit
		return nil, nil
	case "json_object":
		return &openai.ResponseFormat{Type: "json_object"}, nil
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
			return nil, fmt.Errorf("text.format.json_schema: %w", err)
		}
		return &openai.ResponseFormat{Type: "json_schema", JSONSchema: b}, nil
	default:
		// Unknown format — pass through as type string so upstream can decide.
		return &openai.ResponseFormat{Type: f.Type}, nil
	}
}

// translateToolChoice maps responses.ToolChoice → raw JSON for CC.
func translateToolChoice(tc *responses.ToolChoice) (json.RawMessage, error) {
	switch tc.Mode {
	case "auto", "required", "none":
		b, _ := json.Marshal(tc.Mode)
		return b, nil
	case "function":
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.FunctionName},
		})
		return b, nil
	default:
		b, _ := json.Marshal(tc.Mode)
		return b, nil
	}
}

// itemsToMessages converts an instructions string + Responses items array into
// a Chat Completions messages slice.
//
// Grouping rule for FunctionCall items:
//   - If a FunctionCall follows an assistant message (or another FunctionCall on
//     the same assistant turn), it is appended to that assistant message's
//     tool_calls list.
//   - Otherwise a synthetic assistant message is created to carry it.
func itemsToMessages(instructions string, items []responses.Item) ([]openai.ChatMessage, error) {
	var msgs []openai.ChatMessage

	if instructions != "" {
		content, _ := json.Marshal(instructions)
		msgs = append(msgs, openai.ChatMessage{Role: "system", Content: content})
	}

	for _, item := range items {
		switch v := item.(type) {
		case *responses.Message:
			msg, err := messageItemToCC(v)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)

		case *responses.FunctionCall:
			// Attach to the last assistant message if possible; otherwise synthesize one.
			tc := openai.ToolCall{
				ID:   v.CallID,
				Type: "function",
				Function: openai.ToolCallFunction{
					Name:      v.Name,
					Arguments: v.Arguments,
				},
			}
			if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
				last := &msgs[len(msgs)-1]
				last.ToolCalls = append(last.ToolCalls, tc)
			} else {
				// Synthesize an assistant message to carry the tool call.
				nullContent, _ := json.Marshal(nil)
				msgs = append(msgs, openai.ChatMessage{
					Role:      "assistant",
					Content:   nullContent,
					ToolCalls: []openai.ToolCall{tc},
				})
			}

		case *responses.FunctionCallOutput:
			content := functionCallOutputContent(v)
			msgs = append(msgs, openai.ChatMessage{
				Role:       "tool",
				ToolCallID: v.CallID,
				Content:    content,
			})

		case *responses.Reasoning:
			// CC upstreams have no reasoning input concept; silently drop.
			// The upstream will re-derive its own reasoning if it supports it.

		default:
			return nil, fmt.Errorf("unsupported item type %T", item)
		}
	}

	return msgs, nil
}

// messageItemToCC converts a *responses.Message to an openai.ChatMessage.
func messageItemToCC(m *responses.Message) (openai.ChatMessage, error) {
	msg := openai.ChatMessage{Role: string(m.Role)}

	if len(m.Content) == 0 {
		nullContent, _ := json.Marshal(nil)
		msg.Content = nullContent
		return msg, nil
	}

	content, err := partsToContent(m.Content)
	if err != nil {
		return openai.ChatMessage{}, err
	}
	msg.Content = content
	return msg, nil
}

// partsToContent serializes a []Part into a CC content value.
// All-text → compact string JSON. Mixed (text + images/files) → array JSON.
func partsToContent(parts []responses.Part) (json.RawMessage, error) {
	// Determine if all parts are pure text.
	allText := true
	for _, p := range parts {
		switch p.PartType() {
		case responses.PartTypeInputText, responses.PartTypeOutputText:
		default:
			allText = false
		}
	}

	if allText {
		var sb []byte
		for _, p := range parts {
			switch v := p.(type) {
			case *responses.TextPart:
				sb = append(sb, v.Text...)
			case *responses.OutputTextPart:
				sb = append(sb, v.Text...)
			}
		}
		b, _ := json.Marshal(string(sb))
		return b, nil
	}

	// Mixed content → array of content parts.
	ccParts := make([]openai.ContentPart, 0, len(parts))
	for _, p := range parts {
		cp, err := partToCC(p)
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

// partToCC maps one responses.Part to an openai.ContentPart.
func partToCC(p responses.Part) (openai.ContentPart, error) {
	switch v := p.(type) {
	case *responses.TextPart:
		return openai.ContentPart{Type: "text", Text: v.Text}, nil
	case *responses.OutputTextPart:
		return openai.ContentPart{Type: "text", Text: v.Text}, nil
	case *responses.ImagePart:
		return openai.ContentPart{
			Type:     "image_url",
			ImageURL: &openai.ImageURL{URL: v.ImageURL, Detail: v.Detail},
		}, nil
	case *responses.FilePart:
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
			return openai.ContentPart{}, err
		}
		return openai.ContentPart{Type: "file", File: b}, nil
	default:
		return openai.ContentPart{}, fmt.Errorf("unsupported part type %T", p)
	}
}

// functionCallOutputContent serializes a FunctionCallOutput to a CC tool
// message content value (always a JSON string).
func functionCallOutputContent(f *responses.FunctionCallOutput) json.RawMessage {
	if f.Output != "" {
		b, _ := json.Marshal(f.Output)
		return b
	}
	if len(f.Content) > 0 {
		// Flatten content parts to a string for CC (CC tool messages are string-only).
		var sb []byte
		for _, p := range f.Content {
			if tp, ok := p.(*responses.TextPart); ok {
				sb = append(sb, tp.Text...)
			}
		}
		b, _ := json.Marshal(string(sb))
		return b
	}
	b, _ := json.Marshal("")
	return b
}
