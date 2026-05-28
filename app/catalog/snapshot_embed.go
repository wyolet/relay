package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
)

// EnabledHosts returns enabled hosts sorted by Meta.Name.
func (s *Snapshot) EnabledHosts() []*host.Host {
	out := make([]*host.Host, 0, len(s.hostsByID))
	for _, h := range s.hostsByID {
		if h.IsEnabled() {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// EnabledModels returns enabled models sorted by Meta.Name.
func (s *Snapshot) EnabledModels() []*model.Model {
	out := make([]*model.Model, 0, len(s.modelsByID))
	for _, m := range s.modelsByID {
		if m.IsEnabled() {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}
