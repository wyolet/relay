package openai

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason, read in one pass that reads of the output only each item's type and a message's part types.
func (ResponsesTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	if sum, ok := responsesScanSummary(body); ok {
		return sum, nil
	}
	return responsesUnmarshalSummary(body)
}

// responsesScanSummary reads the members responsesUnmarshalSummary decodes and yields its Summary; false when the pass cannot settle them the same way (a syntax error, a mistyped member, an escaped key, or a repeated member encoding/json would merge into), and the body is decoded instead.
func responsesScanSummary(body []byte) (v1.Summary, bool) {
	r := jsonscan.Read(body)
	var (
		status, tier, reason               []byte
		u                                  ResponsesUsage
		calls, refused                     bool
		seenOutput, seenUsage, seenDetails bool
	)
	ok := r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "status"):
			return r.StringField(&status)
		case jsonscan.KeyIs(key, "service_tier"):
			return r.StringField(&tier)
		case jsonscan.KeyIs(key, "output"):
			first := !seenOutput
			seenOutput = true
			return first && r.SliceField(func(int) bool {
				return responsesScanItem(&r, &calls, &refused)
			})
		case jsonscan.KeyIs(key, "usage"):
			first := !seenUsage
			seenUsage = true
			return first && responsesScanUsage(&r, &u)
		case jsonscan.KeyIs(key, "incomplete_details"):
			first := !seenDetails
			seenDetails = true
			return first && r.StructField(func(k []byte) bool {
				return !jsonscan.KeyIs(k, "reason") || r.StringField(&reason)
			})
		}
		return true
	})
	if !ok || !r.End() {
		return v1.Summary{}, false
	}
	// Null usage or incomplete_details reads as zero, which the mappings treat as nil.
	var incomplete ResponsesIncompleteDetails
	incomplete.Reason, _ = jsonscan.String(reason)
	st, _ := jsonscan.String(status)
	sum := v1.Summary{Tokens: responsesUsageToCanonical(&u), FinishReason: responsesFinishReason(ResponsesStatus(st), &incomplete, calls, refused)}
	sum.ServiceTier, _ = jsonscan.String(tier)
	return sum, true
}

// responsesScanItem reads one output item as responsesItemFinish types it: its last non-null type, and its last content, which counts only on a message.
func responsesScanItem(r *jsonscan.Reader, calls, refused *bool) bool {
	var typ []byte
	var refusal, contentOK bool
	ok := r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "type"):
			return r.StringField(&typ)
		case jsonscan.KeyIs(key, "content"):
			refusal, contentOK = responsesScanContent(r)
		}
		return true
	})
	switch {
	case !ok:
		return false
	case jsonscan.StringDecodesTo(typ, string(ResponsesItemTypeFunctionCall)), jsonscan.StringDecodesTo(typ, string(ResponsesItemTypeCustomToolCall)):
		*calls = true
	case jsonscan.StringDecodesTo(typ, string(ResponsesItemTypeMessage)):
		*refused = *refused || refusal
		return contentOK
	}
	return true
}

// responsesScanContent reads an item's content for a refusal part; not ok where responsesItemFinish fails on a message: content neither a string, null nor an array of null or object parts, or a part type not a string.
func responsesScanContent(r *jsonscan.Reader) (refusal, ok bool) {
	ok = true
	isArray := r.Array(func(int) {
		var typ []byte
		ok = ok && r.StructField(func(key []byte) bool {
			return !jsonscan.KeyIs(key, "type") || r.StringField(&typ)
		})
		refusal = refusal || jsonscan.StringDecodesTo(typ, string(ResponsesPartTypeRefusal))
	})
	if isArray {
		return refusal, ok
	}
	v := r.Value()
	return false, jsonscan.IsNull(v) || (len(v) > 0 && v[0] == '"')
}

// responsesScanUsage reads usage as encoding/json decodes it into a *ResponsesUsage: null leaves zero counters, which responsesUsageToCanonical reads as it does nil.
func responsesScanUsage(r *jsonscan.Reader, u *ResponsesUsage) bool {
	var seenInput, seenOutput bool
	return r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "input_tokens"):
			return r.IntField(&u.InputTokens)
		case jsonscan.KeyIs(key, "output_tokens"):
			return r.IntField(&u.OutputTokens)
		case jsonscan.KeyIs(key, "total_tokens"):
			return r.IntField(&u.TotalTokens)
		case jsonscan.KeyIs(key, "input_tokens_details"):
			first := !seenInput
			seenInput = true
			return first && r.StructField(func(k []byte) bool {
				return !jsonscan.KeyIs(k, "cached_tokens") || r.IntField(&u.InputTokensDetails.CachedTokens)
			})
		case jsonscan.KeyIs(key, "output_tokens_details"):
			first := !seenOutput
			seenOutput = true
			return first && r.StructField(func(k []byte) bool {
				return !jsonscan.KeyIs(k, "reasoning_tokens") || r.IntField(&u.OutputTokensDetails.ReasoningTokens)
			})
		}
		return true
	})
}

// responsesUnmarshalSummary is DecodeSummary by encoding/json, for the bodies the one-pass read cannot settle.
func responsesUnmarshalSummary(body []byte) (v1.Summary, error) {
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

// responsesItemFinish decodes an output item the way responsesUnmarshalItem types it: a function or custom tool call, or a message holding a refusal part. It fails where responsesUnmarshalItem does: string or absent content has no parts, and a part of any type decodes (an unmodeled one as a ResponsesRawPart).
func responsesItemFinish(item []byte) (call, refusal bool, err error) {
	var v struct {
		Type    ResponsesItemType `json:"type"`
		Content responsesItemSpan `json:"content"`
	}
	if err := json.Unmarshal(item, &v); err != nil {
		return false, false, err
	}
	switch v.Type {
	case ResponsesItemTypeFunctionCall, ResponsesItemTypeCustomToolCall:
		return true, false, nil
	case ResponsesItemTypeMessage:
		if len(v.Content) == 0 || v.Content[0] == '"' {
			return false, false, nil
		}
		var parts []struct {
			Type ResponsesPartType `json:"type"`
		}
		if err := json.Unmarshal(v.Content, &parts); err != nil {
			return false, false, err
		}
		for _, p := range parts {
			refusal = refusal || p.Type == ResponsesPartTypeRefusal
		}
		return false, refusal, nil
	}
	return false, false, nil
}
