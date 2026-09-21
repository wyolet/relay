package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// responsesPartToCanonical converts a ResponsesPart to a canonical v1.Part.
// RefusalPart → OutputTextPart (canonical rule 9: refusal is text + finish_reason).
func responsesPartToCanonical(p ResponsesPart) (v1.Part, error) {
	switch v := p.(type) {
	case *ResponsesTextPart:
		return &v1.TextPart{Text: v.Text}, nil
	case *ResponsesOutputTextPart:
		out := &v1.OutputTextPart{Text: v.Text}
		for _, a := range v.Annotations {
			ca := responsesAnnotationToCanonical(a)
			if ca != nil {
				out.Annotations = append(out.Annotations, ca)
			}
		}
		return out, nil
	case *ResponsesImagePart:
		return &v1.ImagePart{ImageURL: v.ImageURL, Detail: v.Detail}, nil
	case *ResponsesFilePart:
		return &v1.FilePart{
			FileURL:  v.FileURL,
			FileID:   v.FileID,
			FileData: v.FileData,
			Filename: v.Filename,
		}, nil
	case *ResponsesRefusalPart:
		// Canonical rule 9: refusal text lives in normal message content.
		// Map refusal part → OutputTextPart carrying the refusal text.
		return &v1.OutputTextPart{Text: v.Refusal}, nil
	default:
		return nil, fmt.Errorf("unsupported part type %T", p)
	}
}

// responsesAnnotationToCanonical converts a ResponsesAnnotation to a canonical v1.Annotation.
// responsesAnnotationToCanonical converts a ResponsesAnnotation to a canonical v1.Annotation.
// R-4: file_citation is preserved as *v1.RawAnnotation for forward compatibility.
func responsesAnnotationToCanonical(a ResponsesAnnotation) v1.Annotation {
	switch v := a.(type) {
	case *ResponsesURLCitationAnnotation:
		return &v1.URLCitationAnnotation{
			StartIndex: v.StartIndex,
			EndIndex:   v.EndIndex,
			URL:        v.URL,
			Title:      v.Title,
		}
	case *ResponsesFileCitationAnnotation:
		// file_citation has no dedicated canonical field; preserve as RawAnnotation
		// so it survives same-vendor round-trips without data loss.
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return &v1.RawAnnotation{Type: "file_citation", JSON: b}
	default:
		return nil
	}
}

// responsesPartFromCanonical converts a canonical v1.Part to a ResponsesPart.
// asOutput selects the text wire type required by the parent item's role:
// assistant message content must be output_text, everything else input_text.
// It governs both canonical text variants so a TextPart on an assistant turn
// (common from inbound parsers) still serializes as output_text, and an
// OutputTextPart spliced into a user turn degrades to input_text.
func responsesPartFromCanonical(p v1.Part, asOutput bool) ResponsesPart {
	switch v := p.(type) {
	case *v1.TextPart:
		if asOutput {
			return &ResponsesOutputTextPart{Text: v.Text}
		}
		return &ResponsesTextPart{Text: v.Text}
	case *v1.OutputTextPart:
		if !asOutput {
			return &ResponsesTextPart{Text: v.Text}
		}
		out := &ResponsesOutputTextPart{Text: v.Text}
		for _, a := range v.Annotations {
			ra := responsesAnnotationFromCanonical(a)
			if ra != nil {
				out.Annotations = append(out.Annotations, ra)
			}
		}
		return out
	case *v1.ImagePart:
		return &ResponsesImagePart{ImageURL: v.ImageURL, Detail: v.Detail}
	case *v1.FilePart:
		return &ResponsesFilePart{
			FileURL:  v.FileURL,
			FileID:   v.FileID,
			FileData: v.FileData,
			Filename: v.Filename,
		}
	default:
		return nil
	}
}

// responsesAnnotationFromCanonical converts a canonical v1.Annotation to a ResponsesAnnotation.
// responsesAnnotationFromCanonical converts a canonical v1.Annotation to a ResponsesAnnotation.
func responsesAnnotationFromCanonical(a v1.Annotation) ResponsesAnnotation {
	switch v := a.(type) {
	case *v1.URLCitationAnnotation:
		return &ResponsesURLCitationAnnotation{
			StartIndex: v.StartIndex,
			EndIndex:   v.EndIndex,
			URL:        v.URL,
			Title:      v.Title,
		}
	case *v1.RawAnnotation:
		// Round-trip opaque annotation types (e.g. file_citation) verbatim.
		if v.Type == "file_citation" && len(v.JSON) > 0 {
			var fc ResponsesFileCitationAnnotation
			if json.Unmarshal(v.JSON, &fc) == nil {
				return &fc
			}
		}
		return &ResponsesRawAnnotation{Type: v.Type, JSON: v.JSON}
	default:
		return nil
	}
}
