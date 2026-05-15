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

// sanitizeRelayKey keeps the key when it's policy-less (PolicyID empty —
// the inference settings flag decides runtime behavior) or when its
// target Policy resolves. Drops it only when PolicyID is set but
// points at a row that doesn't exist or is disabled.
func sanitizeRelayKey(k *relaykey.RelayKey, pols idSet) (*relaykey.RelayKey, bool) {
	if k.Spec.PolicyID == "" {
		clean := *k
		return &clean, true
	}
	if _, ok := pols[k.Spec.PolicyID]; !ok {
		return nil, false
	}
	clean := *k
	return &clean, true
}
