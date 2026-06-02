package catalog

import (
	"context"
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

// snapshotFromFixture builds a Snapshot via Reload over the standard fixture
// (with an extra Pricing added). The returned snapshot is what every
// invariant test operates over.
func snapshotFromFixture(t *testing.T) *Snapshot {
	t.Helper()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	// Build a Pricing covering both fixture models.
	pr := &pricing.Pricing{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "openai-tier",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hosts[0].Meta.ID},
		},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{models[0].Meta.ID, models[1].Meta.ID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1},
			},
		},
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, prList{pr}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c.Current()
}

// prList is an in-memory Pricing lister for tests.
type prList []*pricing.Pricing

func (l prList) List(context.Context) ([]*pricing.Pricing, error) { return l, nil }

// TestRefs_EveryOutboundResolves walks every row in the snapshot and
// verifies its outbound refs all point at rows present in the snapshot.
// This catches builders that fail to register a row or fail to validate
// its dependencies.
func TestRefs_EveryOutboundResolves(t *testing.T) {
	s := snapshotFromFixture(t)

	check := func(child refKey, parents []refKey) {
		for _, p := range parents {
			if !s.rowExists(p) {
				t.Errorf("%s/%s -> %s/%s: parent missing", child.Kind, child.ID, p.Kind, p.ID)
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
}

// TestRefs_EveryDependentExists verifies the dual: every entry in every
// refsBy* set points at a row that's actually in the snapshot.
func TestRefs_EveryDependentExists(t *testing.T) {
	s := snapshotFromFixture(t)

	checkMap := func(name string, m map[string]refSet) {
		for parentID, set := range m {
			for child := range set {
				if !s.rowExists(child) {
					t.Errorf("%s[%s]: dependent %s/%s not in snapshot",
						name, parentID, child.Kind, child.ID)
				}
			}
		}
	}
	checkMap("refsByProvider", s.refsByProvider)
	checkMap("refsByHost", s.refsByHost)
	checkMap("refsByModel", s.refsByModel)
	checkMap("refsByHostKey", s.refsByHostKey)
	checkMap("refsByRateLimit", s.refsByRateLimit)
	checkMap("refsByPolicy", s.refsByPolicy)
}

// TestRefs_RegisterUnregisterNetsZero starts from a fresh snapshot, runs
// register+unregister for every row's outbound refs, and asserts the
// refsBy* maps end up empty for those keys.
func TestRefs_RegisterUnregisterNetsZero(t *testing.T) {
	s := snapshotFromFixture(t)

	type op struct {
		child   refKey
		parents []refKey
	}
	var ops []op
	for _, m := range s.modelsByID {
		ops = append(ops, op{refKey{Kind: refModel, ID: m.Meta.ID}, outboundModelRefs(m)})
	}
	for _, k := range s.hostKeysByID {
		ops = append(ops, op{refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k)})
	}
	for _, p := range s.policiesByID {
		ops = append(ops, op{refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p)})
	}
	for _, p := range s.pricingsByID {
		ops = append(ops, op{refKey{Kind: refPricing, ID: p.Meta.ID}, outboundPricingRefs(p)})
	}
	for _, k := range s.relayKeysByID {
		ops = append(ops, op{refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundRelayKeyRefs(k)})
	}
	for _, b := range s.bindingsByID {
		ops = append(ops, op{refKey{Kind: refBinding, ID: b.Meta.ID}, outboundBindingRefs(b)})
	}

	// Currently every op is already registered. Unregister all → every
	// refsBy* set should be empty.
	for _, o := range ops {
		s.unregisterRefs(o.child, o.parents)
	}
	totalEntries := func(m map[string]refSet) int {
		n := 0
		for _, set := range m {
			n += len(set)
		}
		return n
	}
	if n := totalEntries(s.refsByProvider) + totalEntries(s.refsByHost) +
		totalEntries(s.refsByModel) + totalEntries(s.refsByHostKey) +
		totalEntries(s.refsByRateLimit) + totalEntries(s.refsByPolicy); n != 0 {
		t.Errorf("after full unregister: %d ref entries remain (want 0)", n)
	}

	// Re-register; counts must match what we started with.
	for _, o := range ops {
		s.registerRefs(o.child, o.parents)
	}
	// Sanity: every dependent still resolves.
	for _, m := range []map[string]refSet{
		s.refsByProvider, s.refsByHost, s.refsByModel,
		s.refsByHostKey, s.refsByRateLimit, s.refsByPolicy,
	} {
		for _, set := range m {
			for k := range set {
				if !s.rowExists(k) {
					t.Errorf("after re-register: dependent %s/%s missing", k.Kind, k.ID)
				}
			}
		}
	}
}

// rowExists is a test helper that returns true iff (kind, id) is in the
// snapshot. Mirrors the row presence accessors.
func (s *Snapshot) rowExists(k refKey) bool {
	switch k.Kind {
	case refProvider:
		_, ok := s.providersByID[k.ID]
		return ok
	case refHost:
		_, ok := s.hostsByID[k.ID]
		return ok
	case refModel:
		_, ok := s.modelsByID[k.ID]
		return ok
	case refHostKey:
		_, ok := s.hostKeysByID[k.ID]
		return ok
	case refRateLimit:
		_, ok := s.rateLimitsByID[k.ID]
		return ok
	case refPolicy:
		_, ok := s.policiesByID[k.ID]
		return ok
	case refPricing:
		_, ok := s.pricingsByID[k.ID]
		return ok
	case refRelayKey:
		_, ok := s.relayKeysByID[k.ID]
		return ok
	case refBinding:
		_, ok := s.bindingsByID[k.ID]
		return ok
	}
	return false
}

// Sanity import-only: keep the imports from being trimmed if tests stub out.
var (
	_ = host.Spec{}
	_ = hostkey.Spec{}
	_ = model.Spec{}
	_ = policy.Spec{}
	_ = provider.Spec{}
	_ = ratelimit.Spec{}
	_ = relaykey.Spec{}
)
