package catalog

import "github.com/wyolet/relay/app/model"

func (s *Snapshot) addModels(models []*model.Model, providers idSet) {
	for _, m := range models {
		clean, keep := sanitizeModel(m, providers)
		if !keep {
			continue
		}
		s.modelsByID[clean.Meta.ID] = clean
		s.modelsByName[clean.Meta.Name] = append(s.modelsByName[clean.Meta.Name], clean)
		// indexModelSnapshots is deferred to after bindings are added (build.go)
		// — host-pinned aliases read BindingsForModel, which needs bindings.
		s.registerRefs(refKey{Kind: refModel, ID: clean.Meta.ID}, outboundModelRefs(m))
	}
}

// sanitizeModel drops the model if its owning Provider isn't enabled.
// Deprecation.Replacement is left as-is (informational, not a hot-path lookup).
func sanitizeModel(m *model.Model, providers idSet) (*model.Model, bool) {
	if _, ok := providers[m.Meta.Owner.ID]; !ok {
		return nil, false
	}
	clean := *m
	clean.Spec = m.Spec
	return &clean, true
}
