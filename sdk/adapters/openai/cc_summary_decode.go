package openai

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason (choices[0]'s, lifted to refusal by its message's refusal), read in one pass that skips the messages' content.
func (CCTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	if sum, ok := ccScanSummary(body); ok {
		return sum, nil
	}
	return ccUnmarshalSummary(body)
}

// ccScanSummary reads the members ccUnmarshalSummary decodes and yields its Summary; false when the pass cannot settle them the same way (a syntax error, a mistyped member, an escaped key, or a repeated member encoding/json would merge into), and the body is decoded instead.
func ccScanSummary(body []byte) (v1.Summary, bool) {
	r := jsonscan.Read(body)
	var (
		tier, finish           []byte
		refused                bool
		choices                int
		u                      Usage
		prompt                 PromptTokenDetails
		completion             CompletionTokenDetails
		seenUsage, seenChoices bool
	)
	ok := r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "service_tier"):
			return r.StringField(&tier)
		case jsonscan.KeyIs(key, "usage"):
			first := !seenUsage
			seenUsage = true
			return first && ccScanUsage(&r, &u, &prompt, &completion)
		case jsonscan.KeyIs(key, "choices"):
			first := !seenChoices
			seenChoices = true
			return first && r.SliceField(func(i int) bool {
				choices = i + 1
				if i == 0 {
					return ccScanChoice(&r, &finish, &refused)
				}
				var f []byte
				var ref bool
				return ccScanChoice(&r, &f, &ref)
			})
		}
		return true
	})
	if !ok || !r.End() {
		return v1.Summary{}, false
	}
	// Null usage and usage without counters both yield nil tokens, as a nil *Usage does.
	u.PromptDetails, u.CompletionDetails = &prompt, &completion
	sum := v1.Summary{Tokens: ccUsageToCanonical(&u), FinishReason: v1.FinishReasonStop}
	sum.ServiceTier, _ = jsonscan.String(tier)
	if choices > 0 {
		reason, _ := jsonscan.String(finish)
		_, sum.FinishReason, _ = ccFinishReasonToCanonical(reason)
		if refused && sum.FinishReason == v1.FinishReasonStop {
			sum.FinishReason = v1.FinishReasonRefusal
		}
	}
	return sum, true
}

// ccScanChoice reads one choice's finish reason and whether its message holds a non-empty refusal.
func ccScanChoice(r *jsonscan.Reader, finish *[]byte, refused *bool) bool {
	seenMessage := false
	return r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "finish_reason"):
			return r.StringField(finish)
		case jsonscan.KeyIs(key, "message"):
			first := !seenMessage
			seenMessage = true
			return first && r.StructField(func(k []byte) bool {
				if !jsonscan.KeyIs(k, "refusal") {
					return true
				}
				// A *string: null clears it, and any string longer than "" decodes to a non-empty one.
				switch v := r.Value(); {
				case jsonscan.IsNull(v):
					*refused = false
				case len(v) > 0 && v[0] == '"':
					*refused = len(v) > len(`""`)
				default:
					return false
				}
				return true
			})
		}
		return true
	})
}

// ccScanUsage reads usage as encoding/json decodes it into a *Usage, its details into prompt and completion: null usage or details leave zero counters, which ccUsageToCanonical reads as it does nil.
func ccScanUsage(r *jsonscan.Reader, u *Usage, prompt *PromptTokenDetails, completion *CompletionTokenDetails) bool {
	var seenPrompt, seenCompletion bool
	return r.StructField(func(key []byte) bool {
		switch {
		case jsonscan.KeyIs(key, "prompt_tokens"):
			return r.IntField(&u.PromptTokens)
		case jsonscan.KeyIs(key, "completion_tokens"):
			return r.IntField(&u.CompletionTokens)
		case jsonscan.KeyIs(key, "total_tokens"):
			return r.IntField(&u.TotalTokens)
		case jsonscan.KeyIs(key, "prompt_tokens_details"):
			first := !seenPrompt
			seenPrompt = true
			return first && r.StructField(func(k []byte) bool {
				switch {
				case jsonscan.KeyIs(k, "cached_tokens"):
					return r.IntField(&prompt.CachedTokens)
				case jsonscan.KeyIs(k, "audio_tokens"):
					return r.IntField(&prompt.AudioTokens)
				}
				return true
			})
		case jsonscan.KeyIs(key, "completion_tokens_details"):
			first := !seenCompletion
			seenCompletion = true
			return first && r.StructField(func(k []byte) bool {
				switch {
				case jsonscan.KeyIs(k, "reasoning_tokens"):
					return r.IntField(&completion.ReasoningTokens)
				case jsonscan.KeyIs(k, "audio_tokens"):
					return r.IntField(&completion.AudioTokens)
				case jsonscan.KeyIs(k, "accepted_prediction_tokens"):
					return r.IntField(&completion.AcceptedPredictionTokens)
				case jsonscan.KeyIs(k, "rejected_prediction_tokens"):
					return r.IntField(&completion.RejectedPredictionTokens)
				}
				return true
			})
		}
		return true
	})
}

// ccUnmarshalSummary is DecodeSummary by encoding/json, for the bodies the one-pass read cannot settle.
func ccUnmarshalSummary(body []byte) (v1.Summary, error) {
	var cc struct {
		ServiceTier string `json:"service_tier,omitempty"`
		Choices     []struct {
			Message struct {
				Refusal *string `json:"refusal,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &cc); err != nil {
		return v1.Summary{}, err
	}
	sum := v1.Summary{Tokens: ccUsageToCanonical(cc.Usage), FinishReason: v1.FinishReasonStop, ServiceTier: cc.ServiceTier}
	if len(cc.Choices) > 0 {
		ch := &cc.Choices[0]
		_, sum.FinishReason, _ = ccFinishReasonToCanonical(ch.FinishReason)
		if ch.Message.Refusal != nil && *ch.Message.Refusal != "" && sum.FinishReason == v1.FinishReasonStop {
			sum.FinishReason = v1.FinishReasonRefusal
		}
	}
	return sum, nil
}
