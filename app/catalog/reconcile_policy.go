package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/ratelimit"
)

// ── RateLimit ─────────────────────────────────────────────────────────────

func (c *Catalog) ApplyRateLimitUpsert(r *ratelimit.RateLimit) error {
	if !r.IsEnabled() {
		return c.ApplyRateLimitDelete(r.Meta.ID)
	}
	if err := r.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refRateLimit, r.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeRateLimit(r, snapIDs(s.projectsByID))
	if !keep {
		deleteRateLimit(s, r.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	// Strip stale name index when slug changed for the same id.
	if prev, ok := s.rateLimitsByID[clean.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRateLimit, ID: prev.Meta.ID}, outboundRateLimitRefs(prev))
		if prev.Meta.Name != clean.Meta.Name {
			delete(s.rateLimitsByName, prev.Meta.Name)
		}
	}
	s.rateLimitsByID[clean.Meta.ID] = clean
	s.rateLimitsByName[clean.Meta.Name] = clean
	s.registerRefs(refKey{Kind: refRateLimit, ID: clean.Meta.ID}, outboundRateLimitRefs(clean))
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRateLimitDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRateLimit(s, id)
	c.snap.Store(s)
	return nil
}

func deleteRateLimit(s *Snapshot, id string) {
	r, ok := s.rateLimitsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRateLimit, ID: id}, outboundRateLimitRefs(r))
	delete(s.rateLimitsByID, id)
	delete(s.rateLimitsByName, r.Meta.Name)
	// Remove from policy reverse join.
	for polID, rl := range s.rateLimitByPolicy {
		if rl.Meta.ID == id {
			delete(s.rateLimitByPolicy, polID)
		}
	}
	cascadeDelete(s, refRateLimit, id)
	resanitizePoliciesAfterParentChange(s)
}

// ── Policy ────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyPolicyUpsert(p *policy.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	// Absent means the row was never here, or a delete-cascade stripped
	// dependents only source truth still records — true whether or not the
	// arriving row is enabled, so a policy created disabled recovers too.
	if handled, err := c.recoverAbsentLocked(refPolicy, p.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizePolicy(p, snapIDs(s.modelsByID), snapIDs(s.hostKeysByID), snapIDs(s.rateLimitsByID), snapIDs(s.projectsByID))
	if !keep {
		deletePolicy(s, p.Meta.ID)
		c.commitWithGrants(s)
		return nil
	}
	if !clean.IsEnabled() {
		disablePolicy(s, clean)
		c.commitWithGrants(s)
		return nil
	}
	insertPolicy(s, clean)
	// The row that just landed may invalidate its dependents — an owner or
	// host change can leave a host key's tier policy no longer its host's.
	cascadeDelete(s, refPolicy, clean.Meta.ID)
	c.commitWithGrants(s)
	return nil
}

func (c *Catalog) ApplyPolicyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deletePolicy(s, id)
	c.commitWithGrants(s)
	return nil
}

// disablePolicy takes the row out of every routing index but keeps it, and
// every row naming it, in the snapshot: those requests answer 403
// policy_disabled instead of falling through to a broader grant.
func disablePolicy(s *Snapshot, p *policy.Policy) {
	if old, ok := s.policiesByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicy, ID: old.Meta.ID}, outboundPolicyRefs(old))
		delete(s.policiesByID, old.Meta.ID)
		delete(s.policiesByName, old.Meta.Name)
		delete(s.modelsByPolicy, old.Meta.ID)
		delete(s.hostKeysByPolicy, old.Meta.ID)
		delete(s.rateLimitByPolicy, old.Meta.ID)
	}
	if old, ok := s.disabledPoliciesByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicy, ID: old.Meta.ID}, outboundPolicyRefs(old))
	}
	s.disabledPoliciesByID[p.Meta.ID] = p
	// Still a ref holder: losing its project has to evict it, or the rows
	// naming it keep answering 403 against a policy nothing can reach.
	s.registerRefs(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	resanitizeHostsAfterPolicyChange(s)
}

