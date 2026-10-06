package pricing

import (
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

func perMillion(m Meter, amount float64) Rate {
	return Rate{Meter: m, Unit: UnitPerMillion, Amount: amount}
}

// Rates in USD per million, chosen so every meter's nanos per token is distinct: input 1000, output 10000, part 3000.
func TestCostNanos_PartCountedOnce(t *testing.T) {
	tests := []struct {
		part, whole Meter
		partKey     string
		wholeKey    string
	}{
		{MeterTokensReasoning, MeterTokensOutput, "reasoning", "output"},
		{MeterTokensAudioOutput, MeterTokensOutput, "audio_output", "output"},
		{MeterTokensAcceptedPrediction, MeterTokensOutput, "accepted_prediction", "output"},
		{MeterTokensRejectedPrediction, MeterTokensOutput, "rejected_prediction", "output"},
		{MeterTokensAudioInput, MeterTokensInput, "audio_input", "input"},
	}
	for _, tc := range tests {
		other := MeterTokensInput
		otherKey := "input"
		if tc.whole == MeterTokensInput {
			other, otherKey = MeterTokensOutput, "output"
		}
		tokens := usage.Tokens{tc.wholeKey: 1000, tc.partKey: 400, otherKey: 100}
		otherNanos := map[Meter]int64{MeterTokensInput: 1000, MeterTokensOutput: 10_000}[other] * 100
		wholeRate := map[Meter]float64{MeterTokensInput: 1, MeterTokensOutput: 10}[tc.whole]
		base := []Rate{perMillion(MeterTokensInput, 1), perMillion(MeterTokensOutput, 10)}

		t.Run(tc.partKey+" with its own rate", func(t *testing.T) {
			p := &Pricing{Spec: Spec{Rates: append(base, perMillion(tc.part, 3))}}
			total, breakdown, ok := p.CostNanos(tokens)
			wantWhole := int64(600 * wholeRate * 1000)
			if !ok || breakdown[string(tc.whole)] != wantWhole || breakdown[string(tc.part)] != 1_200_000 {
				t.Fatalf("breakdown = %v, want %s %d and %s 1200000", breakdown, tc.whole, wantWhole, tc.part)
			}
			if want := wantWhole + 1_200_000 + otherNanos; total != want {
				t.Errorf("total = %d, want %d", total, want)
			}
		})
		t.Run(tc.partKey+" without a rate stays in "+tc.wholeKey, func(t *testing.T) {
			p := &Pricing{Spec: Spec{Rates: base}}
			total, breakdown, _ := p.CostNanos(tokens)
			wantWhole := int64(1000 * wholeRate * 1000)
			if breakdown[string(tc.whole)] != wantWhole {
				t.Fatalf("breakdown = %v, want %s %d", breakdown, tc.whole, wantWhole)
			}
			if _, priced := breakdown[string(tc.part)]; priced {
				t.Errorf("unrated part priced: %v", breakdown)
			}
			if want := wantWhole + otherNanos; total != want {
				t.Errorf("total = %d, want %d", total, want)
			}
		})
	}
}

// Cache and server-tool keys are counted apart from input and output, so their rates never reduce them.
func TestCostNanos_DisjointKeysNotDeducted(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensInput, 1), perMillion(MeterTokensOutput, 10),
		perMillion(MeterTokensCacheRead, 0.1), perMillion(MeterTokensCacheCreation, 1.25),
		perMillion(MeterTokensServerToolUseInput, 2), perMillion(MeterTokensServerToolUseOutput, 20),
	}}}
	_, breakdown, _ := p.CostNanos(usage.Tokens{
		"input": 1000, "output": 1000, "cache_read": 500, "cache_creation": 500,
		"server_tool_use_input": 300, "server_tool_use_output": 300,
	})
	if breakdown[string(MeterTokensInput)] != 1_000_000 || breakdown[string(MeterTokensOutput)] != 10_000_000 {
		t.Errorf("breakdown = %v, want input 1000000 output 10000000", breakdown)
	}
}

