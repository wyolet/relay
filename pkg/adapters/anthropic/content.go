package anthropic

import (
	"encoding/json"
	"strings"
)

// convertContentToOpenAI walks an Anthropic message's content (string or
// array of blocks) and emits the OpenAI-equivalent (string or array of
// content parts). The conversion is mostly verbatim for text blocks; the
// interesting case is image blocks:
//
//	anthropic: {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"..."}}
//	openai:    {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,..."}}
//
//	anthropic: {"type":"image","source":{"type":"url","url":"https://..."}}
//	openai:    {"type":"image_url","image_url":{"url":"https://..."}}
//
// Unknown block types pass through unchanged — both shapes tolerate
// unknown keys, and a future canonical shape can model them properly.
func convertContentToOpenAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	// Plain string content: passthrough.
	if raw[0] == '"' {
		return raw
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		typeRaw := b["type"]
		var typ string
		_ = json.Unmarshal(typeRaw, &typ)
		switch typ {
		case "image":
			converted := anthropicImageBlockToOpenAI(b["source"])
			if converted != nil {
				out = append(out, converted)
				continue
			}
		}
		// Default: copy block verbatim by re-unmarshaling into a generic map.
		generic := map[string]any{}
		for k, v := range b {
			var anyV any
			_ = json.Unmarshal(v, &anyV)
			generic[k] = anyV
		}
		out = append(out, generic)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// convertContentFromOpenAI is the inverse: walks an OpenAI message's
// content (string or array of parts) and converts image_url parts into
// Anthropic image blocks. Returns content in Anthropic's shape.
func convertContentFromOpenAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	if raw[0] == '"' {
		return raw
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		typeRaw := p["type"]
		var typ string
		_ = json.Unmarshal(typeRaw, &typ)
		switch typ {
		case "image_url":
			converted := openaiImagePartToAnthropic(p["image_url"])
			if converted != nil {
				out = append(out, converted)
				continue
			}
		}
		generic := map[string]any{}
		for k, v := range p {
			var anyV any
			_ = json.Unmarshal(v, &anyV)
			generic[k] = anyV
		}
		out = append(out, generic)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// anthropicImageBlockToOpenAI maps Anthropic's image.source → OpenAI's
// image_url.url (a plain URL or a data URL).
func anthropicImageBlockToOpenAI(source json.RawMessage) map[string]any {
	if len(source) == 0 {
		return nil
	}
	var s struct {
		Type      string `json:"type"`
		URL       string `json:"url,omitempty"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
	}
	if err := json.Unmarshal(source, &s); err != nil {
		return nil
	}
	var url string
	switch s.Type {
	case "url":
		url = s.URL
	case "base64":
		mt := s.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		url = "data:" + mt + ";base64," + s.Data
	default:
		return nil
	}
	if url == "" {
		return nil
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url},
	}
}

// openaiImagePartToAnthropic maps OpenAI's image_url.url back into
// Anthropic's image.source. Detects data URLs and splits them into
// {base64, media_type, data}.
func openaiImagePartToAnthropic(imageURL json.RawMessage) map[string]any {
	if len(imageURL) == 0 {
		return nil
	}
	var iu struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(imageURL, &iu); err != nil {
		return nil
	}
	if iu.URL == "" {
		return nil
	}
	if strings.HasPrefix(iu.URL, "data:") {
		// data:<mediatype>;base64,<data>
		rest := iu.URL[5:]
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
			}
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  iu.URL,
		},
	}
}
