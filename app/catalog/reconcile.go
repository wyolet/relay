package catalog

import (
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

func (c *Catalog) ApplyProviderUpsert(p *provider.Provider) error {
	if !p.IsEnabled() {
		return c.ApplyProviderDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()

	// Remove old entry if present.
	if old, ok := s.providersByID[p.Meta.ID]; ok {
		delete(s.providersByName, old.Meta.Name)
		delete(s.providersByID, old.Meta.ID)
	}
	s.providersByID[p.Meta.ID] = p
	s.providersByName[p.Meta.Name] = p
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyProviderDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteProvider(s, id)
	c.snap.Store(s)
	return nil
}

func deleteProvider(s *Snapshot, id string) {
	p, ok := s.providersByID[id]
	if !ok {
		return
	}
	delete(s.providersByID, id)
	delete(s.providersByName, p.Meta.Name)
	cascadeDelete(s, refProvider, id)
}

// ── Host ──────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyHostUpsert(h *host.Host) error {
	if !h.IsEnabled() {
		return c.ApplyHostDelete(h.Meta.ID)
	}
	if err := h.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()

	if err := validateHostInSnap(h, s); err != nil {
		return err
	}

	if old, ok := s.hostsByID[h.Meta.ID]; ok {
		delete(s.hostsByName, old.Meta.Name)
		delete(s.hostsByID, old.Meta.ID)
	}
	s.hostsByID[h.Meta.ID] = h
	s.hostsByName[h.Meta.Name] = h
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyHostDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteHost(s, id)
	c.snap.Store(s)
	return nil
}

func deleteHost(s *Snapshot, id string) {
	h, ok := s.hostsByID[id]
	if !ok {
		return
	}
	delete(s.hostsByID, id)
	delete(s.hostsByName, h.Meta.Name)
	cascadeDelete(s, refHost, id)
}

// ── Model ─────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyModelUpsert(m *model.Model) error {
	if !m.IsEnabled() {
		return c.ApplyModelDelete(m.Meta.ID)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	if err := validateModelInSnap(m, s); err != nil {
		return err
	}
	insertModel(s, m)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyModelDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteModel(s, id)
	c.snap.Store(s)
	return nil
}

func insertModel(s *Snapshot, m *model.Model) {
	// Remove old aliases/refs if updating.
	if old, ok := s.modelsByID[m.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refModel, ID: old.Meta.ID}, outboundModelRefs(old))
		for _, a := range old.Spec.Aliases {
			s.modelsByName[a] = removeModelFromSlice(s.modelsByName[a], old.Meta.ID)
		}
		// Remove from any policy reverse joins.
		for polID, models := range s.modelsByPolicy {
			s.modelsByPolicy[polID] = removeModelFromSlice(models, old.Meta.ID)
		}
		delete(s.modelsByID, old.Meta.ID)
	}
	s.modelsByID[m.Meta.ID] = m
	for _, a := range m.Spec.Aliases {
		s.modelsByName[a] = append(s.modelsByName[a], m)
	}
	s.registerRefs(refKey{Kind: refModel, ID: m.Meta.ID}, outboundModelRefs(m))
	// Rebuild policy reverse joins for policies that reference this model.
	rebuildModelsByPolicy(s)
}

func deleteModel(s *Snapshot, id string) {
	m, ok := s.modelsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refModel, ID: id}, outboundModelRefs(m))
	for _, a := range m.Spec.Aliases {
		s.modelsByName[a] = removeModelFromSlice(s.modelsByName[a], id)
	}
	delete(s.modelsByID, id)
	// Remove from policy joins.
	for polID, models := range s.modelsByPolicy {
		s.modelsByPolicy[polID] = removeModelFromSlice(models, id)
	}
	// Remove pricingByModelHost entries targeting this model.
	for k, p := range s.pricingByModelHost {
		_ = p
		// key format: modelID|hostID
		if len(k) > len(id) && k[:len(id)] == id && k[len(id)] == '|' {
			delete(s.pricingByModelHost, k)
		}
	}
	cascadeDelete(s, refModel, id)
}

// ── HostKey ───────────────────────────────────────────────────────────────

func (c *Catalog) ApplyHostKeyUpsert(k *hostkey.HostKey) error {
	if !k.IsEnabled() {
		return c.ApplyHostKeyDelete(k.Meta.ID)
	}
	if err := k.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	if err := validateHostKeyInSnap(k, s); err != nil {
		return err
	}
	insertHostKey(s, k)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyHostKeyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteHostKey(s, id)
	c.snap.Store(s)
	return nil
}

