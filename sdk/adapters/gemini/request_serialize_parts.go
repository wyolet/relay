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
			// canonical: FilePart.Filename dropped — Gemini API parts carry no file name. Vertex parts take a displayName, which the Vertex variant does not send yet.
			if v.FileData != "" {
				mt := v.MediaType
				if mt == "" {
					mt = "application/octet-stream"
				}
				out = append(out, geminiPart{InlineData: &inlineData{MIMEType: mt, Data: v.FileData}})
			} else if v.FileURL != "" {
				gp, err := t.fileAt(v.FileURL, v.MediaType)
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
	return t.imageAt(url), nil
}

// imageAt is the fileData part for an image at uri. Vertex answers 400 to a fileData without a mimeType but takes image/* for any image, so there an image whose extension names no image type goes as image/*.
func (t GeminiTranslator) imageAt(uri string) geminiPart {
	fd := &fileData{FileURI: uri}
	if t.Vertex {
		fd.MIMEType = "image/*"
		if mt := extensionMediaType(uriObjectName(uri)); strings.HasPrefix(mt, "image/") {
			fd.MIMEType = mt
		}
	}
	return geminiPart{FileData: fd}
}

// fileAt is the fileData part for a file at uri of mediaType. Vertex answers 400 to a fileData without a mimeType, so there a missing one comes from the URI, and a file whose type it does not name is refused rather than sent.
func (t GeminiTranslator) fileAt(uri, mediaType string) (geminiPart, error) {
	if t.Vertex {
		mediaType, _, _ = strings.Cut(mediaType, ";")
		mediaType = strings.TrimSpace(mediaType)
		if mediaType == "" {
			mediaType = uriMediaType(uri)
		}
		if mediaType == "" {
			return geminiPart{}, fmt.Errorf("%w: file URL with no media type and no known file extension", v1.ErrUnsupportedRequest)
		}
	}
	return geminiPart{FileData: &fileData{FileURI: uri, MIMEType: mediaType}}, nil
}

// fileMediaTypes maps the common image, audio, video and PDF extensions to the media types Vertex AI lists for Gemini input.
var fileMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif",
	".wav": "audio/wav", ".mp3": "audio/mp3", ".flac": "audio/flac", ".ogg": "audio/ogg", ".m4a": "audio/m4a", ".aac": "audio/x-aac", ".mpga": "audio/mpga",
	".mp4": "video/mp4", ".mpeg": "video/mpeg", ".mpg": "video/mpeg", ".mov": "video/quicktime", ".webm": "video/webm", ".wmv": "video/wmv", ".flv": "video/x-flv", ".3gp": "video/3gpp", ".avi": "video/avi",
	".pdf": "application/pdf",
}

// youTubeHosts serve videos Vertex fetches by their watch URL, as video/mp4.
var youTubeHosts = map[string]bool{"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true, "youtu.be": true}

// uriMediaType is the media type uri names: video/mp4 for a YouTube URL, else its extension's; "" when it names none.
func uriMediaType(uri string) string {
	if u, err := url.Parse(uri); err == nil && youTubeHosts[strings.ToLower(u.Hostname())] {
		return "video/mp4"
	}
	return extensionMediaType(uriObjectName(uri))
}

// uriObjectName is the part of uri whose extension names its type: a gs:// object name as written, since ? and # are legal in one; else the URL's path, or for a URL that does not parse, what precedes its first ? or #.
func uriObjectName(uri string) string {
	if strings.HasPrefix(uri, "gs://") {
		return uri
	}
	if u, err := url.Parse(uri); err == nil {
		return u.Path
	}
	name, _, _ := strings.Cut(uri, "?")
	name, _, _ = strings.Cut(name, "#")
	return name
}

// extensionMediaType is the fileMediaTypes entry for the extension of name's last segment, read before any ;params that follow it; "" when there is none.
func extensionMediaType(name string) string {
	segment := name[strings.LastIndexByte(name, '/')+1:]
	if mt := fileMediaTypes[strings.ToLower(path.Ext(segment))]; mt != "" {
		return mt
	}
	if beforeParams, _, found := strings.Cut(segment, ";"); found {
		return fileMediaTypes[strings.ToLower(path.Ext(beforeParams))]
	}
	return ""
}
