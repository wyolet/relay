// Package anthropictranslator translates between the OpenAI Responses API shape
// and the Anthropic Messages shape, enabling Responses inbound requests to be
// forwarded to Anthropic upstreams (api.anthropic.com, AWS Bedrock, GCP Vertex).
//
// Only the forward direction is implemented: Responses inbound → Anthropic
// upstream. The reverse (Anthropic inbound → Responses upstream) is not a
// routing case per the locked architecture.
//
// Expected kv ops: none — this package is pure CPU transform with no I/O.
package anthropictranslator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

const defaultMaxTokens = 4096

// anthropicRequest is the Anthropic Messages API request shape we build.
// Kept local — callers receive []byte, not this struct.
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Metadata      *anthropicMetadata `json:"metadata,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutput   `json:"output_config,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []map[string]any
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicThinking struct {
	Type   string `json:"type"`
	Effort string `json:"effort,omitempty"`
}

type anthropicOutput struct {
	Format *anthropicOutputFormat `json:"format,omitempty"`
}

type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// RequestToAnthropic translates a Responses API request to an Anthropic
// Messages request body (JSON bytes). Fields that have no Anthropic equivalent
// are rejected with an explicit error so the caller can map to HTTP 400.
func RequestToAnthropic(req *responses.Request) ([]byte, error) {
	if err := rejectUnsupportedFields(req); err != nil {
		return nil, err
	}

	out := &anthropicRequest{
		Model:         req.Model,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
	}

	// max_tokens is required by Anthropic; default to 4096 if not set.
	if req.MaxOutputTokens != nil {
		out.MaxTokens = *req.MaxOutputTokens
	} else {
		out.MaxTokens = defaultMaxTokens
	}

	if req.Stream != nil && *req.Stream {
		out.Stream = true
	}

	// reasoning.effort → thinking
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.Thinking = &anthropicThinking{
			Type:   "enabled",
			Effort: req.Reasoning.Effort,
		}
	}

	// text.format → output_config.format
	if req.Text != nil && req.Text.Format != nil {
		oc, err := translateFormat(req.Text.Format)
		if err != nil {
			return nil, err
		}
		if oc != nil {
			out.OutputConfig = &anthropicOutput{Format: oc}
		}
	}

	// tools
	for _, t := range req.Tools {
		ft, ok := t.(*responses.FunctionTool)
		if !ok {
			return nil, fmt.Errorf("responses_unsupported_for_anthropic: tool type %q has no Anthropic equivalent", t.ToolType())
		}
		schema := ft.Parameters
		if schema == nil {
			schema = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        ft.Name,
			Description: ft.Description,
			InputSchema: schema,
		})
	}

	// tool_choice
	if req.ToolChoice != nil {
		tc, err := translateToolChoice(req.ToolChoice, req.ParallelToolCalls)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	} else if req.ParallelToolCalls != nil && !*req.ParallelToolCalls && len(out.Tools) > 0 {
		// No explicit tool_choice but parallel_tool_calls=false; wrap into auto with disable flag.
		out.ToolChoice = map[string]any{
			"type":                      "auto",
			"disable_parallel_tool_use": true,
		}
	}

	// metadata + user → metadata.user_id
	userID := req.User
	if req.Metadata != nil {
		if v, ok := req.Metadata["user_id"]; ok && v != "" {
			userID = v // metadata.user_id wins over user field
		}
	}
	if userID != "" {
		out.Metadata = &anthropicMetadata{UserID: userID}
	}

	// system from instructions
	if req.Instructions != "" {
		out.System = req.Instructions
	}

	// messages from input items
	msgs, system, err := itemsToMessages(req.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	// If items provided additional system content (developer-role messages),
	// append to instructions.
	if system != "" {
		if out.System != "" {
			out.System = out.System + "\n" + system
		} else {
			out.System = system
		}
	}

	return json.Marshal(out)
}

// rejectUnsupportedFields returns an error for fields that have no Anthropic
// equivalent and would silently change semantics if dropped.
func rejectUnsupportedFields(req *responses.Request) error {
	if req.PreviousResponseID != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "previous_response_id")
	}
	if req.Store != nil && *req.Store {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "store")
	}
	if req.Conversation != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "conversation")
	}
	if req.Background != nil && *req.Background {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "background")
	}
	if req.Truncation != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "truncation")
	}
	if req.ServiceTier != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "service_tier")
	}
	if req.SafetyIdentifier != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "safety_identifier")
	}
	if req.PromptCacheKey != "" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "prompt_cache_key")
	}
	if len(req.ContextManagement) > 0 && string(req.ContextManagement) != "null" {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "context_management")
	}
	if len(req.Include) > 0 {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "include")
	}
	if req.Logprobs != nil && *req.Logprobs {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "logprobs")
	}
	if req.TopLogprobs != nil {
		return fmt.Errorf("responses_unsupported_for_anthropic: field %q has no Anthropic Messages equivalent", "top_logprobs")
	}
	if req.Text != nil && req.Text.Format != nil && req.Text.Format.Type == "json_object" {
		return fmt.Errorf("responses_unsupported_for_anthropic: text.format type %q has no Anthropic equivalent; use type %q with a schema", "json_object", "json_schema")
	}
	return nil
}

