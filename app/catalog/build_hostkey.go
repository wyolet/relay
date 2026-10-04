package catalog

import (
	"log/slog"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

func (s *Snapshot) addHostKeys(keys []*hostkey.HostKey, hosts, projects idSet, policyOf policyLookupFn) {
	for _, k := range keys {
		clean, keep := sanitizeHostKey(k, hosts, projects, policyOf)
		if !keep {
			continue
		}
		s.hostKeysByID[clean.Meta.ID] = clean
		s.registerRefs(refKey{Kind: refHostKey, ID: clean.Meta.ID}, outboundHostKeyRefs(k))
	}
}

// sanitizeHostKey drops the key when its value could not be resolved, when
// either its Host or its tier Policy can't resolve, when the Policy isn't
// host-owned by the key's Host, or when a project-owned key's Project is
// missing. Every one of those refs is required for the key to function.
// The two drops an operator can act on are logged, once per key per build.
func sanitizeHostKey(k *hostkey.HostKey, hosts, projects idSet, policyOf policyLookupFn) (*hostkey.HostKey, bool) {
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
	pol, ok := policyOf(k.Spec.PolicyID)
	if !ok {
		return nil, false
	}
	if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != k.Spec.HostID {
		slog.Warn("catalog: host key dropped, policy is not host-owned by the key's host",
			"id", k.Meta.ID, "name", k.Meta.Name, "policy", pol.Meta.Name,
			"host", k.Spec.HostID, "owner", string(pol.Meta.Owner.Kind)+"/"+pol.Meta.Owner.ID)
		return nil, false
	}
	clean := *k
	return &clean, true
}
