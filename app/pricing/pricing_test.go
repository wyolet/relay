package pricing

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func valid() *Pricing {
	return &Pricing{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "openai-gpt4-tier",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()},
		},
		Spec: Spec{
			Currency:       "USD",
			TargetModelIDs: []string{meta.NewID()},
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 2.5},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 10},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 6, AboveTokens: 200_000},
			},
		},
	}
}

func TestValidate_Ok(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

func TestValidate_OwnerMustBeHost(t *testing.T) {
	p := valid()
	p.Meta.Owner.Kind = meta.OwnerUser
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "owner.kind must be host") {
		t.Fatalf("got %v", err)
	}
}

func TestValidate_DuplicateRate(t *testing.T) {
	p := valid()
	p.Spec.Rates = append(p.Spec.Rates, Rate{
		Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 99,
	})
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("got %v", err)
	}
}

func TestRateFor_PicksTier(t *testing.T) {
	p := valid()
	r, ok := p.RateFor(MeterTokensInput, 50_000)
	if !ok || r.Amount != 2.5 {
		t.Fatalf("base tier: got ok=%v amount=%v", ok, r)
	}
	r, ok = p.RateFor(MeterTokensInput, 250_000)
	if !ok || r.Amount != 6 {
		t.Fatalf("above tier: got ok=%v amount=%v", ok, r)
	}
}

func TestRateFor_MissingMeter(t *testing.T) {
	p := valid()
	if _, ok := p.RateFor(MeterTokensReasoning, 0); ok {
		t.Error("expected ok=false for missing meter")
	}
}

func TestMeterForUsageKey(t *testing.T) {
	cases := map[string]Meter{
		"input":                  MeterTokensInput,
		"output":                 MeterTokensOutput,
		"cache_read":             MeterTokensCacheRead,
		"cache_creation":         MeterTokensCacheCreation,
		"reasoning":              MeterTokensReasoning,
		"audio_input":            MeterTokensAudioInput,
		"audio_output":           MeterTokensAudioOutput,
		"accepted_prediction":    MeterTokensAcceptedPrediction,
		"rejected_prediction":    MeterTokensRejectedPrediction,
		"server_tool_use_input":  MeterTokensServerToolUseInput,
		"server_tool_use_output": MeterTokensServerToolUseOutput,
	}
	for k, want := range cases {
		t.Run(k, func(t *testing.T) {
			got, ok := MeterForUsageKey(k)
			if !ok || got != want {
				t.Fatalf("got (%q, %v), want (%q, true)", got, ok, want)
			}
		})
	}
	if _, ok := MeterForUsageKey("nope"); ok {
		t.Error("expected ok=false for unknown key")
	}
}
