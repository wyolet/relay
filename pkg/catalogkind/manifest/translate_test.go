package manifest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

var idx = MapResolver{
	Providers:  map[string]string{"acme": "p-1"},
	Hosts:      map[string]string{"acme-api": "h-1"},
	Models:     map[string]string{"m": "m-1"},
	RateLimits: map[string]string{"tier-rl": "rl-1"},
	Pricings:   map[string]string{"m-price": "pr-1"},
}

func TestToPolicyCatalogLinks(t *testing.T) {
	d := PolicyDTO{
		Metadata: WireMeta{Name: "tier", Owner: WireOwner{Kind: meta.OwnerHost, Name: "acme-api"}},
		Spec:     PolicySpec{Models: []string{"acme/m"}, RateLimits: []PolicyRateLimitDTO{{Ref: "tier-rl"}}},
	}
	p, err := ToPolicy(d, idx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Meta.Owner.ID != "h-1" || len(p.Spec.RateLimits) != 1 || p.Spec.RateLimits[0].RateLimitID != "rl-1" {
		t.Fatalf("policy = %+v", p)
	}
	if p.Spec.PoolIDs != nil || p.Spec.Budgets != nil {
		t.Fatalf("catalog policy gained links: %+v", p.Spec)
	}
}

func TestToPolicyRefusesPoolsWithoutLinks(t *testing.T) {
	for _, spec := range []PolicySpec{{Pools: []string{"p"}}, {Budgets: []PolicyBudgetDTO{{Ref: "b"}}}} {
		_, err := ToPolicy(PolicyDTO{Metadata: WireMeta{Name: "x"}, Spec: spec}, idx)
		if err == nil || !strings.Contains(err.Error(), "not catalog links") {
			t.Fatalf("spec %+v: err = %v", spec, err)
		}
	}
}

func TestToPolicyWithLinks(t *testing.T) {
	d := PolicyDTO{Metadata: WireMeta{Name: "x"}, Spec: PolicySpec{Pools: []string{"p"}}}
	p, err := ToPolicyWith(d, idx, func(PolicyDTO) ([]string, []policy.BudgetLink, error) {
		return []string{"pool-1"}, []policy.BudgetLink{{BudgetID: "b-1"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Spec.PoolIDs, []string{"pool-1"}) || p.Spec.Budgets[0].BudgetID != "b-1" || !p.Spec.Budgets[0].IsShared() {
		t.Fatalf("spec = %+v", p.Spec)
	}
	boom := errors.New("boom")
	if _, err := ToPolicyWith(d, idx, func(PolicyDTO) ([]string, []policy.BudgetLink, error) { return nil, nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestToHostBindingRefNotFound(t *testing.T) {
	_, err := ToHostBinding(HostBindingDTO{Metadata: WireMeta{Name: "b"}, Spec: HostBindingSpec{Model: "m", Host: "nope"}}, idx)
	var nf *RefNotFoundError
	if !errors.As(err, &nf) || err.Error() != `hostbinding "b": host "nope" not found` {
		t.Fatalf("err = %v", err)
	}
	b, err := ToHostBinding(HostBindingDTO{Metadata: WireMeta{Name: "b"}, Spec: HostBindingSpec{Model: "m", Host: "acme-api", Pricing: "m-price", Adapter: "openai"}}, idx)
	if err != nil || b.Spec.ModelID != "m-1" || b.Spec.HostID != "h-1" || b.Spec.PricingID != "pr-1" || b.Meta.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("binding = %+v, err = %v", b, err)
	}
}

func TestToRateLimitWindow(t *testing.T) {
	rl, err := ToRateLimit(RateLimitDTO{
		Metadata: WireMeta{Name: "rl", Owner: WireOwner{Kind: meta.OwnerHost, Name: "acme-api"}},
		Spec:     RateLimitSpec{Rules: []RateLimitRule{{Meter: "requests", Amount: 5, Window: "1m", Strategy: "fixed"}}},
	}, idx)
	if err != nil || rl.Meta.Owner.ID != "h-1" || rl.Spec.Rules[0].Window.Duration().String() != "1m0s" {
		t.Fatalf("ratelimit = %+v, err = %v", rl, err)
	}
	if _, err := ToRateLimit(RateLimitDTO{Metadata: WireMeta{Name: "rl"}, Spec: RateLimitSpec{Rules: []RateLimitRule{{}}}}, idx); err == nil {
		t.Fatal("missing window accepted")
	}
}
