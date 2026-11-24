package gemini

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/wyolet/relay/sdk/internal/dataurl"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func (t GeminiTranslator) canonicalPartsToGemini(parts []v1.Part) ([]geminiPart, error) {
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
			gp, err := t.canonicalImageToGemini(v.ImageURL)
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
				gp, err := t.fileDataPart("file", v.FileURL, v.MediaType)
				if err != nil {
					return nil, err
				}
				out = append(out, gp)
			} else {
				return nil, fmt.Errorf("gemini serialize_request: file part has no data or URL")
			}
		default:
			return nil, fmt.Errorf("gemini serialize_request: unsupported part type %T", p)
		}
	}
	return out, nil
}

func (t GeminiTranslator) canonicalImageToGemini(url string) (geminiPart, error) {
	// fileData.fileUri takes no data: URI, so every data URL goes inline.
	if strings.HasPrefix(url, "data:") {
		mt, data, err := dataurl.Base64(url)
		if err != nil {
			return geminiPart{}, fmt.Errorf("gemini serialize_request: %w: image: %w", v1.ErrUnsupportedRequest, err)
		}
		return geminiPart{InlineData: &inlineData{MIMEType: mt, Data: data}}, nil
	}
	// Plain URL — use fileData.
	return t.fileDataPart("image", url, "")
}

// fileDataPart is the fileData part for the kind part at uri. Vertex answers 400 to a fileData without a mimeType, so there a missing one comes from the URI's extension, and a part with neither is refused rather than sent.
func (t GeminiTranslator) fileDataPart(kind, uri, mediaType string) (geminiPart, error) {
	if t.Vertex && mediaType == "" {
		mediaType = mediaTypeByExtension(uri)
		if mediaType == "" {
			return geminiPart{}, fmt.Errorf("%w: %s URL with no media type and no known file extension", v1.ErrUnsupportedRequest, kind)
		}
	}
	return geminiPart{FileData: &fileData{FileURI: uri, MIMEType: mediaType}}, nil
}

// fileMediaTypes maps the common image, audio, video and PDF extensions to the media types Vertex AI lists for Gemini input.
var fileMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif",
	".wav": "audio/wav", ".mp3": "audio/mp3", ".flac": "audio/flac", ".ogg": "audio/ogg", ".m4a": "audio/m4a",
	".mp4": "video/mp4", ".mpeg": "video/mpeg", ".mpg": "video/mpeg", ".mov": "video/quicktime", ".webm": "video/webm", ".wmv": "video/wmv", ".flv": "video/x-flv", ".3gp": "video/3gpp",
	".pdf": "application/pdf",
}

// mediaTypeByExtension is the media type of the extension ending uri's path, "" when it is not in fileMediaTypes.
func mediaTypeByExtension(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return fileMediaTypes[strings.ToLower(path.Ext(u.Path))]
}
