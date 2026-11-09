package openai

import (
	"encoding/json"
	"slices"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// referenceResponsesSummary is the Responses DecodeSummary the one-pass read replaced, kept as the reference it must match: an outer encoding/json decode, then a lenient scan of each output item.
func referenceResponsesSummary(body []byte) (v1.Summary, error) {
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
		call, refusal, err := referenceItemFinish(item)
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

func referenceItemFinish(item []byte) (call, refusal bool, err error) {
	typ, content, ok := referenceItemMembers(item)
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
		refusal, err = referenceContentRefuses(content)
		return false, refusal, err
	}
	return false, false, nil
}

func referenceItemMembers(item []byte) (typ ResponsesItemType, content []byte, ok bool) {
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

func referenceContentRefuses(content []byte) (bool, error) {
	if len(content) == 0 || content[0] == '"' {
		return false, nil
	}
	types, ok := referencePartTypes(content)
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

func referencePartTypes(content []byte) ([]ResponsesPartType, bool) {
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