func TestCostNanos_SeveralRatedPartsOfOneWhole(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensOutput, 10), perMillion(MeterTokensReasoning, 3), perMillion(MeterTokensAudioOutput, 20),
	}}}
	total, breakdown, _ := p.CostNanos(usage.Tokens{"output": 1000, "reasoning": 300, "audio_output": 200})
	// 500 text tokens at output, 300 at reasoning, 200 at audio output.
	if breakdown[string(MeterTokensOutput)] != 5_000_000 || total != 5_000_000+900_000+4_000_000 {
		t.Errorf("total %d breakdown %v", total, breakdown)
	}
}

// The tier is chosen by the stored input count; a rated part of input lowers what input bills, not the tier.
func TestCostNanos_TierUsesStoredInput(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensInput, 1), {Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 2, AboveTokens: 200_000},
		perMillion(MeterTokensAudioInput, 5),
	}}}
	_, breakdown, _ := p.CostNanos(usage.Tokens{"input": 250_000, "audio_input": 100_000})
	if breakdown[string(MeterTokensInput)] != 150_000*2000 {
		t.Errorf("input = %d, want 150000 tokens at the upper tier", breakdown[string(MeterTokensInput)])
	}
}

// A part rated only from a higher tier stays in its whole below that tier.
func TestCostNanos_PartRatedOnlyAboveTier(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensInput, 1), perMillion(MeterTokensOutput, 10),
		{Meter: MeterTokensReasoning, Unit: UnitPerMillion, Amount: 3, AboveTokens: 200_000},
	}}}
	_, breakdown, _ := p.CostNanos(usage.Tokens{"input": 1000, "output": 1000, "reasoning": 400})
	if breakdown[string(MeterTokensOutput)] != 10_000_000 {
		t.Errorf("breakdown = %v, want all 1000 output tokens at output", breakdown)
	}
}

// The qwen-plus sheet on the alibaba host in the public catalog: input 0.4, output 1.2, reasoning 4 USD per million. Its upstream counts reasoning inside completion_tokens.
func TestCostNanos_ReasoningRateSheet(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensInput, 0.4), perMillion(MeterTokensOutput, 1.2), perMillion(MeterTokensReasoning, 4),
	}}}
	tokens := usage.Tokens{"input": 1000, "output": 3000, "reasoning": 2500}

	// Charging every stored count at its own rate, as before: 1000×400 + 3000×1200 + 2500×4000.
	var everyKey int64
	for key, count := range tokens {
		m, _ := MeterForUsageKey(key)
		r, _ := p.RateFor(m, 0)
		everyKey += perMillionNanos(count, rateNanos(r.Amount))
	}
	if everyKey != 14_000_000 {
		t.Fatalf("every-key sum = %d, want 14000000", everyKey)
	}

	// Each token once: 1000×400 + 500×1200 + 2500×4000.
	total, breakdown, _ := p.CostNanos(tokens)
	if total != 11_000_000 || breakdown[string(MeterTokensOutput)] != 600_000 || breakdown[string(MeterTokensReasoning)] != 10_000_000 {
		t.Errorf("total %d breakdown %v, want 11000000 with output 600000 reasoning 10000000", total, breakdown)
	}
	if got := p.Cost(tokens); got != 0.011 {
		t.Errorf("Cost = %v, want 0.011", got)
	}
}

func TestCostNanos_Allocations(t *testing.T) {
	p := &Pricing{Spec: Spec{Rates: []Rate{
		perMillion(MeterTokensInput, 1), perMillion(MeterTokensOutput, 10),
		perMillion(MeterTokensReasoning, 3), perMillion(MeterTokensAudioInput, 5),
	}}}
	tokens := usage.Tokens{"input": 1000, "output": 1000, "reasoning": 300, "audio_input": 100}
	plain := usage.Tokens{"input": 1000, "output": 1000, "cache_read": 300, "cache_creation": 100}
	withParts := testing.AllocsPerRun(100, func() { _, _, _ = p.CostNanos(tokens) })
	withoutParts := testing.AllocsPerRun(100, func() { _, _, _ = p.CostNanos(plain) })
	if withParts > withoutParts {
		t.Errorf("allocations: %v with rated parts, %v without", withParts, withoutParts)
	}
}
