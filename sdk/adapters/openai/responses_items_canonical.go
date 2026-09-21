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

	case *ResponsesCustomToolCall:
		// The freeform input becomes the lowered `input` argument, so a non-OpenAI upstream sees an ordinary tool call; the marker keeps the custom shape recoverable on a same-vendor round-trip (rule 8).
		args, err := json.Marshal(map[string]string{responsesCustomInputArg: v.Input})
		if err != nil {
			return nil, fmt.Errorf("custom_tool_call arguments: %w", err)
		}
		return &v1.FunctionCall{
			ID:           v.ID,
			CallID:       v.CallID,
			Name:         v.Name,
			Arguments:    string(args),
			Status:       v1.Status(v.Status),
			ProviderData: responsesCustomCallMarker,
		}, nil

	case *ResponsesCustomToolCallOutput:
		return &v1.FunctionCallOutput{CallID: v.CallID, Output: v.Output}, nil

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
func responsesInputItemFromCanonical(item v1.Item, custom *responsesCustomLowering) ResponsesItem {
	ritem := responsesItemFromCanonical(item, custom)
	switch v := ritem.(type) {
	case *ResponsesMessage:
		if !strings.HasPrefix(v.ID, "msg_") {
			v.ID = ""
		}
	case *ResponsesFunctionCall:
		if !strings.HasPrefix(v.ID, "fc_") {
			v.ID = ""
		}
	case *ResponsesCustomToolCall:
		if !strings.HasPrefix(v.ID, "ctc_") {
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
// custom decides which function calls go back out as freeform custom_tool_call items; it may be nil when the caller has no request to read tool definitions from, in which case every call stays a function_call.
func responsesItemFromCanonical(item v1.Item, custom *responsesCustomLowering) ResponsesItem {
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
		if custom.noteCall(v) {
			return &ResponsesCustomToolCall{
				ID:     v.ID,
				CallID: v.CallID,
				Name:   v.Name,
				Input:  responsesCustomToolInput(v.Arguments),
				Status: ResponsesStatus(v.Status),
			}
		}
		return &ResponsesFunctionCall{
			ID:        v.ID,
			CallID:    v.CallID,
			Name:      v.Name,
			Arguments: v.Arguments,
			Status:    ResponsesStatus(v.Status),
		}

	case *v1.FunctionCallOutput:
		// A tool result carries neither name nor provider data, so its call id — recorded when the matching call was emitted just above — is the only thing that pairs it with a custom tool.
		if custom.isCustomOutput(v.CallID) {
			// canonical: typed parts on a custom tool result dropped — the Responses custom_tool_call_output takes a plain string.
			return &ResponsesCustomToolCallOutput{CallID: v.CallID, Output: v.Output}
		}
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
