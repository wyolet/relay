package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// CCTranslator implements v1.Translator for the OpenAI Chat Completions wire shape.
// ParseRequest converts a CC /v1/chat/completions body to canonical *v1.Request.
// SerializeRequest converts canonical *v1.Request to a CC body.
// ParseResponse converts a CC non-streaming response to canonical *v1.Response.
// SerializeResponse converts canonical *v1.Response to a CC response body.
// The stream factories handle per-stream CC SSE ↔ canonical SSE translation.
//
// Reasoning content: CC emits reasoning_content in the delta as a non-standard
// field (some o-series upstreams). In canonical, this maps to Reasoning.Content
// (the visible string field), not ProviderData.
//
// Refusal: CC has message.refusal (*string). Canonical rule 9: refusal text lives
// in a normal message item's text content with finish_reason="refusal". On the way
// to CC, finish_reason="refusal" sets message.refusal and nulls out content.
//
// Multiplex: assumes upstream already rejected len(model)>1. SerializeRequest
// takes model[0].
type CCTranslator struct{}

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
		opts.Tools = tc
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
		if opts.Tools != nil {
			tc := opts.Tools
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
	}

	// Stream flag + include_usage so the terminal chunk carries token counts.
	if req.OutputMode == v1.OutputModeStream {
		t := true
		out.Stream = &t
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Messages: instructions → system message; items → messages.
	msgs, err := canonicalItemsToCC(req.Instructions, req.Input)
	if err != nil {
		return nil, fmt.Errorf("cc serialize_request: %w", err)
	}
	out.Messages = msgs

	return json.Marshal(out)
}

// ParseResponse decodes a CC non-streaming response body into canonical *v1.Response.
func (CCTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var cc ChatResponse
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, fmt.Errorf("cc parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        cc.ID,
		Object:    "response",
		CreatedAt: cc.Created,
		Model:     cc.Model,
	}
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}

	if cc.Usage != nil {
		resp.Usage = ccUsageToCanonical(cc.Usage)
	}

	if len(cc.Choices) == 0 {
		resp.Status = v1.StatusCompleted
		resp.FinishReason = v1.FinishReasonStop
		return resp, nil
	}

	ch := &cc.Choices[0]
	resp.Status, resp.FinishReason, resp.IncompleteDetails = ccFinishReasonToCanonical(ch.FinishReason)
	resp.Output = ccChoiceToCanonicalOutput(cc.ID, ch)

	return resp, nil
}

// SerializeResponse encodes a canonical *v1.Response to a CC response body.
// req is unused (CC doesn't require request-echo). May be nil.
// SerializeResponse encodes a canonical *v1.Response to a CC response body.
// req is unused (CC doesn't require request-echo). May be nil.
func (CCTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	// CC-5: surface errors as an OpenAI error body instead of a silent empty choices response.
	if resp.Error != nil {
		type ccError struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		type ccErrorBody struct {
			Error ccError `json:"error"`
		}
		return json.Marshal(ccErrorBody{Error: ccError{
			Message: resp.Error.Message,
			Type:    resp.Error.Code,
			Code:    resp.Error.Code,
		}})
	}

	cc := ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt,
		Model:   resp.Model,
	}
	if len(resp.Usage) > 0 {
		cc.Usage = canonicalUsageToCC(resp.Usage)
	}

	// Build choices from output items.
	var msg ChatResponseMessage
	msg.Role = "assistant"
	var toolCalls []ToolCall
	var finishReason string

	switch resp.FinishReason {
	case v1.FinishReasonStop:
		finishReason = "stop"
	case v1.FinishReasonLength:
		finishReason = "length"
	case v1.FinishReasonToolCalls:
		finishReason = "tool_calls"
	case v1.FinishReasonContentFilter:
		finishReason = "content_filter"
	case v1.FinishReasonRefusal:
		finishReason = "stop"
	default:
		finishReason = "stop"
	}

	var textBuf []byte
	var refusalText string

	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					textBuf = append(textBuf, tp.Text...)
				case *v1.TextPart:
					textBuf = append(textBuf, tp.Text...)
				}
			}
			// If finish_reason was refusal, the refusal text is the message content.
			if resp.FinishReason == v1.FinishReasonRefusal {
				refusalText = string(textBuf)
				textBuf = nil
			}
		case *v1.FunctionCall:
			toolCalls = append(toolCalls, ToolCall{
				ID:   v.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      v.Name,
					Arguments: v.Arguments,
				},
			})
		case *v1.Reasoning:
			// Echo reasoning under its original wire field, preserved in
			// provider_data (Ollama "reasoning" / o-series "reasoning_content").
			rt := v.Content
			if rt == "" {
				for _, st := range v.Summary {
					rt += st.Text
				}
			}
			if rt != "" {
				if ccReasoningField(v.ProviderData) == ccReasoningFieldOllama {
					msg.Reasoning = rt
				} else {
					msg.ReasoningContent = rt
				}
			}
		}
	}

	if refusalText != "" {
		msg.Refusal = &refusalText
	} else if len(textBuf) > 0 {
		s := string(textBuf)
		msg.Content = &s
	} else if len(toolCalls) == 0 {
		s := ""
		msg.Content = &s
	}
	msg.ToolCalls = toolCalls

	cc.Choices = []Choice{{
		Index:        0,
		Message:      msg,
		FinishReason: finishReason,
	}}

	return json.Marshal(cc)
}