// translateFormat maps responses.Format → anthropicOutputFormat.
// Returns nil for the "text" default (no output_config needed).
func translateFormat(f *responses.Format) (*anthropicOutputFormat, error) {
	switch f.Type {
	case "text", "":
		return nil, nil
	case "json_schema":
		return &anthropicOutputFormat{
			Type:   "json_schema",
			Schema: f.Schema,
		}, nil
	default:
		return nil, fmt.Errorf("responses_unsupported_for_anthropic: text.format type %q is not supported by Anthropic", f.Type)
	}
}

// translateToolChoice maps responses.ToolChoice → Anthropic tool_choice value.
// parallelToolCalls applies disable_parallel_tool_use when false.
func translateToolChoice(tc *responses.ToolChoice, parallelToolCalls *bool) (map[string]any, error) {
	disableParallel := parallelToolCalls != nil && !*parallelToolCalls

	switch tc.Mode {
	case "auto":
		m := map[string]any{"type": "auto"}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m, nil
	case "required":
		m := map[string]any{"type": "any"}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m, nil
	case "none":
		return map[string]any{"type": "none"}, nil
	case "function":
		m := map[string]any{"type": "tool", "name": tc.FunctionName}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m, nil
	default:
		return map[string]any{"type": "auto"}, nil
	}
}

// itemsToMessages converts a Responses items array into Anthropic messages.
// It also returns any system text extracted from developer-role messages.
//
// Grouping algorithm:
//   - Message items flush any pending tool_use/tool_result buffers and emit directly.
//   - FunctionCall items accumulate as pending assistant tool_use blocks.
//   - FunctionCallOutput items flush pending tool_use as an assistant message,
//     then accumulate as pending user tool_result blocks.
//   - Reasoning items (echoed prior reasoning) are silently dropped.
//   - Buffers are flushed at the end.
func itemsToMessages(items []responses.Item) ([]anthropicMessage, string, error) {
	var msgs []anthropicMessage
	var systemParts []string

	// pendingToolUses collects FunctionCall items before they're flushed as
	// an assistant message with tool_use content blocks.
	var pendingToolUses []responses.FunctionCall
	// pendingToolResults collects FunctionCallOutput items before they're
	// flushed as a user message with tool_result content blocks.
	var pendingToolResults []responses.FunctionCallOutput

	flushToolUses := func() {
		if len(pendingToolUses) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingToolUses))
		for _, fc := range pendingToolUses {
			// arguments is a JSON string; Anthropic expects a parsed object in input.
			var inputObj any
			if fc.Arguments != "" {
				if err := json.Unmarshal([]byte(fc.Arguments), &inputObj); err != nil {
					// If not valid JSON, keep as a string inside an object.
					inputObj = map[string]string{"_raw": fc.Arguments}
				}
			} else {
				inputObj = map[string]any{}
			}
			block := map[string]any{
				"type":  "tool_use",
				"id":    fc.CallID,
				"name":  fc.Name,
				"input": inputObj,
			}
			blocks = append(blocks, block)
		}
		msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
		pendingToolUses = pendingToolUses[:0]
	}

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingToolResults))
		for _, fco := range pendingToolResults {
			content := toolResultContent(&fco)
			block := map[string]any{
				"type":       "tool_result",
				"tool_use_id": fco.CallID,
				"content":    content,
			}
			blocks = append(blocks, block)
		}
		msgs = append(msgs, anthropicMessage{Role: "user", Content: blocks})
		pendingToolResults = pendingToolResults[:0]
	}

	for _, item := range items {
		switch v := item.(type) {
		case *responses.Message:
			// Flush any accumulated tool turns before emitting a message.
			flushToolUses()
			flushToolResults()

			role := string(v.Role)
			switch v.Role {
			case responses.RoleDeveloper:
				// Anthropic has no developer role; collect as system text.
				text := extractTextFromParts(v.Content)
				if text != "" {
					systemParts = append(systemParts, text)
				}
				continue
			case responses.RoleSystem:
				// System messages from items become system text too.
				text := extractTextFromParts(v.Content)
				if text != "" {
					systemParts = append(systemParts, text)
				}
				continue
			}

			blocks, err := partsToContentBlocks(v.Content)
			if err != nil {
				return nil, "", err
			}
			msgs = append(msgs, anthropicMessage{Role: role, Content: blocks})

		case *responses.FunctionCall:
			// Flush any pending tool_results before accumulating a tool_use.
			flushToolResults()
			pendingToolUses = append(pendingToolUses, *v)

		case *responses.FunctionCallOutput:
			// Flush pending tool_uses as an assistant message first.
			flushToolUses()
			pendingToolResults = append(pendingToolResults, *v)

		case *responses.Reasoning:
			// Prior reasoning is not echoed to Anthropic upstreams; silently drop.

		default:
			return nil, "", fmt.Errorf("unsupported item type %T", item)
		}
	}

	// Flush any remaining buffers.
	flushToolUses()
	flushToolResults()

	return msgs, strings.Join(systemParts, "\n"), nil
}

