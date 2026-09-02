package catalog

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// commitWithGrants recomputes the per-policy allowed-combo sets (a function of
// providers + hosts + models + policies) then atomically publishes the clone.
// Used by every Apply that can change a grant; hostkey/ratelimit/key/
// pricing writes don't affect grants and call c.snap.Store directly.
func (c *Catalog) commitWithGrants(s *Snapshot) {
	s.rebuildPolicyAllowSets()
	c.snap.Store(s)
}

// recoverAbsentLocked degrades an upsert for an id the snapshot doesn't hold
// to a full rebuild from the stores: an insert is indistinguishable from a
// re-appearance whose earlier delete-cascade stripped dependents (policy
// grants, relay keys, host keys) that only source truth still records, so an
// incremental patch would leave them permanently lost. Leaf kinds with no
// dependents (pricing, key) skip this and stay incremental. Caller must
// hold c.rmu; returns (handled, err).
func (c *Catalog) recoverAbsentLocked(kind refKind, id string) (bool, error) {
	if rowPresent(c.snap.Load(), refKey{Kind: kind, ID: id}) {
		return false, nil
	}
	// Apply* carries no ctx; this is a rare control-plane recovery path.
	return true, c.reloadLocked(context.Background())
}

func (c *Catalog) ApplyProviderUpsert(p *provider.Provider) error {
	if !p.IsEnabled() {
		return c.ApplyProviderDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refProvider, p.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()

	// Remove old entry if present.
	if old, ok := s.providersByID[p.Meta.ID]; ok {
		delete(s.providersByName, old.Meta.Name)
		delete(s.providersByID, old.Meta.ID)
	}
	s.providersByID[p.Meta.ID] = p
	s.providersByName[p.Meta.Name] = p
	c.commitWithGrants(s)
	return nil
}

func (c *Catalog) ApplyProviderDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteProvider(s, id)
	c.commitWithGrants(s)
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
	if handled, err := c.recoverAbsentLocked(refHost, h.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()

	clean := sanitizeHost(h, s.policiesByID)
	if old, ok := s.hostsByID[h.Meta.ID]; ok {
		delete(s.hostsByName, old.Meta.Name)
		delete(s.hostsByID, old.Meta.ID)
	}
	s.hostsByID[clean.Meta.ID] = clean
	s.hostsByName[clean.Meta.Name] = clean
	c.commitWithGrants(s)
	return nil
}

func (c *Catalog) ApplyHostDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteHost(s, id)
	c.commitWithGrants(s)
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
	resanitizeModelsAfterHostChange(s)
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
	if handled, err := c.recoverAbsentLocked(refModel, m.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeModel(m, snapIDs(s.providersByID))
	if !keep {
		deleteModel(s, m.Meta.ID)
		c.commitWithGrants(s)
		return nil
	}
	// clean is the TEMPLATE; index the overlay-merged effective row
	// (identity when no overlay exists). See overlay_apply.go.
	insertModel(s, s.overlaidModel(clean))
	c.commitWithGrants(s)
	return nil
}

func (c *Catalog) ApplyModelDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteModel(s, id)
	c.commitWithGrants(s)
	return nil
}

