package v1

import (
	"encoding/json"
	"fmt"
)

// --- Input parts ---

// TextPart is an input_text content part.
type TextPart struct {
	Text string `json:"text"`
}

func (*TextPart) isPart()            {}
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

func (*ImagePart) isPart()            {}
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
	FileURL   string `json:"file_url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	FileData  string `json:"file_data,omitempty"` // base64-encoded
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

func (*FilePart) isPart()            {}
func (*FilePart) PartType() PartType { return PartTypeInputFile }

func (p *FilePart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      PartType `json:"type"`
		FileURL   string   `json:"file_url,omitempty"`
		FileID    string   `json:"file_id,omitempty"`
		FileData  string   `json:"file_data,omitempty"`
		Filename  string   `json:"filename,omitempty"`
		MediaType string   `json:"media_type,omitempty"`
	}
	return json.Marshal(wire{
		Type:      PartTypeInputFile,
		FileURL:   p.FileURL,
		FileID:    p.FileID,
		FileData:  p.FileData,
		Filename:  p.Filename,
		MediaType: p.MediaType,
	})
}

// --- Output parts ---

// OutputTextPart is an output_text content part with optional annotations.
type OutputTextPart struct {
	Text        string       `json:"text"`
	Annotations []Annotation `json:"-"` // polymorphic; see MarshalJSON
}

func (*OutputTextPart) isPart()            {}
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
	type wire struct {
		Type        PartType          `json:"type"`
		Text        string            `json:"text"`
		Annotations []json.RawMessage `json:"annotations,omitempty"`
	}
	return json.Marshal(wire{
		Type:        PartTypeOutputText,
		Text:        p.Text,
		Annotations: annRaws,
	})
}

func (p *OutputTextPart) UnmarshalJSON(data []byte) error {
	var raw struct {
		Text        string            `json:"text"`
		Annotations []json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Text = raw.Text
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

// --- Annotations ---

// URLCitationAnnotation is a url_citation annotation on output text.
type URLCitationAnnotation struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

func (*URLCitationAnnotation) isAnnotation()          {}
func (*URLCitationAnnotation) AnnotationType() string { return "url_citation" }

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

// TextCitationAnnotation is a text_citation annotation referencing a span in
// an upstream document. StartIndex and EndIndex are byte offsets into the
// cited source text.
type TextCitationAnnotation struct {
	StartIndex int `json:"start_index"`
	EndIndex   int `json:"end_index"`
}

func (*TextCitationAnnotation) isAnnotation()          {}
func (*TextCitationAnnotation) AnnotationType() string { return "text_citation" }

func (a *TextCitationAnnotation) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type       string `json:"type"`
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
	}
	return json.Marshal(wire{
		Type:       "text_citation",
		StartIndex: a.StartIndex,
		EndIndex:   a.EndIndex,
	})
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
	case "text_citation":
		var v TextCitationAnnotation
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("text_citation annotation: %w", err)
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
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("content string: %w", err)
		}
		return []Part{&TextPart{Text: s}}, nil
	}
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
	default:
		return nil, fmt.Errorf("unsupported part type %q", probe.Type)
	}
}
