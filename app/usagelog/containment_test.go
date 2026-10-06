package usagelog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
)

// A Chat Completions response whose prompt carries audio and whose completion carries reasoning and audio, priced by the qwen3-omni-flash sheet on the alibaba host in the public catalog (input 0.43, output 1.66, audio input 3.81, audio output 15.11 USD per million; no reasoning rate).
func TestBuildEvent_PartsChargedOnce(t *testing.T) {
	sheet := &pricing.Pricing{
		Meta: meta.Metadata{ID: "pr-omni", Name: "alibaba-qwen3-omni-flash"},
		Spec: pricing.Spec{Currency: "USD", Rates: []pricing.Rate{
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 0.43},
			{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 1.66},
			{Meter: pricing.MeterTokensAudioInput, Unit: pricing.UnitPerMillion, Amount: 3.81},
			{Meter: pricing.MeterTokensAudioOutput, Unit: pricing.UnitPerMillion, Amount: 15.11},
		}},
	}
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"qwen3-omni-flash",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1200,"completion_tokens":500,"total_tokens":1700,
			"prompt_tokens_details":{"audio_tokens":200},
			"completion_tokens_details":{"reasoning_tokens":40,"audio_tokens":300}}}`)

	lc := lifecycle.NewContext("req-parts", "pipeline", time.Now())
	lc.PricingID, lc.PricingName = "pr-omni", "alibaba-qwen3-omni-flash"
	lc.Translator = pkgopenai.CCTranslator{}
	ev := buildEvent(lc, 200, "", "", body, testPricer(map[string]*pricing.Pricing{"pr-omni": sheet}))

	// Stored counts keep the provider's meaning: output and input include their parts.
	want := map[string]int64{"input": 1200, "output": 500, "reasoning": 40, "audio_input": 200, "audio_output": 300}
	for k, v := range want {
		if ev.Tokens[k] != v {
			t.Errorf("tokens[%s] = %d, want %d (tokens %v)", k, ev.Tokens[k], v, ev.Tokens)
		}
	}

	// Charged: 1000 text input × 430 + 200 audio input × 3810 + 200 text output (reasoning has no rate, so it stays in output) × 1660 + 300 audio output × 15110. Charging every stored count at its own rate gave 6641000.
	wantBreakdown := map[string]int64{
		"tokens.input":        430_000,
		"tokens.audio_input":  762_000,
		"tokens.output":       332_000,
		"tokens.audio_output": 4_533_000,
	}
	if ev.CostNanos == nil || *ev.CostNanos != 6_057_000 {
		t.Fatalf("cost = %v, want 6057000", ev.CostNanos)
	}
	if len(ev.CostBreakdown) != len(wantBreakdown) {
		t.Errorf("breakdown = %v, want %v", ev.CostBreakdown, wantBreakdown)
	}
	for k, v := range wantBreakdown {
		if ev.CostBreakdown[k] != v {
			t.Errorf("breakdown[%s] = %d, want %d", k, ev.CostBreakdown[k], v)
		}
	}
}
