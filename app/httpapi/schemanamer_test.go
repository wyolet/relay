package httpapi

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/ratelimit"
)

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
	}
	for _, tc := range cases {
		if got := schemaNamer(tc.in, ""); got != tc.want {
			t.Errorf("schemaNamer(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
