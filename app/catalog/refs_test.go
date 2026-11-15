package catalog

import (
	"fmt"
	"slices"
	"testing"
)

// snapshotFromFixture builds the standard fixture plus one pricing covering both its models.
func snapshotFromFixture(t *testing.T) *Snapshot {
	t.Helper()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	pr := pricingFor("openai-tier", hosts[0].Meta.ID, models[0].Meta.ID, models[1].Meta.ID)
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr}, bnds)
	must(t, c.Reload(t.Context()))
	return c.Current()
}

// refOps lists every row of s that registers refs, with the parents it names.
func refOps(s *Snapshot) map[refKey][]refKey {
	ops := map[refKey][]refKey{}
	for _, m := range s.modelsByID {
		ops[refKey{refModel, m.Meta.ID}] = outboundModelRefs(m)
	}
	for _, k := range s.hostKeysByID {
		ops[refKey{refHostKey, k.Meta.ID}] = outboundHostKeyRefs(k)
	}
	for _, p := range s.policiesByID {
		ops[refKey{refPolicy, p.Meta.ID}] = outboundPolicyRefs(p)
	}
	for _, p := range s.disabledPoliciesByID {
		ops[refKey{refPolicy, p.Meta.ID}] = outboundPolicyRefs(p)
	}
	for _, r := range s.rateLimitsByID {
		ops[refKey{refRateLimit, r.Meta.ID}] = outboundRateLimitRefs(r)
	}
	for _, p := range s.pricingsByID {
		ops[refKey{refPricing, p.Meta.ID}] = outboundPricingRefs(p)
	}
	for _, k := range s.keysByID {
		ops[refKey{refRelayKey, k.Meta.ID}] = outboundKeyRefs(k)
	}
	for _, b := range s.bindingsByID {
		ops[refKey{refBinding, b.Meta.ID}] = outboundBindingRefs(b)
	}
	for _, p := range s.projectsByID {
		ops[refKey{refProject, p.Meta.ID}] = outboundProjectRefs(p)
	}
	for _, sa := range s.serviceAccountsByID {
		ops[refKey{refServiceAccount, sa.Meta.ID}] = outboundServiceAccountRefs(sa)
	}
	for _, b := range s.roleBindingsByID {
		ops[refKey{refRoleBinding, b.Meta.ID}] = outboundRoleBindingRefs(b)
	}
	for _, b := range s.policyBindingsByID {
		ops[refKey{refPolicyBinding, b.Meta.ID}] = outboundPolicyBindingRefs(b)
	}
	return ops
}

func refMaps(s *Snapshot) map[string]map[string]refSet {
	return map[string]map[string]refSet{
		"Provider": s.refsByProvider, "Host": s.refsByHost, "Model": s.refsByModel,
		"HostKey": s.refsByHostKey, "RateLimit": s.refsByRateLimit, "Policy": s.refsByPolicy,
		"Team": s.refsByTeam, "Project": s.refsByProject,
		"ServiceAccount": s.refsByServiceAccount, "Role": s.refsByRole,
	}
}

// assertRefsSymmetric checks the snapshot's no-stale-state invariants: every present row's parents are present and list it back, every recorded dependent is present, and each (model, host) price is a present pricing no other pricing also claims.
func assertRefsSymmetric(t *testing.T, s *Snapshot, when string) {
	t.Helper()
	for child, parents := range refOps(s) {
		for _, p := range parents {
			if !rowPresent(s, p) {
				t.Errorf("%s: %s/%s references a missing %s/%s", when, child.Kind, child.ID, p.Kind, p.ID)
			} else if !slices.Contains(s.Dependents(p.Kind, p.ID), child) {
				t.Errorf("%s: %s/%s does not list its dependent %s/%s", when, p.Kind, p.ID, child.Kind, child.ID)
			}
		}
	}
	for name, m := range refMaps(s) {
		for parentID, set := range m {
			for child := range set {
				if !rowPresent(s, child) {
					t.Errorf("%s: refsBy%s[%s] holds a dropped dependent %s/%s", when, name, parentID, child.Kind, child.ID)
				}
			}
		}
	}
	claimed := map[string]string{}
	for _, p := range s.pricingsByID {
		for _, mid := range p.Spec.TargetModelIDs {
			slot := mid + "|" + p.Meta.Owner.ID
			if other, dup := claimed[slot]; dup {
				t.Errorf("%s: pricings %s and %s both claim %s", when, other, p.Meta.ID, slot)
			}
			claimed[slot] = p.Meta.ID
			if got := s.pricingByModelHost[slot]; got != p {
				t.Errorf("%s: pricingByModelHost[%s] = %v, want pricing %s", when, slot, got, p.Meta.ID)
			}
		}
	}
	if len(s.pricingByModelHost) != len(claimed) {
		t.Errorf("%s: pricingByModelHost holds %d slots, the present pricings claim %d", when, len(s.pricingByModelHost), len(claimed))
	}
}

func TestRefs_BuiltSnapshotIsSymmetric(t *testing.T) {
	s := snapshotFromFixture(t)
	if len(s.Dependents(refHost, s.EnabledHosts()[0].Meta.ID)) != 5 {
		t.Fatalf("host dependents = %v, want its two keys, two bindings and the pricing", s.Dependents(refHost, s.EnabledHosts()[0].Meta.ID))
	}
	assertRefsSymmetric(t, s, "after build")
}

// Unregistering every row's refs empties every set, and registering them again restores exactly what the build recorded.
func TestRefs_RegisterUnregisterNetsZero(t *testing.T) {
	s := snapshotFromFixture(t)
	built := fmt.Sprint(refMaps(s))
	ops := refOps(s)
	for child, parents := range ops {
		s.unregisterRefs(child, parents)
	}
	for name, m := range refMaps(s) {
		for parentID, set := range m {
			if len(set) != 0 {
				t.Errorf("after full unregister refsBy%s[%s] still holds %v", name, parentID, set)
			}
		}
	}
	for child, parents := range ops {
		s.registerRefs(child, parents)
	}
	if got := fmt.Sprint(refMaps(s)); got != built {
		t.Errorf("after re-register the ref maps are\n%s\nwant what the build recorded\n%s", got, built)
	}
}