func insertHostKey(s *Snapshot, k *hostkey.HostKey) {
	if old, ok := s.hostKeysByID[k.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refHostKey, ID: old.Meta.ID}, outboundHostKeyRefs(old))
		for polID, keys := range s.hostKeysByPolicy {
			s.hostKeysByPolicy[polID] = removeHostKeyFromSlice(keys, old.Meta.ID)
		}
		delete(s.hostKeysByID, old.Meta.ID)
	}
	s.hostKeysByID[k.Meta.ID] = k
	s.registerRefs(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	rebuildHostKeysByPolicy(s)
}

func deleteHostKey(s *Snapshot, id string) {
	k, ok := s.hostKeysByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refHostKey, ID: id}, outboundHostKeyRefs(k))
	delete(s.hostKeysByID, id)
	for polID, keys := range s.hostKeysByPolicy {
		s.hostKeysByPolicy[polID] = removeHostKeyFromSlice(keys, id)
	}
	cascadeDelete(s, refHostKey, id)
}

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
	s := c.snap.Load().clone()
	// Strip stale name index when slug changed for the same id.
	if prev, ok := s.rateLimitsByID[r.Meta.ID]; ok && prev.Meta.Name != r.Meta.Name {
		delete(s.rateLimitsByName, prev.Meta.Name)
	}
	s.rateLimitsByID[r.Meta.ID] = r
	s.rateLimitsByName[r.Meta.Name] = r
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
	delete(s.rateLimitsByID, id)
	delete(s.rateLimitsByName, r.Meta.Name)
	// Remove from policy reverse join.
	for polID, rl := range s.rateLimitByPolicy {
		if rl.Meta.ID == id {
			delete(s.rateLimitByPolicy, polID)
		}
	}
	cascadeDelete(s, refRateLimit, id)
}

// ── Policy ────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyPolicyUpsert(p *policy.Policy) error {
	if !p.IsEnabled() {
		return c.ApplyPolicyDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	if err := validatePolicyInSnap(p, s); err != nil {
		return err
	}
	insertPolicy(s, p)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyPolicyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deletePolicy(s, id)
	c.snap.Store(s)
	return nil
}

func insertPolicy(s *Snapshot, p *policy.Policy) {
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
	p, ok := s.policiesByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refPolicy, ID: id}, outboundPolicyRefs(p))
	delete(s.policiesByID, id)
	delete(s.policiesByName, p.Meta.Name)
	delete(s.modelsByPolicy, id)
	delete(s.hostKeysByPolicy, id)
	delete(s.rateLimitByPolicy, id)
	cascadeDelete(s, refPolicy, id)
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
	if err := validatePricingInSnap(p, s); err != nil {
		return err
	}
	insertPricing(s, p)
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

// ── RelayKey ──────────────────────────────────────────────────────────────

func (c *Catalog) ApplyRelayKeyUpsert(k *relaykey.RelayKey) error {
	if !k.IsEnabled() {
		return c.ApplyRelayKeyDelete(k.Meta.ID)
	}
	if err := k.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	if err := validateRelayKeyInSnap(k, s); err != nil {
		return err
	}
	insertRelayKey(s, k)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRelayKeyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRelayKey(s, id)
	c.snap.Store(s)
	return nil
}

func insertRelayKey(s *Snapshot, k *relaykey.RelayKey) {
	if old, ok := s.relayKeysByID[k.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRelayKey, ID: old.Meta.ID}, outboundRelayKeyRefs(old))
		if old.Spec.KeyHash != "" {
			delete(s.relayKeysByHash, old.Spec.KeyHash)
		}
		delete(s.relayKeysByID, old.Meta.ID)
	}
	s.relayKeysByID[k.Meta.ID] = k
	if k.Spec.KeyHash != "" {
		s.relayKeysByHash[k.Spec.KeyHash] = k
	}
	s.registerRefs(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundRelayKeyRefs(k))
}

func deleteRelayKey(s *Snapshot, id string) {
	k, ok := s.relayKeysByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRelayKey, ID: id}, outboundRelayKeyRefs(k))
	if k.Spec.KeyHash != "" {
		delete(s.relayKeysByHash, k.Spec.KeyHash)
	}
	delete(s.relayKeysByID, id)
}

// ── Cascade helpers ───────────────────────────────────────────────────────

