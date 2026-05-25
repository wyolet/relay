package openai

import (
	"encoding/json"
	"fmt"
)

// --- Input parts ---

// ResponsesTextPart is an input_text content part.
type ResponsesTextPart struct {
	Text string `json:"text"`
}

func (*ResponsesTextPart) isResponsesPart()                     {}
func (*ResponsesTextPart) ResponsesPartType() ResponsesPartType { return ResponsesPartTypeInputText }

func (p *ResponsesTextPart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type ResponsesPartType `json:"type"`
		Text string            `json:"text"`
	}
	return json.Marshal(wire{Type: ResponsesPartTypeInputText, Text: p.Text})
}

// ResponsesImagePart is an input_image content part.
type ResponsesImagePart struct {
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"` // "low" | "high" | "auto"
}

func (*ResponsesImagePart) isResponsesPart()                     {}
func (*ResponsesImagePart) ResponsesPartType() ResponsesPartType { return ResponsesPartTypeInputImage }

func (p *ResponsesImagePart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type     ResponsesPartType `json:"type"`
		ImageURL string            `json:"image_url,omitempty"`
		Detail   string            `json:"detail,omitempty"`
	}
	return json.Marshal(wire{Type: ResponsesPartTypeInputImage, ImageURL: p.ImageURL, Detail: p.Detail})
}

// ResponsesFilePart is an input_file content part.
type ResponsesFilePart struct {
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"` // base64-encoded
	Filename string `json:"filename,omitempty"`
}

func (*ResponsesFilePart) isResponsesPart()                     {}
func (*ResponsesFilePart) ResponsesPartType() ResponsesPartType { return ResponsesPartTypeInputFile }

func (p *ResponsesFilePart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type     ResponsesPartType `json:"type"`
		FileURL  string            `json:"file_url,omitempty"`
		FileID   string            `json:"file_id,omitempty"`
		FileData string            `json:"file_data,omitempty"`
		Filename string            `json:"filename,omitempty"`
	}
	return json.Marshal(wire{
		Type:     ResponsesPartTypeInputFile,
		FileURL:  p.FileURL,
		FileID:   p.FileID,
		FileData: p.FileData,
		Filename: p.Filename,
	})
}

// --- Output parts ---

// ResponsesTokenLogprob is one token's log-probability in an output.
type ResponsesTokenLogprob struct {
	Token       string                `json:"token"`
	Logprob     float64               `json:"logprob"`
	Bytes       []int                 `json:"bytes,omitempty"`
	TopLogprobs []ResponsesTopLogprob `json:"top_logprobs,omitempty"`
}

// ResponsesTopLogprob is one candidate token's log-probability.
type ResponsesTopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// ResponsesOutputTextPart is an output_text content part with optional annotations and logprobs.
type ResponsesOutputTextPart struct {
	Text        string                  `json:"text"`
	Annotations []ResponsesAnnotation   `json:"-"` // polymorphic; see MarshalJSON
	Logprobs    []ResponsesTokenLogprob `json:"-"` // spec requires []; see MarshalJSON
}

func (*ResponsesOutputTextPart) isResponsesPart() {}
func (*ResponsesOutputTextPart) ResponsesPartType() ResponsesPartType {
	return ResponsesPartTypeOutputText
}

func (p *ResponsesOutputTextPart) MarshalJSON() ([]byte, error) {
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
		logprobs = []ResponsesTokenLogprob{}
	}
	type wire struct {
		Type        ResponsesPartType       `json:"type"`
		Text        string                  `json:"text"`
		Annotations []json.RawMessage       `json:"annotations"`
		Logprobs    []ResponsesTokenLogprob `json:"logprobs"`
	}
	return json.Marshal(wire{
		Type:        ResponsesPartTypeOutputText,
		Text:        p.Text,
		Annotations: annRaws,
		Logprobs:    logprobs,
	})
}