// NewToCanonicalStream returns a stateful per-stream function that converts one
// CC SSE chunk into one or more canonical SSE chunks.
func (CCTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &ccToCanonicalStream{}
	return s.translate
}

// NewFromCanonicalStream returns a stateful per-stream function that converts
// one canonical SSE chunk into one or more CC chat.completion.chunk SSE frames.
// This path is live whenever an inbound /v1/chat/completions caller is routed to
// a non-OpenAI upstream (e.g. Anthropic) — the pipeline composes
// anthropic.NewToCanonicalStream → this function.
func (CCTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToCCStream{}
	return s.translate
}

// --- helpers ---

// hasOpts returns true if any field in opts is set.
func hasOpts(opts *v1.ModelOpts) bool {
	return opts.Sampling != nil || opts.Tools != nil || opts.Reasoning != nil || opts.Output != nil
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

// ccFinishReasonToCanonical maps a CC finish_reason string to canonical status/finish/incomplete.
func ccFinishReasonToCanonical(reason string) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	switch reason {
	case "stop":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "length":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_calls":
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	case "content_filter":
		return v1.StatusCompleted, v1.FinishReasonContentFilter, nil
	default:
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	}
}

// ccChoiceToCanonicalOutput converts a CC Choice to canonical []v1.Item.
// ccChoiceToCanonicalOutput converts a CC Choice to canonical []v1.Item.
// CC-2: map message-level annotations to OutputTextPart.Annotations.
func ccChoiceToCanonicalOutput(ccID string, ch *Choice) []v1.Item {
	var items []v1.Item
	msg := ch.Message

	var textContent string
	if msg.Content != nil {
		textContent = *msg.Content
	}
	refusal := ""
	if msg.Refusal != nil {
		refusal = *msg.Refusal
	}

	// Reasoning leads the output. Ollama emits "reasoning", o-series/DeepSeek
	// emit "reasoning_content"; canonical maps both to one Reasoning.Content
	// and provider_data preserves which field carried it.
	rtext, rfield := msg.ReasoningContent, ccReasoningFieldStd
	if rtext == "" && msg.Reasoning != "" {
		rtext, rfield = msg.Reasoning, ccReasoningFieldOllama
	}
	if rtext != "" {
		items = append(items, &v1.Reasoning{
			ID:           "rs_" + ccID,
			Content:      rtext,
			Summary:      []v1.SummaryText{{Text: rtext}},
			Status:       v1.StatusCompleted,
			ProviderData: ccReasoningProviderDataJSON(rfield),
		})
	}

	// Emit message item if there is text or refusal content, or no tool calls.
	if textContent != "" || refusal != "" || len(msg.ToolCalls) == 0 {
		msgItem := &v1.Message{
			ID:     "msg_" + ccID,
			Role:   v1.RoleAssistant,
			Status: v1.StatusCompleted,
		}
		if textContent != "" {
			part := &v1.OutputTextPart{Text: textContent}
			for _, ann := range msg.Annotations {
				if ann.Type == "url_citation" {
					part.Annotations = append(part.Annotations, &v1.URLCitationAnnotation{
						StartIndex: ann.URLCitation.StartIndex,
						EndIndex:   ann.URLCitation.EndIndex,
						URL:        ann.URLCitation.URL,
						Title:      ann.URLCitation.Title,
					})
				}
			}
			msgItem.Content = []v1.Part{part}
		}
		// Canonical rule 9: refusal text is in normal message content with finish_reason="refusal".
		if refusal != "" {
			msgItem.Content = append(msgItem.Content, &v1.OutputTextPart{Text: refusal})
		}
		items = append(items, msgItem)
	}

	// FunctionCall items.
	for _, tc := range msg.ToolCalls {
		items = append(items, &v1.FunctionCall{
			ID:        tc.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
			Status:    v1.StatusCompleted,
		})
	}

	return items
}