func insertModel(s *Snapshot, m *model.Model) {
	// Remove old aliases/refs if updating.
	if old, ok := s.modelsByID[m.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refModel, ID: old.Meta.ID}, outboundModelRefs(old))
		s.modelsByName[old.Meta.Name] = removeModelFromSlice(s.modelsByName[old.Meta.Name], old.Meta.ID)
		s.deindexModelSnapshots(old)
		// Remove from any policy reverse joins.
		for polID, models := range s.modelsByPolicy {
			s.modelsByPolicy[polID] = removeModelFromSlice(models, old.Meta.ID)
		}
		delete(s.modelsByID, old.Meta.ID)
	}
	s.modelsByID[m.Meta.ID] = m
	s.modelsByName[m.Meta.Name] = append(s.modelsByName[m.Meta.Name], m)
	s.indexModelSnapshots(m)
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
	s.modelsByName[m.Meta.Name] = removeModelFromSlice(s.modelsByName[m.Meta.Name], id)
	s.deindexModelSnapshots(m)
	delete(s.modelsByID, id)
	// The overlay row (if any) stays registered — inert until the model
	// reappears; only the stashed template goes with the model.
	delete(s.modelTemplates, id)
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
	resanitizePoliciesAfterParentChange(s)
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
	if handled, err := c.recoverAbsentLocked(refHostKey, k.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeHostKey(k, snapIDs(s.hostsByID), snapIDs(s.projectsByID), s.policiesByID)
	if !keep {
		deleteHostKey(s, k.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertHostKey(s, clean)
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
	resanitizePoliciesAfterParentChange(s)
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
	if !p.IsEnabled() {
		return c.ApplyPolicyDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
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
	insertPolicy(s, clean)
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

// ── Key ──────────────────────────────────────────────────────────────

func (c *Catalog) ApplyKeyUpsert(k *key.Key) error {
	if !k.IsEnabled() {
		return c.ApplyKeyDelete(k.Meta.ID)
	}
	if err := k.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizeKey(k, snapIDs(s.policiesByID), snapIDs(s.serviceAccountsByID))
	if !keep {
		deleteKey(s, k.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertKey(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyKeyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteKey(s, id)
	c.snap.Store(s)
	return nil
}

func insertKey(s *Snapshot, k *key.Key) {
	if old, ok := s.keysByID[k.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRelayKey, ID: old.Meta.ID}, outboundKeyRefs(old))
		unindexKeyHashes(s, old)
		delete(s.keysByID, old.Meta.ID)
	}
	s.keysByID[k.Meta.ID] = k
	if k.Spec.KeyHash != "" {
		s.keysByHash[k.Spec.KeyHash] = k
	}
	// The pre-rotation hash is indexed only while its grace window is open,
	// so an expired grace drops out on the next reconcile or reload.
	if k.InGrace(time.Now()) {
		s.keysByHash[k.Spec.PreviousKeyHash] = k
	}
	s.registerRefs(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundKeyRefs(k))
}

func deleteKey(s *Snapshot, id string) {
	k, ok := s.keysByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRelayKey, ID: id}, outboundKeyRefs(k))
	unindexKeyHashes(s, k)
	delete(s.keysByID, id)
}

func unindexKeyHashes(s *Snapshot, k *key.Key) {
	if k.Spec.KeyHash != "" {
		delete(s.keysByHash, k.Spec.KeyHash)
	}
	if k.Spec.PreviousKeyHash != "" {
		delete(s.keysByHash, k.Spec.PreviousKeyHash)
	}
}

// ── ServiceAccount ────────────────────────────────────────────────────────

func (c *Catalog) ApplyServiceAccountUpsert(sa *serviceaccount.ServiceAccount) error {
	if !sa.IsEnabled() {
		return c.ApplyServiceAccountDelete(sa.Meta.ID)
	}
	if err := sa.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refServiceAccount, sa.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeServiceAccount(sa, snapIDs(s.projectsByID), snapIDs(s.policiesByID))
	if !keep {
		deleteServiceAccount(s, sa.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertServiceAccount(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyServiceAccountDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteServiceAccount(s, id)
	c.snap.Store(s)
	return nil
}

func insertServiceAccount(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	if old, ok := s.serviceAccountsByID[sa.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refServiceAccount, ID: old.Meta.ID}, outboundServiceAccountRefs(old))
		delete(s.serviceAccountsByName, old.Meta.Name)
		delete(s.serviceAccountsByID, old.Meta.ID)
		removeServiceAccountFromProject(s, old)
	}
	s.serviceAccountsByID[sa.Meta.ID] = sa
	s.serviceAccountsByName[sa.Meta.Name] = sa
	insertServiceAccountIntoProject(s, sa)
	s.registerRefs(refKey{Kind: refServiceAccount, ID: sa.Meta.ID}, outboundServiceAccountRefs(sa))
}

func deleteServiceAccount(s *Snapshot, id string) {
	sa, ok := s.serviceAccountsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refServiceAccount, ID: id}, outboundServiceAccountRefs(sa))
	delete(s.serviceAccountsByID, id)
	delete(s.serviceAccountsByName, sa.Meta.Name)
	removeServiceAccountFromProject(s, sa)
	cascadeDelete(s, refServiceAccount, id)
}

// insertServiceAccountIntoProject keeps serviceAccountsByProject sorted by
// account name, the order build() produces.
func insertServiceAccountIntoProject(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	list := append(s.serviceAccountsByProject[sa.Spec.ProjectID], sa)
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	s.serviceAccountsByProject[sa.Spec.ProjectID] = list
}

func removeServiceAccountFromProject(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	list := s.serviceAccountsByProject[sa.Spec.ProjectID]
	out := make([]*serviceaccount.ServiceAccount, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != sa.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.serviceAccountsByProject, sa.Spec.ProjectID)
		return
	}
	s.serviceAccountsByProject[sa.Spec.ProjectID] = out
}

// ── Group ─────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyGroupUpsert(g *group.Group) error {
	if !g.IsEnabled() {
		return c.ApplyGroupDelete(g.Meta.ID)
	}
	if err := g.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	insertGroup(s, g)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyGroupDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteGroup(s, id)
	c.snap.Store(s)
	return nil
}

