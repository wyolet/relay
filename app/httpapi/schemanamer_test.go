package httpapi

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/settings"
)

// Re-declared here with the same name as control.listBody so the
// generic-instantiation path in schemaNamer matches. Test only.
type listBody[T any] struct {
	Items []*T `json:"items"`
}

// Same shape as control.sectionEnvelope — the namer matches on the
// base name string, so this exercise reproduces the live wrapper.
type sectionEnvelope[T any] struct {
	Section settings.SectionName `json:"section"`
	Value   T                    `json:"value"`
}

func TestSchemaNamer_CleanNames(t *testing.T) {
	cases := []struct {
		in   reflect.Type
		want string
	}{
		{reflect.TypeOf(policy.Policy{}), "Policy"},
		{reflect.TypeOf(policy.Spec{}), "PolicySpec"},
		{reflect.TypeOf(ratelimit.RateLimit{}), "RateLimit"},
		{reflect.TypeOf(ratelimit.Spec{}), "RateLimitSpec"},
		{reflect.TypeOf(ratelimit.Rule{}), "RateLimitRule"},
		{reflect.TypeOf(pricing.Spec{}), "PricingSpec"},
		{reflect.TypeOf(pricing.Rate{}), "PricingRate"},
		{reflect.TypeOf(meta.Metadata{}), "Metadata"},
		{reflect.TypeOf(meta.Owner{}), "Owner"},
		{reflect.TypeOf(listBody[policy.Policy]{}), "PolicyList"},
		{reflect.TypeOf(listBody[pricing.Pricing]{}), "PricingList"},
		{reflect.TypeOf(sectionEnvelope[settings.ProxyMode]{}), "ProxyModeEnvelope"},
	}
	for _, tc := range cases {
		if got := schemaNamer(tc.in, ""); got != tc.want {
			t.Errorf("schemaNamer(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