// ccUsageToCanonical maps CC's Usage block to the canonical
// orthogonal-meter Tokens map.
//
// OpenAI's prompt_tokens INCLUDES cached tokens; canonical "input"
// means non-cached input only (consistent with Anthropic semantics).
// We subtract cached_tokens from prompt_tokens so the meter
// dimensions stay non-overlapping under Tokens.Sum().
// ccUsageToCanonical maps CC's Usage block to the canonical
// orthogonal-meter Tokens map.
//
// OpenAI's prompt_tokens INCLUDES cached tokens; canonical "input"
// means non-cached input only (consistent with Anthropic semantics).
// We subtract cached_tokens from prompt_tokens so the meter
// dimensions stay non-overlapping under Tokens.Sum().
func ccUsageToCanonical(u *Usage) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	cached := int64(0)
	if u.PromptDetails != nil {
		cached = int64(u.PromptDetails.CachedTokens)
	}
	if v := int64(u.PromptTokens) - cached; v > 0 {
		t["input"] = v
	}
	if u.CompletionTokens > 0 {
		t["output"] = int64(u.CompletionTokens)
	}
	if cached > 0 {
		t["cache_read"] = cached
	}
	if u.PromptDetails != nil && u.PromptDetails.AudioTokens > 0 {
		t["audio_input"] = int64(u.PromptDetails.AudioTokens)
	}
	if u.CompletionDetails != nil {
		if u.CompletionDetails.ReasoningTokens > 0 {
			t["reasoning"] = int64(u.CompletionDetails.ReasoningTokens)
		}
		if u.CompletionDetails.AudioTokens > 0 {
			t["audio_output"] = int64(u.CompletionDetails.AudioTokens)
		}
		if u.CompletionDetails.AcceptedPredictionTokens > 0 {
			t["accepted_prediction"] = int64(u.CompletionDetails.AcceptedPredictionTokens)
		}
		if u.CompletionDetails.RejectedPredictionTokens > 0 {
			t["rejected_prediction"] = int64(u.CompletionDetails.RejectedPredictionTokens)
		}
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// canonicalUsageToCC maps a canonical orthogonal-meter map back to
// CC's Usage block. prompt_tokens is reconstructed as input +
// cache_read (CC's convention); total is the honest sum.
func canonicalUsageToCC(t usage.Tokens) *Usage {
	if len(t) == 0 {
		return nil
	}
	cached := int(t["cache_read"])
	prompt := int(t["input"]) + cached
	completion := int(t["output"])
	cu := &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      int(t.Sum()),
	}
	if cached > 0 {
		cu.PromptDetails = &PromptTokenDetails{CachedTokens: cached}
	}
	if r := int(t["reasoning"]); r > 0 {
		cu.CompletionDetails = &CompletionTokenDetails{ReasoningTokens: r}
	}
	return cu
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

type ccStreamItemKind int

const (
	ccStreamKindMessage ccStreamItemKind = iota
	ccStreamKindToolCall
	ccStreamKindReasoning
)

type ccStreamItem struct {
	kind        ccStreamItemKind
	outputIndex int
	itemID      string
	callID      string
	name        string
	textBuf     string
	argsBuf     string
	// reasoningField records the wire field name (reasoning|reasoning_content)
	// for reasoning items, preserved into the canonical item's provider_data.
	reasoningField string
}

// ccToCanonicalStream is a stateful CC SSE → canonical SSE translator.
type ccToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	nextIndex        int
	msgItem          *ccStreamItem
	reasoningItem    *ccStreamItem
	toolItems        map[int]*ccStreamItem
	lastUsage        *Usage
	lifecycleEmitted bool
	status           v1.Status
	finishReason     v1.FinishReason
}

