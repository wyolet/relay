package anthropic

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/sdk/internal/dataurl"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// canonicalPartsToAnthropicContent converts canonical []v1.Part to Anthropic content.
// All-text → plain string. Mixed → array of blocks.
func canonicalPartsToAnthropicContent(parts []v1.Part) (any, error) {
	// canonical: OutputTextPart.Annotations dropped — Anthropic text-block citations point at its own document and search-result locations, not URL offsets.
	if len(parts) == 0 {
		return "", nil
	}
	allText := true
	for _, p := range parts {
		switch p.PartType() {
		case v1.PartTypeInputText, v1.PartTypeOutputText:
		default:
			allText = false
		}
	}
	if allText {
		var sb strings.Builder
		for _, p := range parts {
			switch v := p.(type) {
			case *v1.TextPart:
				sb.WriteString(v.Text)
			case *v1.OutputTextPart:
				sb.WriteString(v.Text)
			}
		}
		return sb.String(), nil
	}

	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		block, err := canonicalPartToAnthropicBlock(p)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// canonicalPartToAnthropicBlock converts one canonical Part to an Anthropic content block.
func canonicalPartToAnthropicBlock(p v1.Part) (map[string]any, error) {
	switch v := p.(type) {
	case *v1.TextPart:
		return map[string]any{"type": "text", "text": v.Text}, nil
	case *v1.OutputTextPart:
		return map[string]any{"type": "text", "text": v.Text}, nil
	case *v1.ImagePart:
		// canonical: ImagePart.Detail dropped — Anthropic image blocks take no detail level.
		return canonicalImageURLToAnthropicBlock(v.ImageURL)
	case *v1.FilePart:
		if v.FileData != "" {
			mt := "application/pdf"
			if v.MediaType != "" {
				mt = v.MediaType
			}
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
			// canonical: FilePart.MediaType dropped for URL documents — an Anthropic url source carries no media type.
			return map[string]any{
				"type": "document",
				"source": map[string]any{
					"type": "url",
					"url":  v.FileURL,
				},
			}, nil
		}
		return nil, fmt.Errorf("anthropic serialize_request: file part has no data or URL")
	default:
		return nil, fmt.Errorf("anthropic serialize_request: unsupported part type %T", p)
	}
}

// canonicalImageURLToAnthropicBlock converts a canonical image URL to an Anthropic image block.
func canonicalImageURLToAnthropicBlock(url string) (map[string]any, error) {
	// A url source takes no data: URI, so every data URL goes as base64.
	if strings.HasPrefix(url, "data:") {
		mt, data, err := dataurl.Base64(url)
		if err != nil {
			return nil, fmt.Errorf("anthropic serialize_request: %w: image: %w", v1.ErrUnsupportedRequest, err)
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mt,
				"data":       data,
			},
		}, nil
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  url,
		},
	}, nil
}
