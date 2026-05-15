package catalog

import (
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

func (s *Snapshot) addHosts(hosts []*host.Host, polByID map[string]*policy.Policy) {
	for _, h := range hosts {
		clean := sanitizeHost(h, polByID)
		s.hostsByID[clean.Meta.ID] = clean
		s.hostsByName[clean.Meta.Name] = clean
	}
}

// sanitizeHost drops Spec.Policies entries that don't resolve to a host-
// owned Policy of THIS host, and clears DefaultPolicy if it isn't in the
// resulting menu. PG still has the full Spec; snapshot reflects only the
// usable subset.
func sanitizeHost(h *host.Host, polByID map[string]*policy.Policy) *host.Host {
	clean := *h
	clean.Spec = h.Spec
	if len(h.Spec.Policies) > 0 {
		menu := make([]string, 0, len(h.Spec.Policies))
		for _, polID := range h.Spec.Policies {
			pol, ok := polByID[polID]
			if !ok {
				continue
			}
			if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != h.Meta.ID {
				continue
			}
			menu = append(menu, polID)
		}
		if len(menu) == 0 {
			clean.Spec.Policies = nil
		} else {
			clean.Spec.Policies = menu
		}
	}
	if clean.Spec.DefaultPolicy != "" {
		found := false
		for _, id := range clean.Spec.Policies {
			if id == clean.Spec.DefaultPolicy {
				found = true
				break
			}
		}
		if !found {
			clean.Spec.DefaultPolicy = ""
		}
	}
	return &clean
}
