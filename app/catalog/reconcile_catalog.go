package catalog

import (
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
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
	if handled, err := c.recoverAbsentLocked(refProvider, p.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()

	// Remove old entry if present.
	renamed := false
	if old, ok := s.providersByID[p.Meta.ID]; ok {
		renamed = old.Meta.Name != p.Meta.Name
		delete(s.providersByName, old.Meta.Name)
		delete(s.providersByID, old.Meta.ID)
	}
	s.providersByID[p.Meta.ID] = p
	s.providersByName[p.Meta.Name] = p
	if renamed {
		for _, m := range s.modelsByID {
			if m.Meta.Owner.ID == p.Meta.ID {
				s.reindexModelSnapshots(m)
			}
		}
	}
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
	if !h.IsEnabled() || !notTenantOwned("host", h.Meta) {
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
	renamed := false
	if old, ok := s.hostsByID[h.Meta.ID]; ok {
		renamed = old.Meta.Name != clean.Meta.Name
		delete(s.hostsByName, old.Meta.Name)
		delete(s.hostsByID, old.Meta.ID)
	}
	s.hostsByID[clean.Meta.ID] = clean
	s.hostsByName[clean.Meta.Name] = clean
	if renamed {
		for _, b := range s.bindingsByID {
			if m, ok := s.modelsByID[b.Spec.ModelID]; ok && b.Spec.HostID == clean.Meta.ID {
				s.reindexModelSnapshots(m)
			}
		}
	}
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