// cascadeDelete uses an explicit worklist to avoid deep recursion. For each
// dependent of (kind, id) that fails cross-validation, it is deleted and its
// own dependents pushed onto the worklist.
func cascadeDelete(s *Snapshot, kind refKind, id string) {
	worklist := s.Dependents(kind, id)
	for len(worklist) > 0 {
		dep := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]

		if !rowPresent(s, dep) {
			continue
		}
		// Re-validate the dependent; if it's now invalid, delete it too.
		if !dependentStillValid(s, dep) {
			extra := s.Dependents(dep.Kind, dep.ID)
			worklist = append(worklist, extra...)
			deleteDirect(s, dep)
		}
	}
}

// dependentStillValid returns true only when the row's cross-refs all still
// resolve in s. Pricings: always invalidate when a target is gone (handled by
// deleteModel/deleteHost clearing pricingByModelHost, but we still need to
// evict the Pricing row).
func dependentStillValid(s *Snapshot, k refKey) bool {
	switch k.Kind {
	case refModel:
		m, ok := s.modelsByID[k.ID]
		if !ok {
			return true // already gone
		}
		return validateModelInSnap(m, s) == nil
	case refHostKey:
		hk, ok := s.hostKeysByID[k.ID]
		if !ok {
			return true
		}
		return validateHostKeyInSnap(hk, s) == nil
	case refPolicy:
		p, ok := s.policiesByID[k.ID]
		if !ok {
			return true
		}
		return validatePolicyInSnap(p, s) == nil
	case refPricing:
		p, ok := s.pricingsByID[k.ID]
		if !ok {
			return true
		}
		// For cascade, check refs without duplicate check (we already cleaned
		// pricingByModelHost) — just check host and model presence.
		if _, ok := s.hostsByID[p.Meta.Owner.ID]; !ok {
			return false
		}
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; !ok {
				return false
			}
		}
		return true
	case refRelayKey:
		rk, ok := s.relayKeysByID[k.ID]
		if !ok {
			return true
		}
		return validateRelayKeyInSnap(rk, s) == nil
	}
	return true
}

// rowPresent is the non-test equivalent of the test helper rowExists.
func rowPresent(s *Snapshot, k refKey) bool {
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
	}
	return false
}

// deleteDirect calls the appropriate delete helper for (kind, id).
func deleteDirect(s *Snapshot, k refKey) {
	switch k.Kind {
	case refModel:
		deleteModel(s, k.ID)
	case refHostKey:
		deleteHostKey(s, k.ID)
	case refPolicy:
		deletePolicy(s, k.ID)
	case refPricing:
		deletePricing(s, k.ID)
	case refRelayKey:
		deleteRelayKey(s, k.ID)
	case refRateLimit:
		deleteRateLimit(s, k.ID)
	case refProvider:
		deleteProvider(s, k.ID)
	case refHost:
		deleteHost(s, k.ID)
	}
}

// ── Reverse-join rebuild helpers ──────────────────────────────────────────

// rebuildModelsByPolicy recomputes the modelsByPolicy map from the current
// state of policiesByID and modelsByID.
func rebuildModelsByPolicy(s *Snapshot) {
	for polID, pol := range s.policiesByID {
		sl := s.modelsByPolicy[polID][:0]
		for _, id := range pol.Spec.ModelIDs {
			if m, ok := s.modelsByID[id]; ok {
				sl = append(sl, m)
			}
		}
		s.modelsByPolicy[polID] = sl
	}
}

func rebuildHostKeysByPolicy(s *Snapshot) {
	for polID, pol := range s.policiesByID {
		sl := s.hostKeysByPolicy[polID][:0]
		for _, id := range pol.Spec.HostKeyIDs {
			if k, ok := s.hostKeysByID[id]; ok {
				sl = append(sl, k)
			}
		}
		s.hostKeysByPolicy[polID] = sl
	}
}

// ── Slice helpers ─────────────────────────────────────────────────────────

func removeModelFromSlice(sl []*model.Model, id string) []*model.Model {
	out := sl[:0]
	for _, m := range sl {
		if m.Meta.ID != id {
			out = append(out, m)
		}
	}
	return out
}

func removeHostKeyFromSlice(sl []*hostkey.HostKey, id string) []*hostkey.HostKey {
	out := sl[:0]
	for _, k := range sl {
		if k.Meta.ID != id {
			out = append(out, k)
		}
	}
	return out
}