func (p *ResponsesOutputTextPart) UnmarshalJSON(data []byte) error {
	var raw struct {
		Text        string                  `json:"text"`
		Annotations []json.RawMessage       `json:"annotations"`
		Logprobs    []ResponsesTokenLogprob `json:"logprobs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Text = raw.Text
	p.Logprobs = raw.Logprobs
	if len(raw.Annotations) > 0 {
		anns := make([]ResponsesAnnotation, 0, len(raw.Annotations))
		for _, ab := range raw.Annotations {
			a, err := responsesUnmarshalAnnotation(ab)
			if err != nil {
				return err
			}
			anns = append(anns, a)
		}
		p.Annotations = anns
	}
	return nil
}

// ResponsesRefusalPart is a refusal content part in an assistant message.
type ResponsesRefusalPart struct {
	Refusal string `json:"refusal"`
}

func (*ResponsesRefusalPart) isResponsesPart()                     {}
func (*ResponsesRefusalPart) ResponsesPartType() ResponsesPartType { return ResponsesPartTypeRefusal }

func (p *ResponsesRefusalPart) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    ResponsesPartType `json:"type"`
		Refusal string            `json:"refusal"`
	}
	return json.Marshal(wire{Type: ResponsesPartTypeRefusal, Refusal: p.Refusal})
}

// --- Annotations ---

// ResponsesURLCitationAnnotation is a url_citation annotation on output text.
type ResponsesURLCitationAnnotation struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

func (*ResponsesURLCitationAnnotation) isResponsesAnnotation()          {}
func (*ResponsesURLCitationAnnotation) ResponsesAnnotationType() string { return "url_citation" }

func (a *ResponsesURLCitationAnnotation) MarshalJSON() ([]byte, error) {
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

// ResponsesFileCitationAnnotation is a file_citation annotation on output text.
type ResponsesFileCitationAnnotation struct {
	FileID string `json:"file_id"`
	Index  int    `json:"index,omitempty"`
}

func (*ResponsesFileCitationAnnotation) isResponsesAnnotation()          {}
func (*ResponsesFileCitationAnnotation) ResponsesAnnotationType() string { return "file_citation" }

func (a *ResponsesFileCitationAnnotation) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type   string `json:"type"`
		FileID string `json:"file_id"`
		Index  int    `json:"index,omitempty"`
	}
	return json.Marshal(wire{Type: "file_citation", FileID: a.FileID, Index: a.Index})
}

// ResponsesRawAnnotation preserves unknown annotation types for forward compatibility.
type ResponsesRawAnnotation struct {
	Type string          `json:"type"`
	JSON json.RawMessage `json:"-"`
}

func (*ResponsesRawAnnotation) isResponsesAnnotation()            {}
func (a *ResponsesRawAnnotation) ResponsesAnnotationType() string { return a.Type }

func (a *ResponsesRawAnnotation) MarshalJSON() ([]byte, error) {
	if len(a.JSON) > 0 {
		return a.JSON, nil
	}
	return json.Marshal(map[string]string{"type": a.Type})
}

// responsesUnmarshalAnnotation dispatches to the correct ResponsesAnnotation concrete type.
func responsesUnmarshalAnnotation(data []byte) (ResponsesAnnotation, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("annotation: %w", err)
	}
	switch probe.Type {
	case "url_citation":
		var v ResponsesURLCitationAnnotation
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("url_citation annotation: %w", err)
		}
		return &v, nil
	case "file_citation":
		var v ResponsesFileCitationAnnotation
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("file_citation annotation: %w", err)
		}
		return &v, nil
	default:
		return &ResponsesRawAnnotation{Type: probe.Type, JSON: data}, nil
	}
}

// responsesUnmarshalContent normalizes a content field (string or []ResponsesPart) into []ResponsesPart.
// Wire string "hello" → []ResponsesPart{&ResponsesTextPart{Text:"hello"}}.
func responsesUnmarshalContent(raw json.RawMessage) ([]ResponsesPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// String form: normalize to a single TextPart.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("content string: %w", err)
		}
		return []ResponsesPart{&ResponsesTextPart{Text: s}}, nil
	}
	// Array form.
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("content array: %w", err)
	}
	parts := make([]ResponsesPart, 0, len(raws))
	for _, rb := range raws {
		p, err := responsesUnmarshalPart(rb)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// responsesUnmarshalPart dispatches to the correct ResponsesPart concrete type.
func responsesUnmarshalPart(data []byte) (ResponsesPart, error) {
	var probe struct {
		Type ResponsesPartType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("part: %w", err)
	}
	switch probe.Type {
	case ResponsesPartTypeInputText:
		var v ResponsesTextPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_text part: %w", err)
		}
		return &v, nil
	case ResponsesPartTypeInputImage:
		var v ResponsesImagePart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_image part: %w", err)
		}
		return &v, nil
	case ResponsesPartTypeInputFile:
		var v ResponsesFilePart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("input_file part: %w", err)
		}
		return &v, nil
	case ResponsesPartTypeOutputText:
		var v ResponsesOutputTextPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("output_text part: %w", err)
		}
		return &v, nil
	case ResponsesPartTypeRefusal:
		var v ResponsesRefusalPart
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("refusal part: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported part type %q", probe.Type)
	}
}
