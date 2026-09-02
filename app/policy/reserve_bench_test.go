package policy

import (
	"context"
	"testing"
	"time"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
)

func benchRules() []appratelimit.Rule {
	return []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1 << 40, Window: appratelimit.Window(time.Hour)},
		{Meter: appratelimit.MeterTokens, Amount: 1 << 40, Window: appratelimit.Window(time.Hour)},
		{Meter: appratelimit.MeterTokensInput, Amount: 1 << 40, Window: appratelimit.Window(time.Hour)},
	}
}

// BenchmarkReserveInboundToken is the per-request inbound reservation for a
// token: rule rendering, key building, and one script call.
func BenchmarkReserveInboundToken(b *testing.B) {
	svc, _, pol := reserveFixture(b, benchRules()...)
	in := InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
		ProviderSlug: "prov", ModelSlug: "model", HostSlug: "host"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.ReserveInbound(ctx, in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReserveInboundKey is the same path without the revocation rule.
func BenchmarkReserveInboundKey(b *testing.B) {
	svc, _, pol := reserveFixture(b, benchRules()...)
	in := InboundInput{Policy: pol, TeamID: "team-1",
		ProviderSlug: "prov", ModelSlug: "model", HostSlug: "host"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.ReserveInbound(ctx, in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolveRules isolates the rule-key rendering the request path
// runs once per rule.
func BenchmarkResolveRules(b *testing.B) {
	pol := fix("prod-policy")
	rl := &appratelimit.RateLimit{}
	rl.Meta.ID, rl.Meta.Name = "rl-1", "rl-1"
	rl.Spec.Rules = benchRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pol.ResolveRules(rl, "model-1")
	}
}