func insertGroup(s *Snapshot, g *group.Group) {
	if old, ok := s.groupsByID[g.Meta.ID]; ok {
		delete(s.groupsByName, old.Meta.Name)
		delete(s.groupsByID, old.Meta.ID)
		removeGroupFromUsers(s, old)
	}
	s.groupsByID[g.Meta.ID] = g
	s.groupsByName[g.Meta.Name] = g
	for _, uid := range g.Spec.MemberIDs {
		list := append(s.groupsByUser[uid], g.Meta.Name)
		sort.Strings(list)
		s.groupsByUser[uid] = list
	}
}

func deleteGroup(s *Snapshot, id string) {
	g, ok := s.groupsByID[id]
	if !ok {
		return
	}
	delete(s.groupsByID, id)
	delete(s.groupsByName, g.Meta.Name)
	removeGroupFromUsers(s, g)
}

func removeGroupFromUsers(s *Snapshot, g *group.Group) {
	for _, uid := range g.Spec.MemberIDs {
		list := s.groupsByUser[uid]
		out := make([]string, 0, len(list))
		for _, name := range list {
			if name != g.Meta.Name {
				out = append(out, name)
			}
		}
		if len(out) == 0 {
			delete(s.groupsByUser, uid)
			continue
		}
		s.groupsByUser[uid] = out
	}
}

// ── Role ──────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyRoleUpsert(r *role.Role) error {
	if !r.IsEnabled() {
		return c.ApplyRoleDelete(r.Meta.ID)
	}
	if err := r.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refRole, r.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	insertRole(s, r)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRoleDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRole(s, id)
	c.snap.Store(s)
	return nil
}

func insertRole(s *Snapshot, r *role.Role) {
	if old, ok := s.rolesByID[r.Meta.ID]; ok {
		delete(s.rolesByName, old.Meta.Name)
		delete(s.rolesByID, old.Meta.ID)
	}
	s.rolesByID[r.Meta.ID] = r
	s.rolesByName[r.Meta.Name] = r
}

func deleteRole(s *Snapshot, id string) {
	r, ok := s.rolesByID[id]
	if !ok {
		return
	}
	delete(s.rolesByID, id)
	delete(s.rolesByName, r.Meta.Name)
	cascadeDelete(s, refRole, id)
}

// ── RoleBinding ───────────────────────────────────────────────────────────

func (c *Catalog) ApplyRoleBindingUpsert(b *rolebinding.RoleBinding) error {
	if !b.IsEnabled() {
		return c.ApplyRoleBindingDelete(b.Meta.ID)
	}
	if err := b.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizeRoleBinding(b, snapIDs(s.rolesByID), snapIDs(s.teamsByID), snapIDs(s.projectsByID))
	if !keep {
		deleteRoleBinding(s, b.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertRoleBinding(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRoleBindingDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRoleBinding(s, id)
	c.snap.Store(s)
	return nil
}

func insertRoleBinding(s *Snapshot, b *rolebinding.RoleBinding) {
	if old, ok := s.roleBindingsByID[b.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRoleBinding, ID: old.Meta.ID}, outboundRoleBindingRefs(old))
		delete(s.roleBindingsByID, old.Meta.ID)
		removeRoleBindingFromSubjects(s, old)
	}
	s.roleBindingsByID[b.Meta.ID] = b
	for i := range b.Spec.Subjects {
		key := b.Spec.Subjects[i].Key()
		list := append(s.roleBindingsBySubject[key], b)
		sortRoleBindings(list)
		s.roleBindingsBySubject[key] = list
	}
	s.registerRefs(refKey{Kind: refRoleBinding, ID: b.Meta.ID}, outboundRoleBindingRefs(b))
}

func deleteRoleBinding(s *Snapshot, id string) {
	b, ok := s.roleBindingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRoleBinding, ID: id}, outboundRoleBindingRefs(b))
	delete(s.roleBindingsByID, id)
	removeRoleBindingFromSubjects(s, b)
}

func removeRoleBindingFromSubjects(s *Snapshot, b *rolebinding.RoleBinding) {
	for i := range b.Spec.Subjects {
		key := b.Spec.Subjects[i].Key()
		list := s.roleBindingsBySubject[key]
		out := make([]*rolebinding.RoleBinding, 0, len(list))
		for _, cur := range list {
			if cur.Meta.ID != b.Meta.ID {
				out = append(out, cur)
			}
		}
		if len(out) == 0 {
			delete(s.roleBindingsBySubject, key)
			continue
		}
		s.roleBindingsBySubject[key] = out
	}
}