// partsToContentBlocks converts Responses []Part to Anthropic content blocks.
func partsToContentBlocks(parts []responses.Part) (any, error) {
	if len(parts) == 0 {
		return "", nil
	}

	// All-text fast path: return a plain string.
	allText := true
	for _, p := range parts {
		switch p.PartType() {
		case responses.PartTypeInputText, responses.PartTypeOutputText:
		default:
			allText = false
		}
	}
	if allText {
		var sb strings.Builder
		for _, p := range parts {
			switch v := p.(type) {
			case *responses.TextPart:
				sb.WriteString(v.Text)
			case *responses.OutputTextPart:
				sb.WriteString(v.Text)
			}
		}
		return sb.String(), nil
	}

	// Mixed content: build blocks array.
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		block, err := partToContentBlock(p)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// partToContentBlock converts one responses.Part to an Anthropic content block.
func partToContentBlock(p responses.Part) (map[string]any, error) {
	switch v := p.(type) {
	case *responses.TextPart:
		return map[string]any{"type": "text", "text": v.Text}, nil

	case *responses.OutputTextPart:
		// OutputTextPart in echoed-back assistant messages: emit as text block.
		// Annotations are response-side artifacts; drop them on the way to Anthropic.
		return map[string]any{"type": "text", "text": v.Text}, nil

	case *responses.ImagePart:
		return imagePartToBlock(v.ImageURL)

	case *responses.FilePart:
		return filePartToBlock(v)

	case *responses.RefusalPart:
		// Refusal parts in echoed-back content: emit as text.
		return map[string]any{"type": "text", "text": v.Refusal}, nil

	default:
		return nil, fmt.Errorf("unsupported part type %T", p)
	}
}

// imagePartToBlock converts an image URL (data URL or plain URL) to an Anthropic
// image content block. Replicates the logic from pkg/adapters/anthropic/content.go
// openaiImagePartToAnthropic inline to avoid a cross-package import.
func imagePartToBlock(url string) (map[string]any, error) {
	if url == "" {
		return nil, fmt.Errorf("image part has empty URL")
	}
	if strings.HasPrefix(url, "data:") {
		// data:<mediatype>;base64,<data>
		rest := url[5:]
		semi := strings.Index(rest, ";")
		comma := strings.Index(rest, ",")
		if semi >= 0 && comma > semi {
			mediaType := rest[:semi]
			data := rest[comma+1:]
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			}, nil
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  url,
		},
	}, nil
}

// filePartToBlock converts a FilePart to an Anthropic document content block.
// FileID refs are rejected; Anthropic does not accept OpenAI storage IDs.
func filePartToBlock(v *responses.FilePart) (map[string]any, error) {
	if v.FileID != "" {
		return nil, fmt.Errorf("responses_unsupported_for_anthropic: input_file with file_id has no Anthropic equivalent; upload the file content directly")
	}
	if v.FileData != "" {
		// Determine MIME type from filename if available.
		mt := guessMIMEType(v.Filename)
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mt,
				"data":       v.FileData,
			},
		}, nil
	}
	if v.FileURL != "" {
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type": "url",
				"url":  v.FileURL,
			},
		}, nil
	}
	return nil, fmt.Errorf("input_file part has no data, URL, or ID")
}

// guessMIMEType returns a MIME type based on file extension; defaults to
// application/pdf (the most common document type sent to Anthropic).
func guessMIMEType(filename string) string {
	if filename == "" {
		return "application/pdf"
	}
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	default:
		return "application/pdf"
	}
}

// extractTextFromParts joins all text parts in a content slice.
func extractTextFromParts(parts []responses.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case *responses.TextPart:
			sb.WriteString(v.Text)
		case *responses.OutputTextPart:
			sb.WriteString(v.Text)
		}
	}
	return sb.String()
}

// toolResultContent extracts the tool result content for a tool_result block.
// Returns a string for simple outputs or an array of blocks for rich content.
func toolResultContent(f *responses.FunctionCallOutput) any {
	if f.Output != "" {
		return f.Output
	}
	if len(f.Content) > 0 {
		// Flatten to string (Anthropic tool_result content can be string or block array).
		var sb strings.Builder
		for _, p := range f.Content {
			if tp, ok := p.(*responses.TextPart); ok {
				sb.WriteString(tp.Text)
			}
		}
		return sb.String()
	}
	return ""
}
