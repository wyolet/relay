package catalog

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

func flip(p *bool) *bool {
	next := p != nil && !*p
	return &next
}

// toggled flips row's enabled flag in place, as its store row would change, and applies a copy.
func toggled[T any](row *T, enabled func(*T) **bool, apply func(*T) error) func() error {
	return func() error {
		e := enabled(row)
		*e = flip(*e)
		cp := *row
		return apply(&cp)
	}
}

// revived applies a copy of row with its enabled flag on.
func revived[T any](row *T, enabled func(*T) **bool, apply func(*T) error) func() error {
	return func() error {
		on := true
		*enabled(row) = &on
		cp := *row
		return apply(&cp)
	}
}

// The reconciler leaves no stale state: after every one of a random sequence of writes over the fixture, each of which must be accepted, the ref graph is symmetric and no pricing slot is claimed twice.
func TestProperty_InvariantsHoldUnderRandomEvents(t *testing.T) {
	for _, seed := range []int64{1, 42, 1337, 9999} {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) { runProperty(t, seed, 500) })
	}
}

func runProperty(t *testing.T, seed int64, events int) {
	r := rand.New(rand.NewSource(seed))
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	pr0 := pricingFor("p0", hosts[0].Meta.ID, models[0].Meta.ID)
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr0}, bnds)
	must(t, c.Reload(t.Context()))

	// A policy owned by a project the snapshot does not hold is dropped: build and reconcile must reach the same fixpoint.
	orphanPol := *pols[0]
	orphanPol.Meta = meta.Metadata{ID: meta.NewID(), Name: "orphan-project-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}}
	extraKey := *rks[0]
	extraKey.Meta.ID, extraKey.Meta.Name, extraKey.Spec.KeyHash = meta.NewID(), "second-key", hex64("b")

	all := []struct {
		name string
		run  func() error
	}{
		{"toggle provider", toggled(provs[0], func(p *provider.Provider) **bool { return &p.Spec.Enabled }, c.ApplyProviderUpsert)},
		{"toggle host", toggled(hosts[0], func(h *host.Host) **bool { return &h.Spec.Enabled }, c.ApplyHostUpsert)},
		{"toggle model 0", toggled(models[0], func(m *model.Model) **bool { return &m.Spec.Enabled }, c.ApplyModelUpsert)},
		{"toggle model 1", toggled(models[1], func(m *model.Model) **bool { return &m.Spec.Enabled }, c.ApplyModelUpsert)},
		{"toggle host key", toggled(keys[0], func(k *hostkey.HostKey) **bool { return &k.Spec.Enabled }, c.ApplyHostKeyUpsert)},
		{"toggle rate limit", toggled(rls[0], func(r *ratelimit.RateLimit) **bool { return &r.Spec.Enabled }, c.ApplyRateLimitUpsert)},
		{"toggle policy", toggled(pols[0], func(p *policy.Policy) **bool { return &p.Spec.Enabled }, c.ApplyPolicyUpsert)},
		{"toggle key", toggled(rks[0], func(k *key.Key) **bool { return &k.Spec.Enabled }, c.ApplyKeyUpsert)},
		{"toggle pricing", toggled(pr0, func(p *pricing.Pricing) **bool { return &p.Spec.Enabled }, c.ApplyPricingUpsert)},
		{"toggle orphan policy", toggled(&orphanPol, func(p *policy.Policy) **bool { return &p.Spec.Enabled }, c.ApplyPolicyUpsert)},
		{"upsert second key", func() error { cp := extraKey; return c.ApplyKeyUpsert(&cp) }},
		{"delete model", func() error { return c.ApplyModelDelete(models[0].Meta.ID) }},
		{"delete host key", func() error { return c.ApplyHostKeyDelete(keys[0].Meta.ID) }},
		{"delete pricing", func() error { return c.ApplyPricingDelete(pr0.Meta.ID) }},
		{"revive model", revived(models[0], func(m *model.Model) **bool { return &m.Spec.Enabled }, c.ApplyModelUpsert)},
		{"revive host key", revived(keys[0], func(k *hostkey.HostKey) **bool { return &k.Spec.Enabled }, c.ApplyHostKeyUpsert)},
		{"revive pricing", revived(pr0, func(p *pricing.Pricing) **bool { return &p.Spec.Enabled }, c.ApplyPricingUpsert)},
	}
	for i := range events {
		ev := all[r.Intn(len(all))]
		if err := ev.run(); err != nil {
			t.Fatalf("step %d: %s: %v", i, ev.name, err)
		}
		assertRefsSymmetric(t, c.Current(), fmt.Sprintf("step %d (%s)", i, ev.name))
		if _, ok := c.Current().Policy(orphanPol.Meta.ID); ok {
			t.Fatalf("step %d: a policy of a project the snapshot does not hold was kept", i)
		}
		if t.Failed() {
			return
		}
	}
}
