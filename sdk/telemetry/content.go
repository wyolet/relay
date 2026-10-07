package telemetry

import (
	"encoding/json"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Content attribute names. Their values follow the conventions' JSON schemas for system instructions, input and output messages, and tool definitions.
const (
	attrSystemInstructions = "gen_ai.system_instructions"
	attrInputMessages      = "gen_ai.input.messages"
	attrOutputMessages     = "gen_ai.output.messages"
	attrToolDefinitions    = "gen_ai.tool.definitions"
)

// Content is a call's message content, already in the conventions' shapes. Build it on the goroutine that owns the request and the response: everything is copied, so both may be reused afterwards.
type Content struct {
	attrs []keyValue
}

// NewContent returns the input side of a call made with req: its instructions, input messages and tool definitions.
func NewContent(req *v1.Request) *Content {
	c := &Content{}
	if req == nil {
		return c
	}
	if req.Instructions != "" {
		c.attrs = append(c.attrs, keyValue{Key: attrSystemInstructions, Value: valueOf([]any{textPart(req.Instructions)})})
	}
	if msgs := messages(req.Input); len(msgs) > 0 {
		c.attrs = append(c.attrs, keyValue{Key: attrInputMessages, Value: valueOf(msgs)})
	}
	if req.Tools != nil {
		if defs := toolDefinitions(req.Tools.Definitions); len(defs) > 0 {
			c.attrs = append(c.attrs, keyValue{Key: attrToolDefinitions, Value: valueOf(defs)})
		}
	}
	return c
}

// SetOutput adds the model's output items, which ended with finish, as the output messages.
func (c *Content) SetOutput(output []v1.Item, finish v1.FinishReason) {
	if c == nil {
		return
	}
	msgs := messages(output)
	if len(msgs) == 0 {
		return
	}
	if finish != "" {
		reason := outputFinishReason(finish)
		for _, m := range msgs {
			m.(map[string]any)["finish_reason"] = reason
		}
	}
	c.attrs = append(c.attrs, keyValue{Key: attrOutputMessages, Value: valueOf(msgs)})
}

// outputFinishReason names a canonical finish reason as the output message schema does. A reason the schema does not list, such as refusal, is kept as it is; the schema allows any string.
func outputFinishReason(f v1.FinishReason) string {
	if f == v1.FinishReasonToolCalls {
		return "tool_call"
	}
	return string(f)
}

// messages maps canonical items to chat messages. Consecutive assistant-side items (text, reasoning, tool calls) form one assistant message and consecutive tool results one tool message, as providers group them into turns; every other message stands alone.
func messages(items []v1.Item) []any {
	var out []any
	appendPart := func(role string, part map[string]any) {
		if n := len(out); n > 0 {
			last := out[n-1].(map[string]any)
			if last["role"] == role && (role == string(v1.RoleAssistant) || role == "tool") {
				last["parts"] = append(last["parts"].([]any), part)
				return
			}
		}
		out = append(out, map[string]any{"role": role, "parts": []any{part}})
	}
	for _, it := range items {
		switch x := it.(type) {
		case *v1.Message:
			// canonical: Message.ProviderData, CacheConfig and Hoist dropped — opaque or delivery hints, not content.
			parts := contentParts(x.Content)
			if x.Role == v1.RoleAssistant {
				for _, p := range parts {
					appendPart(string(x.Role), p.(map[string]any))
				}
				if len(parts) > 0 {
					continue
				}
			}
			out = append(out, map[string]any{"role": string(x.Role), "parts": parts})
		case *v1.FunctionCall:
			// canonical: FunctionCall.ProviderData dropped — opaque vendor payload.
			appendPart(string(v1.RoleAssistant), map[string]any{"type": "tool_call", "id": x.CallID, "name": x.Name, "arguments": decodedJSON(x.Arguments)})
		case *v1.Reasoning:
			if p, ok := reasoningPart(x); ok {
				appendPart(string(v1.RoleAssistant), p)
			}
		case *v1.FunctionCallOutput:
			var response any = x.Output
			if x.Output == "" && len(x.Content) > 0 {
				response = contentParts(x.Content)
			}
			appendPart("tool", map[string]any{"type": "tool_call_response", "id": x.CallID, "response": response})
		}
	}
	return out
}

// reasoningPart reports false for a reasoning item with no readable text, such as one carrying only an encrypted payload.
//
// canonical: Reasoning.ProviderData dropped — signatures and encrypted reasoning are opaque.
func reasoningPart(r *v1.Reasoning) (map[string]any, bool) {
	text := r.Content
	if text == "" {
		summaries := make([]string, 0, len(r.Summary))
		for _, s := range r.Summary {
			summaries = append(summaries, s.Text)
		}
		text = strings.Join(summaries, "\n\n")
	}
	if text == "" {
		return nil, false
	}
	return map[string]any{"type": "reasoning", "content": text}, true
}

func contentParts(parts []v1.Part) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch x := p.(type) {
		case *v1.TextPart:
			out = append(out, textPart(x.Text))
		case *v1.OutputTextPart:
			// canonical: OutputTextPart.Annotations dropped — the message schema has no citations.
			out = append(out, textPart(x.Text))
		case *v1.ImagePart:
			// canonical: ImagePart.Detail dropped — a request hint, not content.
			out = append(out, mediaPart("image", "", x.ImageURL))
		case *v1.FilePart:
			// canonical: FilePart.Filename dropped — the message schema has no file name.
			modality := modalityOf(x.MediaType)
			switch {
			case x.FileID != "":
				part := map[string]any{"type": "file", "modality": modality, "file_id": x.FileID}
				if x.MediaType != "" {
					part["mime_type"] = x.MediaType
				}
				out = append(out, part)
			case x.FileData != "":
				part := map[string]any{"type": "blob", "modality": modality, "content": x.FileData}
				if x.MediaType != "" {
					part["mime_type"] = x.MediaType
				}
				out = append(out, part)
			default:
				out = append(out, mediaPart(modality, x.MediaType, x.FileURL))
			}
		}
	}
	return out
}

