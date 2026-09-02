package catalog

import (
	"time"

	"github.com/wyolet/relay/app/key"
)

func (s *Snapshot) addKeys(rks []*key.Key, pols, sas idSet) {
	now := time.Now()
	for _, k := range rks {
		clean, keep := sanitizeKey(k, pols, sas)
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
		s.subjectsByKey[clean.Meta.ID] = keySubjects(s, clean)
		s.registerRefs(refKey{Kind: refRelayKey, ID: clean.Meta.ID}, outboundKeyRefs(clean))
	}
}

// sanitizeKey keeps the key when it's policy-less (PolicyID empty — the
// inference settings flag decides runtime behavior) or when its target
// Policy resolves. A serviceaccount principal must resolve too; user
// principals are unchecked because users aren't in the snapshot.
func sanitizeKey(k *key.Key, pols, sas idSet) (*key.Key, bool) {
	if k.Spec.Principal.Kind == key.PrincipalServiceAccount {
		if _, ok := sas[k.Spec.Principal.ID]; !ok {
			return nil, false
		}
	}
	if k.Spec.PolicyID != "" {
		if _, ok := pols[k.Spec.PolicyID]; !ok {
			return nil, false
		}
	}
	clean := *k
	return &clean, true
}