func insertPolicy(s *Snapshot, p *policy.Policy) {
	if off, ok := s.disabledPoliciesByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(off))
		delete(s.disabledPoliciesByID, p.Meta.ID)
	}
	if old, ok := s.policiesByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicy, ID: old.Meta.ID}, outboundPolicyRefs(old))
		delete(s.policiesByID, old.Meta.ID)
		delete(s.policiesByName, old.Meta.Name)
		delete(s.modelsByPolicy, old.Meta.ID)
		delete(s.hostKeysByPolicy, old.Meta.ID)
		delete(s.rateLimitByPolicy, old.Meta.ID)
	}
	s.policiesByID[p.Meta.ID] = p
	s.policiesByName[p.Meta.Name] = p
	s.registerRefs(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	// Populate reverse joins.
	for _, id := range p.Spec.ModelIDs {
		if m, ok := s.modelsByID[id]; ok {
			s.modelsByPolicy[p.Meta.ID] = append(s.modelsByPolicy[p.Meta.ID], m)
		}
	}
	for _, id := range p.Spec.HostKeyIDs {
		if k, ok := s.hostKeysByID[id]; ok {
			s.hostKeysByPolicy[p.Meta.ID] = append(s.hostKeysByPolicy[p.Meta.ID], k)
		}
	}
	if p.Spec.RateLimitID != "" {
		if r, ok := s.rateLimitsByID[p.Spec.RateLimitID]; ok {
			s.rateLimitByPolicy[p.Meta.ID] = r
		}
	}
}

func deletePolicy(s *Snapshot, id string) {
	if off, ok := s.disabledPoliciesByID[id]; ok {
		s.unregisterRefs(refKey{Kind: refPolicy, ID: id}, outboundPolicyRefs(off))
		delete(s.disabledPoliciesByID, id)
	}
	p, ok := s.policiesByID[id]
	if !ok {
		cascadeDelete(s, refPolicy, id)
		return
	}
	s.unregisterRefs(refKey{Kind: refPolicy, ID: id}, outboundPolicyRefs(p))
	delete(s.policiesByID, id)
	delete(s.policiesByName, p.Meta.Name)
	delete(s.modelsByPolicy, id)
	delete(s.hostKeysByPolicy, id)
	delete(s.rateLimitByPolicy, id)
	cascadeDelete(s, refPolicy, id)
	resanitizeHostsAfterPolicyChange(s)
}

// ── Pricing ───────────────────────────────────────────────────────────────

func (c *Catalog) ApplyPricingUpsert(p *pricing.Pricing) error {
	if !p.IsEnabled() {
		return c.ApplyPricingDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizePricing(p, snapIDs(s.hostsByID), snapIDs(s.modelsByID))
	if !keep {
		deletePricing(s, p.Meta.ID)
		// Also check duplicate-pricing invariant on the cleaned row before
		// inserting; not applicable here since !keep aborts.
		c.snap.Store(s)
		return nil
	}
	// Enforce the duplicate-pricing invariant (two enabled rows competing
	// for the same model+host slot) — this is a real authoring bug and
	// stays hard-fail.
	for _, modelID := range clean.Spec.TargetModelIDs {
		key := modelID + "|" + clean.Meta.Owner.ID
		if existing, dup := s.pricingByModelHost[key]; dup && existing.Meta.ID != clean.Meta.ID {
			return fmt.Errorf("duplicate pricing: pricing %q and %q both cover model %q for the same host",
				existing.Meta.Name, clean.Meta.Name, modelID)
		}
	}
	insertPricing(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyPricingDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deletePricing(s, id)
	c.snap.Store(s)
	return nil
}

func insertPricing(s *Snapshot, p *pricing.Pricing) {
	if old, ok := s.pricingsByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPricing, ID: old.Meta.ID}, outboundPricingRefs(old))
		for _, modelID := range old.Spec.TargetModelIDs {
			delete(s.pricingByModelHost, modelID+"|"+old.Meta.Owner.ID)
		}
		delete(s.pricingsByID, old.Meta.ID)
	}
	s.pricingsByID[p.Meta.ID] = p
	hostID := p.Meta.Owner.ID
	for _, modelID := range p.Spec.TargetModelIDs {
		if _, ok := s.modelsByID[modelID]; ok {
			s.pricingByModelHost[modelID+"|"+hostID] = p
		}
	}
	s.registerRefs(refKey{Kind: refPricing, ID: p.Meta.ID}, outboundPricingRefs(p))
}

func deletePricing(s *Snapshot, id string) {
	p, ok := s.pricingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refPricing, ID: id}, outboundPricingRefs(p))
	for _, modelID := range p.Spec.TargetModelIDs {
		delete(s.pricingByModelHost, modelID+"|"+p.Meta.Owner.ID)
	}
	delete(s.pricingsByID, id)
}
