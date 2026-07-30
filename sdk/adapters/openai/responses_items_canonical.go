package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// responsesItemToCanonical converts a ResponsesItem to a canonical v1.Item.
// responsesItemToCanonical converts a ResponsesItem to a canonical v1.Item.
func responsesItemToCanonical(item ResponsesItem) (v1.Item, error) {
	switch v := item.(type) {
	case *ResponsesMessage:
		parts := make([]v1.Part, 0, len(v.Content))
		for _, p := range v.Content {
			cp, err := responsesPartToCanonical(p)
			if err != nil {
				return nil, err
			}
			if cp != nil {
				parts = append(parts, cp)
			}
		}
		return &v1.Message{
			ID:      v.ID,
			Status:  v1.Status(v.Status),
			Role:    v1.Role(v.Role),
			Content: parts,
		}, nil

	case *ResponsesFunctionCall:
		return &v1.FunctionCall{
			ID:        v.ID,
			CallID:    v.CallID,
			Name:      v.Name,
			Arguments: v.Arguments,
			Status:    v1.Status(v.Status),
		}, nil

	case *ResponsesFunctionCallOutput:
		out := &v1.FunctionCallOutput{CallID: v.CallID, Output: v.Output}
		for _, p := range v.Content {
			cp, err := responsesPartToCanonical(p)
			if err != nil {
				return nil, err
			}
			if cp != nil {
				out.Content = append(out.Content, cp)
			}
		}
		return out, nil

	case *ResponsesReasoning:
		r := &v1.Reasoning{
			ID:     v.ID,
			Status: v1.Status(v.Status),
		}
		for _, s := range v.Summary {
			r.Summary = append(r.Summary, v1.SummaryText{Text: s.Text})
		}
		// R-1: store encrypted_content + item id in ProviderData for same-vendor round-trip.
		if v.EncryptedContent != "" {
			type reasoningProviderData struct {
				EncryptedContent string `json:"encrypted_content"`
				ID               string `json:"id,omitempty"`
			}
			if b, err := json.Marshal(reasoningProviderData{
				EncryptedContent: v.EncryptedContent,
				ID:               v.ID,
			}); err == nil {
				r.ProviderData = b
			}
		}
		return r, nil

	case *ResponsesRawItem:
		// canonical: hosted-tool item (web_search_call, mcp_call, …) dropped —
		// no canonical representation. Round-trips only within Responses, which
		// is byte-pass and never reaches this translator. Returning (nil, nil)
		// drops it without failing the parse (rule 11: annotated, not silent).
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported item type %T", item)
	}
}

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

// responsesInputItemFromCanonical converts a canonical item for the request
// input array. Input differs from output in one load-bearing way: the
// Responses API treats input[N].id as a reference to an object IT minted and
// 400s on foreign ids ("Invalid 'input[5].id': 't_0'. Expected an ID that
// begins with 'rs'"). Canonical item ids are relay-scoped — adapters mint them
// when translating other vendors' streams — so they must never go upstream:
//
//   - message / function_call: id kept only with the OpenAI per-type prefix
//     (msg_/fc_ — same-vendor round-trip), stripped otherwise; tool linkage
//     rides call_id, which the API treats as an opaque client string.
//   - reasoning: forwarded only when ProviderData restored OpenAI's own
//     encrypted_content blob (same-vendor stateless round-trip), where the
//     original rs_ id pairs it with its function_call sibling.
//     canonical: foreign reasoning items dropped on Responses input — they
//     are provider-signed (e.g. Anthropic thinking signatures) and cannot
//     round-trip cross-vendor (rule 8); their relay-minted id would 400.
func responsesInputItemFromCanonical(item v1.Item) ResponsesItem {
	ritem := responsesItemFromCanonical(item)
	switch v := ritem.(type) {
	case *ResponsesMessage:
		if !strings.HasPrefix(v.ID, "msg_") {
			v.ID = ""
		}
	case *ResponsesFunctionCall:
		if !strings.HasPrefix(v.ID, "fc_") {
			v.ID = ""
		}
	case *ResponsesReasoning:
		if v.EncryptedContent == "" {
			return nil
		}
	}
	return ritem
}

// responsesItemFromCanonical converts a canonical v1.Item to a ResponsesItem.
func responsesItemFromCanonical(item v1.Item) ResponsesItem {
	switch v := item.(type) {
	case *v1.Message:
		// The Responses API ties content-part type to role: assistant content
		// must be output_text (or refusal), user/system content input_text.
		// Honor the role here rather than the canonical part type — inbound
		// parsers (and canonical clients) don't always carry an assistant turn
		// as an OutputTextPart, and emitting input_text on an assistant message
		// is a hard 400 ("Invalid value: 'input_text'").
		asOutput := v.Role == v1.RoleAssistant
		parts := make([]ResponsesPart, 0, len(v.Content))
		for _, p := range v.Content {
			rp := responsesPartFromCanonical(p, asOutput)
			if rp != nil {
				parts = append(parts, rp)
			}
		}
		return &ResponsesMessage{
			ID:      v.ID,
			Status:  ResponsesStatus(v.Status),
			Role:    ResponsesRole(v.Role),
			Content: parts,
		}

	case *v1.FunctionCall:
		return &ResponsesFunctionCall{
			ID:        v.ID,
			CallID:    v.CallID,
			Name:      v.Name,
			Arguments: v.Arguments,
			Status:    ResponsesStatus(v.Status),
		}

	case *v1.FunctionCallOutput:
		out := &ResponsesFunctionCallOutput{
			CallID: v.CallID,
			Output: v.Output,
		}
		for _, p := range v.Content {
			rp := responsesPartFromCanonical(p, false) // tool result is model input
			if rp != nil {
				out.Content = append(out.Content, rp)
			}
		}
		return out

	case *v1.Reasoning:
		r := &ResponsesReasoning{
			ID:     v.ID,
			Status: ResponsesStatus(v.Status),
		}
		for _, s := range v.Summary {
			r.Summary = append(r.Summary, ResponsesSummaryText{Text: s.Text})
		}
		// R-1: restore encrypted_content from ProviderData for same-vendor round-trip.
		if len(v.ProviderData) > 0 {
			var pd struct {
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(v.ProviderData, &pd) == nil {
				r.EncryptedContent = pd.EncryptedContent
			}
		}
		return r

	default:
		// canonical: unknown canonical item type dropped — no Responses wire
		// representation. Latent: canonical carries only the four modeled types
		// (hosted-tool items are dropped at responsesItemToCanonical, never
		// reaching canonical), so this is unreachable today (rule 11: annotated).
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
