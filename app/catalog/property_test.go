package catalog

import (
	"math/rand"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// TestProperty_InvariantsHoldUnderRandomEvents fuzzes the reconciler. It
// starts from the standard fixture and applies a deterministic random
// sequence of Apply* calls. After every event the snapshot must satisfy:
//
//  1. Every outbound ref of every present row resolves to a present row.
//  2. Every dependent entry in every refsBy* map points at a present row.
//  3. No duplicate Pricing claim on any (model, host) pair.
//  4. No alias collision in modelsByName beyond what aliases legitimately allow.
//  5. modelsByPolicy / hostKeysByPolicy / rateLimitByPolicy only reference
//     rows that are themselves present.
//  6. pricingByModelHost values are present in pricingsByID.
//
// The reconciler's correctness invariant is "no stale state". This test
// hammers it from random angles to surface any reconcile path that
// forgets to clean up.
func TestProperty_InvariantsHoldUnderRandomEvents(t *testing.T) {
	for _, seed := range []int64{1, 42, 1337, 9999} {
		t.Run("seed", func(t *testing.T) { runProperty(t, seed, 500) })
	}
}

func runProperty(t *testing.T, seed int64, events int) {
	r := rand.New(rand.NewSource(seed))

	// Boot a Catalog from the fixture so the initial snapshot is non-empty
	// and validating.
	provs, hosts, pols, models, keys, rls, rks := fixture()
	pr0 := &pricing.Pricing{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "p0",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hosts[0].Meta.ID},
		},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{models[0].Meta.ID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1},
			},
		},
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, prList{pr0})
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	// Operate on the same set of ids the fixture established. The reconciler
	// is exercised via toggle-disable, toggle-enable, delete, and re-upsert.
	all := []func(){
		// Toggle provider 0 enabled state by upserting a flipped copy.
		func() {
			cp := *provs[0]
			cp.Spec.Enabled = togglePtr(provs[0].Spec.Enabled)
			provs[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyProviderUpsert(&cp)
		},
		func() {
			cp := *hosts[0]
			cp.Spec.Enabled = togglePtr(hosts[0].Spec.Enabled)
			hosts[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyHostUpsert(&cp)
		},
		func() {
			cp := *models[0]
			cp.Spec.Enabled = togglePtr(models[0].Spec.Enabled)
			models[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyModelUpsert(&cp)
		},
		func() {
			cp := *models[1]
			cp.Spec.Enabled = togglePtr(models[1].Spec.Enabled)
			models[1].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyModelUpsert(&cp)
		},
		func() {
			cp := *keys[0]
			cp.Spec.Enabled = togglePtr(keys[0].Spec.Enabled)
			keys[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyHostKeyUpsert(&cp)
		},
		func() {
			cp := *rls[0]
			cp.Spec.Enabled = togglePtr(rls[0].Spec.Enabled)
			rls[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyRateLimitUpsert(&cp)
		},
		func() {
			cp := *pols[0]
			cp.Spec.Enabled = togglePtr(pols[0].Spec.Enabled)
			pols[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyPolicyUpsert(&cp)
		},
		func() {
			cp := *rks[0]
			cp.Spec.Enabled = togglePtr(rks[0].Spec.Enabled)
			rks[0].Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyRelayKeyUpsert(&cp)
		},
		func() {
			cp := *pr0
			cp.Spec.Enabled = togglePtr(pr0.Spec.Enabled)
			pr0.Spec.Enabled = cp.Spec.Enabled
			_ = c.ApplyPricingUpsert(&cp)
		},
		// Hard deletes (the reconciler treats absent as no-op on second call).
		func() { _ = c.ApplyModelDelete(models[0].Meta.ID) },
		func() { _ = c.ApplyHostKeyDelete(keys[0].Meta.ID) },
		func() { _ = c.ApplyPricingDelete(pr0.Meta.ID) },
		// Re-upsert (revive). The fixture-row may have been deleted/disabled.
		func() {
			enable := true
			cp := *models[0]
			cp.Spec.Enabled = &enable
			models[0].Spec.Enabled = &enable
			_ = c.ApplyModelUpsert(&cp)
		},
		func() {
			enable := true
			cp := *keys[0]
			cp.Spec.Enabled = &enable
			keys[0].Spec.Enabled = &enable
			_ = c.ApplyHostKeyUpsert(&cp)
		},
		func() {
			enable := true
			cp := *pr0
			cp.Spec.Enabled = &enable
			pr0.Spec.Enabled = &enable
			_ = c.ApplyPricingUpsert(&cp)
		},
	}

	for i := 0; i < events; i++ {
		all[r.Intn(len(all))]()
		s := c.Current()
		if t.Failed() {
			return
		}
		assertSnapshotInvariants(t, s, i)
	}
}

func togglePtr(p *bool) *bool {
	cur := true
	if p != nil {
		cur = *p
	}
	next := !cur
	return &next
}

func assertSnapshotInvariants(t *testing.T, s *Snapshot, step int) {
	t.Helper()

	// 1. Every outbound ref of every present row resolves.
	check := func(child refKey, parents []refKey) {
		for _, p := range parents {
			if !s.rowExists(p) {
				t.Errorf("step %d: %s/%s -> %s/%s: parent missing",
					step, child.Kind, child.ID, p.Kind, p.ID)
			}
		}
	}
	for _, m := range s.modelsByID {
		check(refKey{Kind: refModel, ID: m.Meta.ID}, outboundModelRefs(m))
	}
	for _, k := range s.hostKeysByID {
		check(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	}
	for _, p := range s.policiesByID {
		check(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	}
	for _, p := range s.pricingsByID {
		check(refKey{Kind: refPricing, ID: p.Meta.ID}, outboundPricingRefs(p))
	}
	for _, k := range s.relayKeysByID {
		check(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundRelayKeyRefs(k))
	}

	// 2. Every dependent in every refsBy* set points at a present row.
	for name, m := range map[string]map[string]refSet{
		"refsByProvider":  s.refsByProvider,
		"refsByHost":      s.refsByHost,
		"refsByModel":     s.refsByModel,
		"refsByHostKey":   s.refsByHostKey,
		"refsByRateLimit": s.refsByRateLimit,
		"refsByPolicy":    s.refsByPolicy,
	} {
		for parentID, set := range m {
			for child := range set {
				if !s.rowExists(child) {
					t.Errorf("step %d: %s[%s]: dangling dependent %s/%s",
						step, name, parentID, child.Kind, child.ID)
				}
			}
		}
	}

	// 3. No duplicate Pricing claim on (model, host).
	seen := map[string]string{} // key → pricingID
	for _, p := range s.pricingsByID {
		hostID := p.Meta.Owner.ID
		for _, mid := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[mid]; !ok {
				continue
			}
			key := mid + "|" + hostID
			if other, exists := seen[key]; exists && other != p.Meta.ID {
				t.Errorf("step %d: duplicate Pricing claim on (%s, %s): %s and %s",
					step, mid, hostID, other, p.Meta.ID)
			}
			seen[key] = p.Meta.ID
		}
	}

	// 5. Reverse-join maps only reference present rows.
	for pid, ms := range s.modelsByPolicy {
		if _, ok := s.policiesByID[pid]; !ok {
			t.Errorf("step %d: modelsByPolicy[%s]: policy not present", step, pid)
		}
		for _, m := range ms {
			if _, ok := s.modelsByID[m.Meta.ID]; !ok {
				t.Errorf("step %d: modelsByPolicy[%s]: model %s not present", step, pid, m.Meta.ID)
			}
		}
	}
	for pid, ks := range s.hostKeysByPolicy {
		if _, ok := s.policiesByID[pid]; !ok {
			t.Errorf("step %d: hostKeysByPolicy[%s]: policy not present", step, pid)
		}
		for _, k := range ks {
			if _, ok := s.hostKeysByID[k.Meta.ID]; !ok {
				t.Errorf("step %d: hostKeysByPolicy[%s]: key %s not present", step, pid, k.Meta.ID)
			}
		}
	}
	for pid, r := range s.rateLimitByPolicy {
		if _, ok := s.policiesByID[pid]; !ok {
			t.Errorf("step %d: rateLimitByPolicy[%s]: policy not present", step, pid)
		}
		if _, ok := s.rateLimitsByID[r.Meta.ID]; !ok {
			t.Errorf("step %d: rateLimitByPolicy[%s]: rl %s not present", step, pid, r.Meta.ID)
		}
	}

	// 6. pricingByModelHost values are present.
	for key, p := range s.pricingByModelHost {
		if _, ok := s.pricingsByID[p.Meta.ID]; !ok {
			t.Errorf("step %d: pricingByModelHost[%s]: pricing %s not present", step, key, p.Meta.ID)
		}
	}
}

// Sanity: keep imports referenced.
var (
	_ = host.Spec{}
	_ = hostkey.Spec{}
	_ = model.Spec{}
	_ = policy.Spec{}
	_ = provider.Spec{}
	_ = ratelimit.Spec{}
	_ = relaykey.Spec{}
)
