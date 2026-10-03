package openai

import (
	"github.com/wyolet/relay/sdk/usage"
)

// ccUsageToCanonical maps CC's Usage block to the canonical
// orthogonal-meter Tokens map.
//
// OpenAI's prompt_tokens INCLUDES cached tokens; canonical "input"
// means non-cached input only (consistent with Anthropic semantics).
// We subtract cached_tokens from prompt_tokens so input + cache_read
// reconstructs the prompt total. Note the output side is NOT fully
// orthogonal: reasoning/audio_output/predictions are sub-breakdowns of
// completion, so Tokens.Sum() over this map double-counts them. The
// honest request total is input + cache_read + output (== prompt +
// completion), never Sum() — see canonicalUsageToCC.
func ccUsageToCanonical(u *Usage) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	cached := int64(0)
	if u.PromptDetails != nil {
		cached = int64(u.PromptDetails.CachedTokens)
	}
	if v := int64(u.PromptTokens) - cached; v > 0 {
		t["input"] = v
	}
	if u.CompletionTokens > 0 {
		t["output"] = int64(u.CompletionTokens)
	}
	if cached > 0 {
		t["cache_read"] = cached
	}
	if u.PromptDetails != nil && u.PromptDetails.AudioTokens > 0 {
		t["audio_input"] = int64(u.PromptDetails.AudioTokens)
	}
	if u.CompletionDetails != nil {
		if u.CompletionDetails.ReasoningTokens > 0 {
			t["reasoning"] = int64(u.CompletionDetails.ReasoningTokens)
		}
		if u.CompletionDetails.AudioTokens > 0 {
			t["audio_output"] = int64(u.CompletionDetails.AudioTokens)
		}
		if u.CompletionDetails.AcceptedPredictionTokens > 0 {
			t["accepted_prediction"] = int64(u.CompletionDetails.AcceptedPredictionTokens)
		}
		if u.CompletionDetails.RejectedPredictionTokens > 0 {
			t["rejected_prediction"] = int64(u.CompletionDetails.RejectedPredictionTokens)
		}
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// canonicalUsageToCC maps a canonical orthogonal-meter map back to
// CC's Usage block. prompt_tokens is reconstructed as input +
// cache_read (CC's convention). total_tokens is prompt + completion —
// NOT Tokens.Sum(): reasoning/audio are sub-breakdowns already inside
// completion, so summing the whole map double-counts them (OpenAI's own
// total_tokens is just input_tokens + output_tokens).
func canonicalUsageToCC(t usage.Tokens) *Usage {
	if len(t) == 0 {
		return nil
	}
	cached := int(t["cache_read"])
	prompt := int(t["input"]) + cached
	completion := int(t["output"])
	cu := &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	if cached > 0 {
		cu.PromptDetails = &PromptTokenDetails{CachedTokens: cached}
	}
	if r := int(t["reasoning"]); r > 0 {
		cu.CompletionDetails = &CompletionTokenDetails{ReasoningTokens: r}
	}
	return cu
}
