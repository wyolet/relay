package catalog

import (
	"github.com/wyolet/relay/app/key"
)

func (s *Snapshot) addKeys(rks []*key.Key, sas idSet) {
	now := s.clock()
	for _, k := range rks {
		s.indexUserKeyHashes(k)
		clean, keep := sanitizeKey(k, sas)
		if !keep {
			continue
		}
		s.keysByID[clean.Meta.ID] = clean
		if clean.Spec.KeyHash != "" {
			s.keysByHash[clean.Spec.KeyHash] = clean
		}
		if clean.InGrace(now) {
			s.keysByHash[clean.Spec.PreviousKeyHash] = clean
		}
		indexKeyPrincipal(s, clean)
		s.subjectsByKey[clean.Meta.ID] = keySubjects(s, clean)
		s.registerRefs(refKey{Kind: refRelayKey, ID: clean.Meta.ID}, outboundKeyRefs(clean))
	}
}

// sanitizeKey drops a key whose service-account principal does not resolve;
// user principals are unchecked because users aren't in the snapshot.
func sanitizeKey(k *key.Key, sas idSet) (*key.Key, bool) {
	if k.Spec.Principal.Kind == key.PrincipalServiceAccount {
		if !sas(k.Spec.Principal.ID) {
			return nil, false
		}
	}
	clean := *k
	return &clean, true
}
