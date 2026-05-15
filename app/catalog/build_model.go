package catalog

import "github.com/wyolet/relay/app/model"

func (s *Snapshot) addModels(models []*model.Model, providers, hosts idSet) {
	for _, m := range models {
		clean, keep := sanitizeModel(m, providers, hosts)
		if !keep {
			continue
		}
		s.modelsByID[clean.Meta.ID] = clean
		s.modelsByName[clean.Meta.Name] = append(s.modelsByName[clean.Meta.Name], clean)
		for _, a := range clean.Spec.Aliases {
			if a == clean.Meta.Name {
				continue
			}
			s.modelsByName[a] = append(s.modelsByName[a], clean)
		}
		s.registerRefs(refKey{Kind: refModel, ID: clean.Meta.ID}, outboundModelRefs(m))
	}
}

// sanitizeModel drops the model if its owning Provider isn't enabled.
// Otherwise filters HostBindings to enabled Hosts only. Deprecation.Replacement
// is left as-is (informational, not a hot-path lookup).
func sanitizeModel(m *model.Model, providers, hosts idSet) (*model.Model, bool) {
	if _, ok := providers[m.Meta.Owner.ID]; !ok {
		return nil, false
	}
	clean := *m
	clean.Spec = m.Spec
	if len(m.Spec.Hosts) > 0 {
		hs := make([]model.HostBinding, 0, len(m.Spec.Hosts))
		for _, b := range m.Spec.Hosts {
			if _, ok := hosts[b.HostID]; !ok {
				continue
			}
			hs = append(hs, b)
		}
		clean.Spec.Hosts = hs
	}
	return &clean, true
}
