package anthropic

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason, the structured-output tool block's override included, read in one pass that skips content.
func (AnthropicTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	if sum, ok := anthropicScanSummary(body); ok {
		return sum, nil
	}
	return anthropicUnmarshalSummary(body)
}

// anthropicScanSummary reads the members anthropicUnmarshalSummary decodes and yields its Summary; false when the pass cannot settle them the same way (a syntax error, a mistyped member, an escaped key, or a repeated member encoding/json would merge into), and the body is decoded instead.
func anthropicScanSummary(body []byte) (v1.Summary, bool) {
	r := jsonscan.Read(body)
	var (
		stop, tier             []byte
		u                      anthropicFullUsage
		structured             bool
		seenContent, seenUsage bool
	)
	ok := r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "content"):
			first := !seenContent
			seenContent = true
			return first && r.SliceField(func(int) bool {
				return anthropicScanBlock(&r, &structured)
			})
		case jsonscan.KeyIs(key, "stop_reason"):
			return r.StringField(&stop)
		case jsonscan.KeyIs(key, "usage"):
			first := !seenUsage
			seenUsage = true
			return first && anthropicScanUsage(&r, &u, &tier)
		}
		return true
	})
	if !ok || !r.End() {
		return v1.Summary{}, false
	}
	reason, _ := jsonscan.String(stop)
	_, finish, _ := anthropicStopReasonToCanonical(reason)
	if structured {
		finish = v1.FinishReasonStop
	}
	u.ServiceTier, _ = jsonscan.String(tier)
	return v1.Summary{Tokens: anthropicUsageToCanonical(&u), FinishReason: finish, ServiceTier: u.ServiceTier}, true
}

// anthropicScanBlock reads one content block's type and name, reporting the structured-output tool block.
func anthropicScanBlock(r *jsonscan.Reader, structured *bool) bool {
	var typ, name []byte
	ok := r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "type"):
			return r.StringField(&typ)
		case jsonscan.KeyIs(key, "name"):
			return r.StringField(&name)
		}
		return true
	})
	if jsonscan.StringDecodesTo(typ, "tool_use") && jsonscan.StringDecodesTo(name, structuredOutputToolName) {
		*structured = true
	}
	return ok
}

// anthropicScanUsage reads usage as encoding/json decodes it into an anthropicFullUsage, leaving the service tier raw in tier.
func anthropicScanUsage(r *jsonscan.Reader, u *anthropicFullUsage, tier *[]byte) bool {
	seenCacheCreation := false
	return r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "input_tokens"):
			return r.IntField(&u.InputTokens)
		case jsonscan.KeyIs(key, "output_tokens"):
			return r.IntField(&u.OutputTokens)
		case jsonscan.KeyIs(key, "cache_creation_input_tokens"):
			return r.IntField(&u.CacheCreationInputTokens)
		case jsonscan.KeyIs(key, "cache_read_input_tokens"):
			return r.IntField(&u.CacheReadInputTokens)
		case jsonscan.KeyIs(key, "service_tier"):
			return r.StringField(tier)
		case jsonscan.KeyIs(key, "cache_creation"):
			first := !seenCacheCreation
			seenCacheCreation = true
			return first && r.StructField(func(k []byte) bool {
				if jsonscan.KeyIs(k, "ephemeral_1h_input_tokens") {
					return r.IntField(&u.CacheCreationByTTL.Ephemeral1hInputTokens)
				}
				return true
			})
		}
		return true
	})
}

// anthropicUnmarshalSummary is DecodeSummary by encoding/json, for the bodies the one-pass read cannot settle.
func anthropicUnmarshalSummary(body []byte) (v1.Summary, error) {
	var ar struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		} `json:"content"`
		StopReason string             `json:"stop_reason"`
		Usage      anthropicFullUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return v1.Summary{}, err
	}
	_, finish, _ := anthropicStopReasonToCanonical(ar.StopReason)
	for _, b := range ar.Content {
		if b.Type == "tool_use" && b.Name == structuredOutputToolName {
			finish = v1.FinishReasonStop
		}
	}
	return v1.Summary{Tokens: anthropicUsageToCanonical(&ar.Usage), FinishReason: finish, ServiceTier: ar.Usage.ServiceTier}, nil
}
