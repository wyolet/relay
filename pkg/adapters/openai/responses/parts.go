package responses

import (
	"encoding/json"
	"fmt"
)

// --- Input parts ---

// TextPart is an input_text content part.
type TextPart struct {
	Text string `json:"text"`
}

func (*TextPart) isPart()           {}
func (*TextPart) PartType() PartType { return PartTypeInputText }

func (p *TextPart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type PartType `json:"type"`
		Text string   `json:"text"`
	}
	return json.Marshal(wire{Type: PartTypeInputText, Text: p.Text})
}

// ImagePart is an input_image content part.
// Detail defaults to empty (server interprets as "auto").
type ImagePart struct {
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"` // "low" | "high" | "auto"
}

func (*ImagePart) isPart()           {}
func (*ImagePart) PartType() PartType { return PartTypeInputImage }

func (p *ImagePart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type     PartType `json:"type"`
		ImageURL string   `json:"image_url,omitempty"`
		Detail   string   `json:"detail,omitempty"`
	}
	return json.Marshal(wire{Type: PartTypeInputImage, ImageURL: p.ImageURL, Detail: p.Detail})
}

// FilePart is an input_file content part.
// Exactly one of FileURL, FileID, or FileData is set on a valid part.
type FilePart struct {
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"` // base64-encoded
	Filename string `json:"filename,omitempty"`
}

func (*FilePart) isPart()           {}
func (*FilePart) PartType() PartType { return PartTypeInputFile }

func (p *FilePart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type     PartType `json:"type"`
		FileURL  string   `json:"file_url,omitempty"`
		FileID   string   `json:"file_id,omitempty"`
		FileData string   `json:"file_data,omitempty"`
		Filename string   `json:"filename,omitempty"`
	}
	return json.Marshal(wire{
		Type:     PartTypeInputFile,
		FileURL:  p.FileURL,
		FileID:   p.FileID,
		FileData: p.FileData,
		Filename: p.Filename,
	})
}

// --- Output parts ---

// TokenLogprob is one token's log-probability in an output.
type TokenLogprob struct {
	Token       string        `json:"token"`
	Logprob     float64       `json:"logprob"`
	Bytes       []int         `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob  `json:"top_logprobs,omitempty"`
}

// TopLogprob is one candidate token's log-probability.
type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// OutputTextPart is an output_text content part with optional annotations and logprobs.
type OutputTextPart struct {
	Text        string         `json:"text"`
	Annotations []Annotation   `json:"-"` // polymorphic; see MarshalJSON
	Logprobs    []TokenLogprob `json:"-"` // spec requires []; see MarshalJSON
}

func (*OutputTextPart) isPart()           {}
func (*OutputTextPart) PartType() PartType { return PartTypeOutputText }

func (p *OutputTextPart) MarshalJSON() ([]byte, error) {
	annRaws := make([]json.RawMessage, len(p.Annotations))
	for i, a := range p.Annotations {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		annRaws[i] = b
	}
	// OpenAI spec requires both annotations and logprobs on every output_text
	// part; emit empty arrays (not null, not omitted) when we have none.
	logprobs := p.Logprobs
	if logprobs == nil {
		logprobs = []TokenLogprob{}
	}
	type wire struct {
		Type        PartType          `json:"type"`
		Text        string            `json:"text"`
		Annotations []json.RawMessage `json:"annotations"`
		Logprobs    []TokenLogprob    `json:"logprobs"`
	}
	return json.Marshal(wire{
		Type:        PartTypeOutputText,
		Text:        p.Text,
		Annotations: annRaws,
		Logprobs:    logprobs,
	})
}

func (p *OutputTextPart) UnmarshalJSON(data []byte) error {
	var raw struct {
		Text        string            `json:"text"`
		Annotations []json.RawMessage `json:"annotations"`
		Logprobs    []TokenLogprob    `json:"logprobs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Text = raw.Text
	p.Logprobs = raw.Logprobs
	if len(raw.Annotations) > 0 {
		anns := make([]Annotation, 0, len(raw.Annotations))
		for _, ab := range raw.Annotations {
			a, err := unmarshalAnnotation(ab)
			if err != nil {
				return err
			}
			anns = append(anns, a)
		}
		p.Annotations = anns
	}
	return nil
}

