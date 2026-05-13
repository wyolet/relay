package ratelimit

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string, owner meta.OwnerKind) *RateLimit {
	rl := &RateLimit{
		Meta: meta.Metadata{Name: name, Owner: meta.Owner{Kind: owner}},
		Spec: Spec{Rules: []Rule{{
			Meter:    MeterRequests,
			Amount:   100,
			Window:   time.Minute,
			Strategy: StrategyTokenBucket,
		}}},
	}
	if owner == meta.OwnerProvider {
		rl.Meta.Owner.ID = meta.NewID()
	}
	return rl
}

func TestValidate(t *testing.T) {
	t.Run("ok user-owned", func(t *testing.T) {
		if err := fix("rpm", meta.OwnerUser).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("ok system-owned", func(t *testing.T) {
		if err := fix("inference-api", meta.OwnerSystem).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("ok provider-owned", func(t *testing.T) {
		if err := fix("upstream-tier-1", meta.OwnerProvider).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		rl   *RateLimit
		want string
	}{
		{
			name: "missing name",
			rl:   func() *RateLimit { r := fix("x", meta.OwnerUser); r.Meta.Name = ""; return r }(),
			want: "Name",
		},
		{
			name: "empty rules",
			rl:   func() *RateLimit { r := fix("x", meta.OwnerUser); r.Spec.Rules = nil; return r }(),
			want: "Rules",
		},
		{
			name: "rule meter unknown",
			rl: func() *RateLimit {
				r := fix("x", meta.OwnerUser)
				r.Spec.Rules[0].Meter = "bogus"
				return r
			}(),
			want: "Meter",
		},
		{
			name: "rule amount zero",
			rl: func() *RateLimit {
				r := fix("x", meta.OwnerUser)
				r.Spec.Rules[0].Amount = 0
				return r
			}(),
			want: "Amount",
		},
		{
			name: "rule window zero",
			rl: func() *RateLimit {
				r := fix("x", meta.OwnerUser)
				r.Spec.Rules[0].Window = 0
				return r
			}(),
			want: "Window",
		},
		{
			name: "rule strategy unknown",
			rl: func() *RateLimit {
				r := fix("x", meta.OwnerUser)
				r.Spec.Rules[0].Strategy = "bogus"
				return r
			}(),
			want: "Strategy",
		},
		{
			name: "owner missing",
			rl:   func() *RateLimit { r := fix("x", meta.OwnerUser); r.Meta.Owner.Kind = ""; return r }(),
			want: "owner.kind required",
		},
		{
			name: "provider owner missing id",
			rl: func() *RateLimit {
				r := fix("x", meta.OwnerProvider)
				r.Meta.Owner.ID = ""
				return r
			}(),
			want: "owner.id is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rl.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}
