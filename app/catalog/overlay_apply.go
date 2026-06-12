// Overlay application — the single place TEMPLATE rows become EFFECTIVE
// rows. Overlays (app/overlay) are user-owned sparse spec patches; the
// merge happens here at snapshot build/reconcile time, never in storage,
// so re-seeding templates is overlay-unaware and user customizations
// survive catalog upgrades. See docs/overlays.md.
package catalog

import (
	"log/slog"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
)

// applyOverlays registers ovls and swaps every overlaid model's snapshot
// entry for its effective row. Build-time path: runs after addModels and
// before snapshot indexing, so aliases/refs index the effective spec.
func (s *Snapshot) applyOverlays(ovls []*overlay.Overlay) {
	for _, o := range ovls {
		s.overlaysByTarget[o.Key()] = o
	}
	if len(s.overlaysByTarget) == 0 {
		return
	}
	for id, m := range s.modelsByID {
		eff := s.overlaidModel(m)
		if eff == m {
			continue
		}
		s.modelsByID[id] = eff
		swapModelInSlice(s.modelsByName[m.Meta.Name], eff)
	}
}

// overlaidModel returns the effective row for template m: the merge with
// its overlay when one exists (stashing the template for later
// re-derivation), or m itself. An invalid merge QUARANTINES the overlay:
// the pristine template is served and the failure logged — a bad overlay
// must never take the model (or the snapshot) down.
func (s *Snapshot) overlaidModel(m *model.Model) *model.Model {
	o, ok := s.overlaysByTarget[overlay.TargetKey(overlay.KindModel, m.Meta.ID)]
	if !ok {
		delete(s.modelTemplates, m.Meta.ID)
		return m
	}
	if s.modelTemplates == nil {
		s.modelTemplates = map[string]*model.Model{}
	}
	s.modelTemplates[m.Meta.ID] = m
	eff, err := overlay.EffectiveModel(m, o)
	if err != nil {
		slog.Warn("catalog: overlay quarantined — serving pristine template",
			"model", m.Meta.Name, "id", m.Meta.ID, "err", err)
		return m
	}
	return eff
}

// templateModel returns the pristine template for a model id: the
// stashed template when an overlay is applied, else the live row (which
// IS the template when no overlay exists).
func (s *Snapshot) templateModel(id string) *model.Model {
	if t, ok := s.modelTemplates[id]; ok {
		return t
	}
	return s.modelsByID[id]
}

// ApplyOverlayUpsert installs (or replaces) an overlay and re-derives
// the target's effective row. An overlay whose target isn't in the
// snapshot is registered inert — it merges when the target appears.
func (c *Catalog) ApplyOverlayUpsert(o *overlay.Overlay) error {
	if err := o.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	s.overlaysByTarget[o.Key()] = o
	if o.Kind == overlay.KindModel {
		if tmpl := s.templateModel(o.ResourceID); tmpl != nil {
			insertModel(s, s.overlaidModel(tmpl))
		}
	}
	c.commitWithGrants(s)
	return nil
}

// ApplyOverlayDelete removes an overlay and restores the target's
// pristine template (factory reset).
func (c *Catalog) ApplyOverlayDelete(kind, resourceID string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	delete(s.overlaysByTarget, overlay.TargetKey(kind, resourceID))
	if kind == overlay.KindModel {
		if tmpl := s.templateModel(resourceID); tmpl != nil {
			insertModel(s, s.overlaidModel(tmpl))
		}
	}
	c.commitWithGrants(s)
	return nil
}

// swapModelInSlice replaces the entry with eff's id in place.
func swapModelInSlice(models []*model.Model, eff *model.Model) {
	for i, m := range models {
		if m.Meta.ID == eff.Meta.ID {
			models[i] = eff
			return
		}
	}
}
