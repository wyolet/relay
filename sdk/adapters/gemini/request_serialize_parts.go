package gemini

import (
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func canonicalPartsToGemini(parts []v1.Part) ([]geminiPart, error) {
	var out []geminiPart
	for _, p := range parts {
		switch v := p.(type) {
		case *v1.TextPart:
			out = append(out, geminiPart{Text: v.Text})
		case *v1.OutputTextPart:
			// canonical: OutputTextPart.Annotations dropped — a Gemini text part takes no citations on input.
			out = append(out, geminiPart{Text: v.Text})
		case *v1.ImagePart:
			// canonical: ImagePart.Detail dropped — per-part mediaResolution exists on Gemini 3+ only, and canonical carries no model generation to gate it.
			gp, err := canonicalImageToGemini(v.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, gp)
		case *v1.FilePart:
			// canonical: FilePart.Filename dropped — Gemini API parts carry no file name (displayName on inlineData/fileData is Vertex-only).
			if v.FileData != "" {
				mt := v.MediaType
				if mt == "" {
					mt = "application/octet-stream"
				}
				out = append(out, geminiPart{InlineData: &inlineData{MIMEType: mt, Data: v.FileData}})
			} else if v.FileURL != "" {
				out = append(out, geminiPart{FileData: &fileData{FileURI: v.FileURL, MIMEType: v.MediaType}})
			} else {
				return nil, fmt.Errorf("gemini serialize_request: file part has no data or URL")
			}
		default:
			return nil, fmt.Errorf("gemini serialize_request: unsupported part type %T", p)
		}
	}
	return out, nil
}

func canonicalImageToGemini(url string) (geminiPart, error) {
	if strings.HasPrefix(url, "data:") {
		rest := url[5:]
		semi := strings.Index(rest, ";")
		comma := strings.Index(rest, ",")
		if semi >= 0 && comma > semi {
			mt := rest[:semi]
			data := rest[comma+1:]
			return geminiPart{InlineData: &inlineData{MIMEType: mt, Data: data}}, nil
		}
	}
	// Plain URL — use fileData.
	return geminiPart{FileData: &fileData{FileURI: url}}, nil
}