func (s *ccToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	// Parse the CC SSE chunk.
	_, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	if bytes.Equal(data, []byte("[DONE]")) {
		return s.handleDone()
	}

	var ccChunk ChatStreamChunk
	if err := json.Unmarshal(data, &ccChunk); err != nil {
		return nil, fmt.Errorf("cc stream: parse chunk: %w", err)
	}

	if ccChunk.Usage != nil {
		s.lastUsage = ccChunk.Usage
	}

	var frames []v1.SSEFrame

	if !s.lifecycleEmitted {
		s.responseID = ccChunk.ID
		s.model = ccChunk.Model
		s.created = ccChunk.Created
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		if s.responseID == "" {
			s.responseID = fmt.Sprintf("resp_%d", s.created)
		}
		if s.toolItems == nil {
			s.toolItems = make(map[int]*ccStreamItem)
		}
		// Emit generation.created
		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		s.lifecycleEmitted = true
	}

	if len(ccChunk.Choices) == 0 {
		return marshalCanonicalFrames(frames), nil
	}

	ch := ccChunk.Choices[0]
	delta := ch.Delta

	// finish_reason arrives on the terminal chunk (separate from deltas); capture
	// it so handleDone emits the real reason instead of a hardcoded "stop".
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		s.status, s.finishReason, _ = ccFinishReasonToCanonical(*ch.FinishReason)
	}

	// Reasoning text (Ollama "reasoning" or o-series "reasoning_content").
	if rc, field := ccExtractReasoningContent(data); rc != "" {
		rf, err := s.handleReasoningDelta(rc, field)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Text content.
	if delta.Content != nil && *delta.Content != "" {
		rf, err := s.handleTextDelta(*delta.Content)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Tool calls.
	for _, tc := range delta.ToolCalls {
		rf, err := s.handleToolCallDelta(tc)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Refusal content.
	if delta.Refusal != nil && *delta.Refusal != "" {
		// Refusal in streaming: treat as text delta with finish_reason=refusal on completion.
		// Map to text delta here; the completed event will carry finish_reason=refusal.
		rf, err := s.handleTextDelta(*delta.Refusal)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	return marshalCanonicalFrames(frames), nil
}

func (s *ccToCanonicalStream) handleDone() ([]byte, error) {
	var frames []v1.SSEFrame

	// Close open reasoning item.
	if s.reasoningItem != nil {
		f := s.closeReasoningItem(s.reasoningItem)
		frames = append(frames, f...)
		s.reasoningItem = nil
	}

	// Close open message item.
	if s.msgItem != nil {
		f := s.closeMsgItem(s.msgItem)
		frames = append(frames, f...)
		s.msgItem = nil
	}

	// Close open tool call items.
	for idx, ti := range s.toolItems {
		f := s.closeToolItem(ti)
		frames = append(frames, f...)
		delete(s.toolItems, idx)
	}

	// generation.completed
	var u usage.Tokens
	if s.lastUsage != nil {
		u = ccUsageToCanonical(s.lastUsage)
	}
	status, finish := s.status, s.finishReason
	if finish == "" {
		status, finish = v1.StatusCompleted, v1.FinishReasonStop
	}
	completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
		ID:           s.responseID,
		Status:       status,
		FinishReason: finish,
		Usage:        u,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	return marshalCanonicalFrames(frames), nil
}

func (s *ccToCanonicalStream) handleReasoningDelta(text, field string) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	if s.reasoningItem == nil {
		ti := &ccStreamItem{
			kind:           ccStreamKindReasoning,
			outputIndex:    s.nextIndex,
			itemID:         fmt.Sprintf("rs_%d", s.nextIndex),
			reasoningField: field,
		}
		s.nextIndex++
		s.reasoningItem = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeReasoning,
			Index:    ti.outputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	s.reasoningItem.textBuf += text
	deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
		ItemID: s.reasoningItem.itemID,
		Index:  s.reasoningItem.outputIndex,
		Kind:   v1.DeltaKindReasoning,
		Delta:  text,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	return frames, nil
}

func (s *ccToCanonicalStream) handleTextDelta(text string) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	if s.msgItem == nil {
		// Close reasoning first if open.
		if s.reasoningItem != nil {
			frames = append(frames, s.closeReasoningItem(s.reasoningItem)...)
			s.reasoningItem = nil
		}

		ti := &ccStreamItem{
			kind:        ccStreamKindMessage,
			outputIndex: s.nextIndex,
			itemID:      fmt.Sprintf("msg_%d", s.nextIndex),
		}
		s.nextIndex++
		s.msgItem = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeMessage,
			Index:    ti.outputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	s.msgItem.textBuf += text
	deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
		ItemID: s.msgItem.itemID,
		Index:  s.msgItem.outputIndex,
		Kind:   v1.DeltaKindText,
		Delta:  text,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	return frames, nil
}

func (s *ccToCanonicalStream) handleToolCallDelta(tc ToolCallChunk) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	// Close open message item.
	if s.msgItem != nil {
		frames = append(frames, s.closeMsgItem(s.msgItem)...)
		s.msgItem = nil
	}
	// Close open reasoning item.
	if s.reasoningItem != nil {
		frames = append(frames, s.closeReasoningItem(s.reasoningItem)...)
		s.reasoningItem = nil
	}

	ti, exists := s.toolItems[tc.Index]
	if !exists {
		itemID := tc.ID
		if itemID == "" {
			itemID = fmt.Sprintf("fc_%d", s.nextIndex)
		}
		name := ""
		if tc.Function != nil {
			name = tc.Function.Name
		}
		ti = &ccStreamItem{
			kind:        ccStreamKindToolCall,
			outputIndex: s.nextIndex,
			itemID:      itemID,
			callID:      tc.ID,
			name:        name,
		}
		s.nextIndex++
		s.toolItems[tc.Index] = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeFunctionCall,
			Index:    ti.outputIndex,
			Name:     ti.name,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	if tc.Function != nil && tc.Function.Arguments != "" {
		ti.argsBuf += tc.Function.Arguments
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ti.itemID,
			Index:  ti.outputIndex,
			Kind:   v1.DeltaKindArguments,
			Delta:  tc.Function.Arguments,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	}

	return frames, nil
}

func (s *ccToCanonicalStream) closeMsgItem(ti *ccStreamItem) []v1.SSEFrame {
	finalMsg := &v1.Message{
		ID:     ti.itemID,
		Role:   v1.RoleAssistant,
		Status: v1.StatusCompleted,
	}
	if ti.textBuf != "" {
		finalMsg.Content = []v1.Part{&v1.OutputTextPart{Text: ti.textBuf}}
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalMsg,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

func (s *ccToCanonicalStream) closeReasoningItem(ti *ccStreamItem) []v1.SSEFrame {
	finalItem := &v1.Reasoning{
		ID:           ti.itemID,
		Content:      ti.textBuf,
		Status:       v1.StatusCompleted,
		ProviderData: ccReasoningProviderDataJSON(ti.reasoningField),
	}
	if ti.textBuf != "" {
		finalItem.Summary = []v1.SummaryText{{Text: ti.textBuf}}
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalItem,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

func (s *ccToCanonicalStream) closeToolItem(ti *ccStreamItem) []v1.SSEFrame {
	finalItem := &v1.FunctionCall{
		ID:        ti.itemID,
		CallID:    ti.callID,
		Name:      ti.name,
		Arguments: ti.argsBuf,
		Status:    v1.StatusCompleted,
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalItem,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

// Reasoning is carried on CC under one of two non-standard delta/message
// fields. OLLAMA DIVERGES FROM OPENAI: it maps its Thinking output to
// "reasoning", whereas OpenAI-compatible o-series / DeepSeek upstreams use
// "reasoning_content". The shared OpenAI adapter handles both rather than
// forking a near-identical pkg/adapters/ollama for a single field name; if
// Ollama's divergence grows beyond this, promote it to its own vendor adapter.
const (
	ccReasoningFieldStd    = "reasoning_content" // o-series / DeepSeek / vLLM
	ccReasoningFieldOllama = "reasoning"         // Ollama
)

// ccReasoningProviderData preserves which CC wire field carried the reasoning
// text. Canonical normalizes both names into the single Reasoning.Content
// ("canonical maps it to one"); this records the original so a canonical→CC
// serialize can echo the field verbatim ("the adapter never invents a field").
type ccReasoningProviderData struct {
	Field string `json:"cc_reasoning_field"`
}

func ccReasoningProviderDataJSON(field string) json.RawMessage {
	if field == "" {
		field = ccReasoningFieldStd
	}
	b, _ := json.Marshal(ccReasoningProviderData{Field: field})
	return b
}

// ccReasoningField reads the preserved wire field from a canonical Reasoning
// item's provider_data, defaulting to reasoning_content.
func ccReasoningField(pd json.RawMessage) string {
	if len(pd) > 0 {
		var d ccReasoningProviderData
		if json.Unmarshal(pd, &d) == nil && d.Field != "" {
			return d.Field
		}
	}
	return ccReasoningFieldStd
}

// ccExtractReasoningContent extracts the reasoning text from a raw CC chunk,
// probing both the OpenAI ("reasoning_content") and Ollama ("reasoning") field
// names. Returns the text and which field carried it (empty when neither).
func ccExtractReasoningContent(raw []byte) (text, field string) {
	var probe struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Choices) == 0 {
		return "", ""
	}
	d := probe.Choices[0].Delta
	if d.ReasoningContent != "" {
		return d.ReasoningContent, ccReasoningFieldStd
	}
	if d.Reasoning != "" {
		return d.Reasoning, ccReasoningFieldOllama
	}
	return "", ""
}

// marshalCanonicalFrames serializes a slice of v1.SSEFrame values to wire bytes.
// Returns all frames concatenated.
// canonicalToCCStream converts canonical SSE frames to OpenAI chat.completion.chunk
// SSE frames. Used by NewFromCanonicalStream when a CC inbound caller is served by
// a non-CC upstream (e.g. Anthropic → canonical → CC).
type canonicalToCCStream struct {
	responseID string
	model      string
	created    int64
	toolItems  map[string]ccFromCanonicalToolItem // itemID → per-item state
}

type ccFromCanonicalToolItem struct {
	index  int // sequential index within this response's tool_calls array
	callID string
	name   string
}

func (s *canonicalToCCStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	if s.toolItems == nil {
		s.toolItems = make(map[string]ccFromCanonicalToolItem)
	}

	var out []byte

	switch event {
	case v1.EventGenerationCreated:
		var ev v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("cc from_canonical stream: created: %w", err)
		}
		s.responseID = ev.ID
		s.model = ev.Model
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		// Role-bearing first chunk; no content yet.
		role := "assistant"
		b, _ := json.Marshal(ChatStreamChunk{
			ID:      s.responseID,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{Role: role},
				FinishReason: nil,
			}},
		})
		out = append(out, ccSSEDataFrame(b)...)

	case v1.EventItemStarted:
		var ev v1.ItemStartedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		if ev.ItemType == v1.ItemTypeFunctionCall {
			idx := len(s.toolItems)
			// Emit the id+name header chunk for this tool call immediately:
			// CC streaming convention is id+type+name on the first chunk, then
			// arguments-only deltas follow.
			callID := ev.ItemID
			s.toolItems[ev.ItemID] = ccFromCanonicalToolItem{index: idx, callID: callID, name: ev.Name}
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: StreamDelta{
						ToolCalls: []ToolCallChunk{{
							Index: idx,
							ID:    callID,
							Type:  "function",
							Function: &ToolCallFunctionChunk{
								Name: ev.Name,
							},
						}},
					},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)
		}

	case v1.EventItemDelta:
		var ev v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		switch ev.Kind {
		case v1.DeltaKindText:
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index:        0,
					Delta:        StreamDelta{Content: &ev.Delta},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)

		case v1.DeltaKindArguments:
			ti, ok := s.toolItems[ev.ItemID]
			if !ok {
				break
			}
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: StreamDelta{
						ToolCalls: []ToolCallChunk{{
							Index:    ti.index,
							Function: &ToolCallFunctionChunk{Arguments: ev.Delta},
						}},
					},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)

		case v1.DeltaKindReasoning:
			// Emits the "reasoning_content" field. Unlike the non-stream path,
			// the canonical Reasoning item's provider_data (which records the
			// original Ollama-vs-OpenAI field) only arrives at item.completed —
			// after these deltas have already flushed — so the wire field can't
			// be preserved here without buffering. reasoning_content is the safe
			// default; clients that don't understand it skip it.
			b, _ := ccMarshalReasoningChunk(s.responseID, s.model, s.created, ev.Delta)
			out = append(out, ccSSEDataFrame(b)...)
		}

	case v1.EventItemCompleted:
		// item.completed carries the full assembled item; use it to patch in the
		// real call_id for any tool call whose id we only know now.
		var evHeader struct {
			ItemID string `json:"item_id"`
			Item   struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		if ti, ok := s.toolItems[evHeader.ItemID]; ok {
			if evHeader.Item.CallID != "" && ti.callID != evHeader.Item.CallID {
				ti.callID = evHeader.Item.CallID
				s.toolItems[evHeader.ItemID] = ti
			}
		}

	case v1.EventGenerationCompleted:
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		fr := canonicalFinishReasonToCC(ev.FinishReason)
		finalChunk := ChatStreamChunk{
			ID:      s.responseID,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{},
				FinishReason: &fr,
			}},
		}
		if len(ev.Usage) > 0 {
			finalChunk.Usage = canonicalUsageToCC(ev.Usage)
		}
		b, _ := json.Marshal(finalChunk)
		out = append(out, ccSSEDataFrame(b)...)
		out = append(out, []byte("data: [DONE]\n\n")...)

	case v1.EventError:
		var ev v1.ErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errBody := map[string]any{
			"error": map[string]string{
				"message": ev.Message,
				"type":    ev.Code,
				"code":    ev.Code,
			},
		}
		b, _ := json.Marshal(errBody)
		out = append(out, ccSSEDataFrame(b)...)
		out = append(out, []byte("data: [DONE]\n\n")...)
	}

	return out, nil
}

// ccSSEDataFrame wraps JSON bytes in a CC SSE data frame (no event: line — CC
// uses bare data: lines, not event: lines).
func ccSSEDataFrame(data []byte) []byte {
	frame := make([]byte, 0, len(data)+8)
	frame = append(frame, []byte("data: ")...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

// canonicalFinishReasonToCC maps canonical FinishReason to CC finish_reason strings.
func canonicalFinishReasonToCC(fr v1.FinishReason) string {
	switch fr {
	case v1.FinishReasonStop:
		return "stop"
	case v1.FinishReasonLength:
		return "length"
	case v1.FinishReasonToolCalls:
		return "tool_calls"
	case v1.FinishReasonContentFilter:
		return "content_filter"
	case v1.FinishReasonRefusal:
		return "stop"
	default:
		return "stop"
	}
}

// ccMarshalReasoningChunk builds a CC chunk carrying a reasoning_content delta.
func ccMarshalReasoningChunk(id, model string, created int64, delta string) ([]byte, error) {
	type reasoningDelta struct {
		ReasoningContent string `json:"reasoning_content"`
	}
	type reasoningChoice struct {
		Index        int            `json:"index"`
		Delta        reasoningDelta `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	}
	type reasoningChunk struct {
		ID      string            `json:"id"`
		Object  string            `json:"object"`
		Created int64             `json:"created"`
		Model   string            `json:"model"`
		Choices []reasoningChoice `json:"choices"`
	}
	return json.Marshal(reasoningChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []reasoningChoice{{
			Index: 0,
			Delta: reasoningDelta{ReasoningContent: delta},
		}},
	})
}
func marshalCanonicalFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}