// ── PolicyBinding ─────────────────────────────────────────────────────────

func (c *Catalog) ApplyPolicyBindingUpsert(b *policybinding.PolicyBinding) error {
	if !b.IsEnabled() {
		return c.ApplyPolicyBindingDelete(b.Meta.ID)
	}
	if err := b.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizePolicyBinding(b, snapIDs(s.projectsByID), snapIDs(s.policiesByID))
	if !keep {
		deletePolicyBinding(s, b.Meta.ID)
		c.commitWithGrants(s)
		return nil
	}
	insertPolicyBinding(s, clean)
	c.commitWithGrants(s)
	return nil
}

func (c *Catalog) ApplyPolicyBindingDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deletePolicyBinding(s, id)
	c.commitWithGrants(s)
	return nil
}

func insertPolicyBinding(s *Snapshot, b *policybinding.PolicyBinding) {
	if old, ok := s.policyBindingsByID[b.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicyBinding, ID: old.Meta.ID}, outboundPolicyBindingRefs(old))
		delete(s.policyBindingsByID, old.Meta.ID)
		removePolicyBindingFromProject(s, old)
	}
	s.policyBindingsByID[b.Meta.ID] = b
	list := append(s.policyBindingsByProject[b.Spec.ProjectID], b)
	sortPolicyBindings(list)
	s.policyBindingsByProject[b.Spec.ProjectID] = list
	s.registerRefs(refKey{Kind: refPolicyBinding, ID: b.Meta.ID}, outboundPolicyBindingRefs(b))
}

func deletePolicyBinding(s *Snapshot, id string) {
	b, ok := s.policyBindingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refPolicyBinding, ID: id}, outboundPolicyBindingRefs(b))
	delete(s.policyBindingsByID, id)
	removePolicyBindingFromProject(s, b)
}

func removePolicyBindingFromProject(s *Snapshot, b *policybinding.PolicyBinding) {
	list := s.policyBindingsByProject[b.Spec.ProjectID]
	out := make([]*policybinding.PolicyBinding, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != b.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.policyBindingsByProject, b.Spec.ProjectID)
		return
	}
	s.policyBindingsByProject[b.Spec.ProjectID] = out
}

// ── Binding ───────────────────────────────────────────────────────────────

func deleteBinding(s *Snapshot, id string) {
	b, ok := s.bindingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refBinding, ID: id}, outboundBindingRefs(b))
	delete(s.bindingsByID, id)
	delete(s.bindingsByModelHost, b.Spec.ModelID+"|"+b.Spec.HostID)
	// Remove from the per-model list.
	list := s.bindingsByModel[b.Spec.ModelID]
	newList := make([]*binding.Binding, 0, len(list))
	for _, bnd := range list {
		if bnd.Meta.ID != id {
			newList = append(newList, bnd)
		}
	}
	if len(newList) == 0 {
		delete(s.bindingsByModel, b.Spec.ModelID)
	} else {
		s.bindingsByModel[b.Spec.ModelID] = newList
	}
}

// ── Team ──────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyTeamUpsert(t *team.Team) error {
	if !t.IsEnabled() {
		return c.ApplyTeamDelete(t.Meta.ID)
	}
	if err := t.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refTeam, t.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	insertTeam(s, t)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyTeamDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteTeam(s, id)
	c.snap.Store(s)
	return nil
}

func insertTeam(s *Snapshot, t *team.Team) {
	if old, ok := s.teamsByID[t.Meta.ID]; ok {
		delete(s.teamsByName, old.Meta.Name)
		delete(s.teamsByID, old.Meta.ID)
	}
	s.teamsByID[t.Meta.ID] = t
	s.teamsByName[t.Meta.Name] = t
}

func deleteTeam(s *Snapshot, id string) {
	t, ok := s.teamsByID[id]
	if !ok {
		return
	}
	delete(s.teamsByID, id)
	delete(s.teamsByName, t.Meta.Name)
	cascadeDelete(s, refTeam, id)
}

// ── Project ───────────────────────────────────────────────────────────────

