package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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
