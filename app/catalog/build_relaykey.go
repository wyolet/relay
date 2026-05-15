package catalog

import "github.com/wyolet/relay/app/relaykey"

func (s *Snapshot) addRelayKeys(rks []*relaykey.RelayKey, pols idSet) {
	for _, k := range rks {
		clean, keep := sanitizeRelayKey(k, pols)
		if !keep {
			continue
		}
		s.relayKeysByID[clean.Meta.ID] = clean
		if clean.Spec.KeyHash != "" {
			s.relayKeysByHash[clean.Spec.KeyHash] = clean
		}
		s.registerRefs(refKey{Kind: refRelayKey, ID: clean.Meta.ID}, outboundRelayKeyRefs(k))
	}
}

// sanitizeRelayKey drops the key when its target Policy isn't resolvable
// — without a policy the inbound auth has no grants to apply.
func sanitizeRelayKey(k *relaykey.RelayKey, pols idSet) (*relaykey.RelayKey, bool) {
	if _, ok := pols[k.Spec.PolicyID]; !ok {
		return nil, false
	}
	clean := *k
	return &clean, true
}