func (c *Catalog) ApplyProjectUpsert(p *project.Project) error {
	if !p.IsEnabled() {
		return c.ApplyProjectDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refProject, p.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeProject(p, snapIDs(s.teamsByID))
	if !keep {
		deleteProject(s, p.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertProject(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyProjectDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteProject(s, id)
	c.snap.Store(s)
	return nil
}

func insertProject(s *Snapshot, p *project.Project) {
	if old, ok := s.projectsByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refProject, ID: old.Meta.ID}, outboundProjectRefs(old))
		delete(s.projectsByName, old.Meta.Name)
		delete(s.projectsByID, old.Meta.ID)
		removeProjectFromTeam(s, old)
	}
	s.projectsByID[p.Meta.ID] = p
	s.projectsByName[p.Meta.Name] = p
	insertProjectIntoTeam(s, p)
	s.registerRefs(refKey{Kind: refProject, ID: p.Meta.ID}, outboundProjectRefs(p))
}

func deleteProject(s *Snapshot, id string) {
	p, ok := s.projectsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refProject, ID: id}, outboundProjectRefs(p))
	delete(s.projectsByID, id)
	delete(s.projectsByName, p.Meta.Name)
	removeProjectFromTeam(s, p)
	cascadeDelete(s, refProject, id)
}

// insertProjectIntoTeam keeps projectsByTeam sorted by project name, the
// order build() produces.
func insertProjectIntoTeam(s *Snapshot, p *project.Project) {
	list := append(s.projectsByTeam[p.Spec.TeamID], p)
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	s.projectsByTeam[p.Spec.TeamID] = list
}

func removeProjectFromTeam(s *Snapshot, p *project.Project) {
	list := s.projectsByTeam[p.Spec.TeamID]
	out := make([]*project.Project, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != p.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.projectsByTeam, p.Spec.TeamID)
		return
	}
	s.projectsByTeam[p.Spec.TeamID] = out
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
// resolve in s. Pricings stay valid while their owning host and at least one
// target model still resolve, matching full snapshot sanitization.
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
		// pricingByModelHost) — just check host and any model presence.
		if _, ok := s.hostsByID[p.Meta.Owner.ID]; !ok {
			return false
		}
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; ok {
				return true
			}
		}
		return false
	case refRelayKey:
		rk, ok := s.keysByID[k.ID]
		if !ok {
			return true
		}
		return validateKeyInSnap(rk, s) == nil
	case refBinding:
		b, ok := s.bindingsByID[k.ID]
		if !ok {
			return true
		}
		_, modelOK := s.modelsByID[b.Spec.ModelID]
		_, hostOK := s.hostsByID[b.Spec.HostID]
		return modelOK && hostOK
	case refRateLimit:
		r, ok := s.rateLimitsByID[k.ID]
		if !ok {
			return true
		}
		return validateRateLimitInSnap(r, s) == nil
	case refProject:
		p, ok := s.projectsByID[k.ID]
		if !ok {
			return true
		}
		return validateProjectInSnap(p, s) == nil
	case refServiceAccount:
		sa, ok := s.serviceAccountsByID[k.ID]
		if !ok {
			return true
		}
		return validateServiceAccountInSnap(sa, s) == nil
	case refRoleBinding:
		b, ok := s.roleBindingsByID[k.ID]
		if !ok {
			return true
		}
		return validateRoleBindingInSnap(b, s) == nil
	case refPolicyBinding:
		b, ok := s.policyBindingsByID[k.ID]
		if !ok {
			return true
		}
		return validatePolicyBindingInSnap(b, s) == nil
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
		_, ok := s.keysByID[k.ID]
		return ok
	case refBinding:
		_, ok := s.bindingsByID[k.ID]
		return ok
	case refTeam:
		_, ok := s.teamsByID[k.ID]
		return ok
	case refProject:
		_, ok := s.projectsByID[k.ID]
		return ok
	case refServiceAccount:
		_, ok := s.serviceAccountsByID[k.ID]
		return ok
	case refRole:
		_, ok := s.rolesByID[k.ID]
		return ok
	case refRoleBinding:
		_, ok := s.roleBindingsByID[k.ID]
		return ok
	case refPolicyBinding:
		_, ok := s.policyBindingsByID[k.ID]
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
		deleteKey(s, k.ID)
	case refRateLimit:
		deleteRateLimit(s, k.ID)
	case refProvider:
		deleteProvider(s, k.ID)
	case refHost:
		deleteHost(s, k.ID)
	case refBinding:
		deleteBinding(s, k.ID)
	case refTeam:
		deleteTeam(s, k.ID)
	case refProject:
		deleteProject(s, k.ID)
	case refServiceAccount:
		deleteServiceAccount(s, k.ID)
	case refRole:
		deleteRole(s, k.ID)
	case refRoleBinding:
		deleteRoleBinding(s, k.ID)
	case refPolicyBinding:
		deletePolicyBinding(s, k.ID)
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
