package catalog

import (
	"log/slog"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

func (s *Snapshot) addHostKeys(keys []*hostkey.HostKey, hosts, projects idSet) {
	for _, k := range keys {
		clean, keep := sanitizeHostKey(k, hosts, projects)
		if !keep {
			continue
		}
		s.hostKeysByID[clean.Meta.ID] = clean
		s.registerRefs(refKey{Kind: refHostKey, ID: clean.Meta.ID}, outboundHostKeyRefs(k))
	}
}

// sanitizeHostKey drops the key when its value could not be resolved, when
// its Host can't resolve, or when a project-owned key's Project is missing.
// Every one of those refs is required for the key to function. The
// unresolved value is logged, once per key per build.
func sanitizeHostKey(k *hostkey.HostKey, hosts, projects idSet) (*hostkey.HostKey, bool) {
	if k.Status.Unresolved != nil {
		slog.Warn("catalog: host key dropped, value unresolved",
			"id", k.Meta.ID, "name", k.Meta.Name, "reason", k.Status.Unresolved.Reason)
		return nil, false
	}
	if k.Meta.Owner.Kind == meta.OwnerProject {
		if !projects(k.Meta.Owner.ID) {
			return nil, false
		}
	}
	if !hosts(k.Spec.HostID) {
		return nil, false
	}
	clean := *k
	return &clean, true
}
