package openai

import (
	"encoding/json"
	"slices"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason, reading of the output only each item's type and a message's part types.
func (ResponsesTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	var r struct {
		Status            ResponsesStatus             `json:"status"`
		Output            []responsesItemSpan         `json:"output"`
		Usage             *ResponsesUsage             `json:"usage"`
		ServiceTier       string                      `json:"service_tier"`
		IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return v1.Summary{}, err
	}
	calls, refused := false, false
	for _, item := range r.Output {
		call, refusal, err := responsesItemFinish(item)
		if err != nil {
			return v1.Summary{}, err
		}
		calls, refused = calls || call, refused || refusal
	}
	return v1.Summary{
		Tokens:       responsesUsageToCanonical(r.Usage),
		FinishReason: responsesFinishReason(r.Status, r.IncompleteDetails, calls, refused),
		ServiceTier:  r.ServiceTier,
	}, nil
}

// responsesItemSpan holds one output item's bytes, as the json.RawMessage UnmarshalResponsesResponse decodes output into, without the copy: they alias the body being decoded, which outlives every use.
type responsesItemSpan []byte

func (s *responsesItemSpan) UnmarshalJSON(b []byte) error {
	*s = b
	return nil
}

// responsesItemFinish reads an output item, already validated by the outer decode, the way responsesUnmarshalItem types it: a function or custom tool call, or a message holding a refusal part. Scanning, not decoding, finds its members; an item the scan cannot read is decoded instead, failing where responsesUnmarshalItem does.
func responsesItemFinish(item []byte) (call, refusal bool, err error) {
	typ, content, ok := responsesItemMembers(item)
	if !ok {
		var v struct {
			Type    ResponsesItemType `json:"type"`
			Content responsesItemSpan `json:"content"`
		}
		if err := json.Unmarshal(item, &v); err != nil {
			return false, false, err
		}
		typ, content = v.Type, v.Content
	}
	switch typ {
	case ResponsesItemTypeFunctionCall, ResponsesItemTypeCustomToolCall:
		return true, false, nil
	case ResponsesItemTypeMessage:
		refusal, err = responsesContentRefuses(content)
		return false, refusal, err
	}
	return false, false, nil
}

// responsesItemMembers scans an item object for its last type and content members; false when the scan cannot settle them (not an object, an escaped key, a type that is not a string).
func responsesItemMembers(item []byte) (typ ResponsesItemType, content []byte, ok bool) {
	m := jsonscan.Object(item)
	for m.Next() {
		switch k, v := m.Key(), m.Value(); {
		case jsonscan.KeyIs(k, "type"):
			if json.Unmarshal(v, &typ) != nil {
				return "", nil, false
			}
		case jsonscan.KeyIs(k, "content"):
			content = v
		}
	}
	return typ, content, !m.Err()
}

// responsesContentRefuses reports whether message content holds a refusal part, failing where responsesUnmarshalContent does: string or absent content has no parts, and a part of any type decodes (an unmodeled one as a ResponsesRawPart).
func responsesContentRefuses(content []byte) (bool, error) {
	if len(content) == 0 || content[0] == '"' {
		return false, nil
	}
	types, ok := responsesPartTypes(content)
	if !ok {
		var parts []struct {
			Type ResponsesPartType `json:"type"`
		}
		if err := json.Unmarshal(content, &parts); err != nil {
			return false, err
		}
		types = types[:0]
		for _, p := range parts {
			types = append(types, p.Type)
		}
	}
	return slices.Contains(types, ResponsesPartTypeRefusal), nil
}

// responsesPartTypes scans a content array for each part's last type; false when the scan cannot settle them.
func responsesPartTypes(content []byte) ([]ResponsesPartType, bool) {
	if jsonscan.IsNull(content) {
		return nil, true
	}
	var types []ResponsesPartType
	parts := jsonscan.Array(content)
	for parts.Next() {
		var typ ResponsesPartType
		if !jsonscan.IsNull(parts.Value()) {
			p := jsonscan.Object(parts.Value())
			for p.Next() {
				if jsonscan.KeyIs(p.Key(), "type") && json.Unmarshal(p.Value(), &typ) != nil {
					return nil, false
				}
			}
			if p.Err() {
				return nil, false
			}
		}
		types = append(types, typ)
	}
	return types, !parts.Err()
}