func textPart(text string) map[string]any { return map[string]any{"type": "text", "content": text} }

// mediaPart is a blob part for a base64 data URL, a uri part for any other URL.
func mediaPart(modality, mimeType, url string) map[string]any {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok && strings.HasSuffix(meta, ";base64") {
			part := map[string]any{"type": "blob", "modality": modality, "content": data}
			if mt := strings.TrimSuffix(meta, ";base64"); mt != "" {
				part["mime_type"] = mt
			}
			return part
		}
	}
	part := map[string]any{"type": "uri", "modality": modality, "uri": url}
	if mimeType != "" {
		part["mime_type"] = mimeType
	}
	return part
}

// modalityOf picks the schema's modality from a media type; anything not image, audio or video is a document.
func modalityOf(mediaType string) string {
	for _, m := range [...]string{"image", "audio", "video"} {
		if strings.HasPrefix(mediaType, m+"/") {
			return m
		}
	}
	return "document"
}

func toolDefinitions(tools v1.Tools) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		switch x := t.(type) {
		case *v1.FunctionTool:
			// canonical: FunctionTool.Strict and ProviderData dropped — request options, not part of the definition's schema.
			def := map[string]any{"type": "function", "name": x.Name}
			if x.Description != "" {
				def["description"] = x.Description
			}
			if len(x.Parameters) > 0 {
				def["parameters"] = decodedJSON(string(x.Parameters))
			}
			out = append(out, def)
		case *v1.ServerTool:
			out = append(out, map[string]any{"type": string(v1.ToolTypeServer), "name": x.Name})
		case *v1.MCPTool:
			// canonical: MCPTool.Headers dropped — they carry the MCP server's credentials.
			def := map[string]any{"type": string(v1.ToolTypeMCP), "name": x.Name}
			if x.ServerURL != "" {
				def["server_url"] = x.ServerURL
			}
			out = append(out, def)
		}
	}
	return out
}

// decodedJSON returns JSON text as a structured value, or the text itself when it is not valid JSON. Numbers keep their written form.
func decodedJSON(text string) any {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return text
	}
	return v
}
