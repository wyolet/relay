package anthropic

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Block mappings shared by parseRequestFlat and parseRequestLayered, which
// differ only in how they decode a block's fields.

// blockSource is the source of an image or document block.
type blockSource struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	FileID    string `json:"file_id"`
}

func documentSourceToPart(src blockSource) *v1.FilePart {
	switch src.Type {
	case "base64":
		return &v1.FilePart{FileData: src.Data, MediaType: src.MediaType}
	case "url":
		return &v1.FilePart{FileURL: src.URL}
	case "file":
		return &v1.FilePart{FileID: src.FileID}
	}
	// canonical: document text and content sources dropped — FilePart.FileData is base64 file bytes, and Anthropic accepts base64 documents only as PDF.
	return nil
}

func imageSourceToURL(src blockSource) string {
	switch src.Type {
	case "url":
		return src.URL
	case "base64":
		mt := src.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		return "data:" + mt + ";base64," + src.Data
	}
	return ""
}

// thinkingToReasoning maps a thinking block, carrying a signed block whole in ProviderData for same-vendor replay.
func thinkingToReasoning(thinking, signature string) *v1.Reasoning {
	var pd json.RawMessage
	if signature != "" {
		pdMap := map[string]string{
			"type":      "thinking",
			"thinking":  thinking,
			"signature": signature,
		}
		pd, _ = json.Marshal(pdMap)
	}
	return &v1.Reasoning{
		Content:      thinking,
		Summary:      []v1.SummaryText{{Text: thinking}},
		ProviderData: pd,
	}
}
