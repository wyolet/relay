package openai

import (
	"encoding/json"
	"fmt"
)

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
