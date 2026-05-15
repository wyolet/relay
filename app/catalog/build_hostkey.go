package catalog

import (
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

func (s *Snapshot) addHostKeys(keys []*hostkey.HostKey, hosts idSet, polByID map[string]*policy.Policy) {
	for _, k := range keys {
		clean, keep := sanitizeHostKey(k, hosts, polByID)
		if !keep {
			continue
		}
		s.hostKeysByID[clean.Meta.ID] = clean
		s.registerRefs(refKey{Kind: refHostKey, ID: clean.Meta.ID}, outboundHostKeyRefs(k))
	}
}

// sanitizeHostKey drops the key when either its Host or its tier Policy
// can't resolve, or when the Policy isn't host-owned by the key's Host.
// Both refs are required for the key to function.
func sanitizeHostKey(k *hostkey.HostKey, hosts idSet, polByID map[string]*policy.Policy) (*hostkey.HostKey, bool) {
	if _, ok := hosts[k.Spec.HostID]; !ok {
		return nil, false
	}
	pol, ok := polByID[k.Spec.PolicyID]
	if !ok {
		return nil, false
	}
	if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != k.Spec.HostID {
		return nil, false
	}
	clean := *k
	return &clean, true
}