// RefusalPart is a refusal content part in an assistant message.
type RefusalPart struct {
	Refusal string `json:"refusal"`
}

func (*RefusalPart) isPart()           {}
func (*RefusalPart) PartType() PartType { return PartTypeRefusal }

func (p *RefusalPart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    PartType `json:"type"`
		Refusal string   `json:"refusal"`
	}
	return json.Marshal(wire{Type: PartTypeRefusal, Refusal: p.Refusal})
}

// --- Annotations ---

// URLCitationAnnotation is a url_citation annotation on output text.
type URLCitationAnnotation struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

func (*URLCitationAnnotation) isAnnotation()               {}
func (*URLCitationAnnotation) AnnotationType() string      { return "url_citation" }

func (a *URLCitationAnnotation) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type       string `json:"type"`
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
		URL        string `json:"url"`
		Title      string `json:"title,omitempty"`
	}
	return json.Marshal(wire{
		Type:       "url_citation",
		StartIndex: a.StartIndex,
		EndIndex:   a.EndIndex,
		URL:        a.URL,
		Title:      a.Title,
	})
}

// FileCitationAnnotation is a file_citation annotation on output text.
type FileCitationAnnotation struct {
	FileID string `json:"file_id"`
	Index  int    `json:"index,omitempty"`
}

func (*FileCitationAnnotation) isAnnotation()           {}
func (*FileCitationAnnotation) AnnotationType() string  { return "file_citation" }

func (a *FileCitationAnnotation) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type   string `json:"type"`
		FileID string `json:"file_id"`
		Index  int    `json:"index,omitempty"`
	}
	return json.Marshal(wire{Type: "file_citation", FileID: a.FileID, Index: a.Index})
}

// RawAnnotation preserves unknown annotation types for forward compatibility.
type RawAnnotation struct {
	Type string          `json:"type"`
	JSON json.RawMessage `json:"-"`
}

func (*RawAnnotation) isAnnotation()           {}
func (a *RawAnnotation) AnnotationType() string { return a.Type }

func (a *RawAnnotation) MarshalJSON() ([]byte, error) {
	if len(a.JSON) > 0 {
		return a.JSON, nil
	}
	return json.Marshal(map[string]string{"type": a.Type})
}

// unmarshalAnnotation dispatches to the correct Annotation concrete type.
func unmarshalAnnotation(data []byte) (Annotation, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("annotation: %w", err)
	}
	switch probe.Type {
	case "url_citation":
		var v URLCitationAnnotation
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("url_citation annotation: %w", err)
		}
		return &v, nil
	case "file_citation":
		var v FileCitationAnnotation
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("file_citation annotation: %w", err)
		}
		return &v, nil
	default:
		return &RawAnnotation{Type: probe.Type, JSON: data}, nil
	}
}

// unmarshalContent normalizes a content field (string or []Part) into []Part.
// Wire string "hello" → []Part{&TextPart{Text:"hello"}}.
func unmarshalContent(raw json.RawMessage) ([]Part, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// String form: normalize to a single TextPart.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("content string: %w", err)
		}
		return []Part{&TextPart{Text: s}}, nil
	}
	// Array form.
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("content array: %w", err)
	}
	parts := make([]Part, 0, len(raws))
	for _, rb := range raws {
		p, err := unmarshalPart(rb)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// unmarshalPart dispatches to the correct Part concrete type.
func unmarshalPart(data []byte) (Part, error) {
	var probe struct {
		Type PartType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("part: %w", err)
	}
	switch probe.Type {
	case PartTypeInputText:
		var v TextPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_text part: %w", err)
		}
		return &v, nil
	case PartTypeInputImage:
		var v ImagePart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_image part: %w", err)
		}
		return &v, nil
	case PartTypeInputFile:
		var v FilePart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_file part: %w", err)
		}
		return &v, nil
	case PartTypeOutputText:
		var v OutputTextPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("output_text part: %w", err)
		}
		return &v, nil
	case PartTypeRefusal:
		var v RefusalPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("refusal part: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported part type %q", probe.Type)
	}
}
