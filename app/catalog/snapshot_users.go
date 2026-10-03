package catalog

import (
	"slices"

	"github.com/wyolet/relay/app/key"
)

// KeyHashesForUser returns the hashes every Key of this user authenticates
// under, the pre-rotation one included. Read from the principal index, so it
// costs the user's own keys rather than a walk of the deployment's.
func (s *Snapshot) KeyHashesForUser(userID string) []string {
	return s.hashesByUser[userID]
}

// indexUserKeyHashes records the hashes k authenticates under against its
// user principal. Called for every key row, enabled or not, so a disabled
// key stays inside its owner's read scope even though it is in no routing
// index. A hard delete of a disabled key leaves its entry behind until the
// next full reload rebuilds the map.
func (s *Snapshot) indexUserKeyHashes(k *key.Key) {
	if k.Spec.Principal.Kind != key.PrincipalUser || k.Spec.Principal.ID == "" {
		return
	}
	have := s.hashesByUser[k.Spec.Principal.ID]
	next := have
	for _, h := range []string{k.Spec.KeyHash, k.Spec.PreviousKeyHash} {
		if h == "" || slices.Contains(next, h) {
			continue
		}
		if len(next) == len(have) {
			// Copy on first append: the header is shared with the snapshot
			// this one was cloned from.
			next = append(append(make([]string, 0, len(have)+2), have...), h)
			continue
		}
		next = append(next, h)
	}
	if len(next) != len(have) {
		s.hashesByUser[k.Spec.Principal.ID] = next
	}
}

// dropUserKeyHashes removes the hashes k held from its owner's list.
func (s *Snapshot) dropUserKeyHashes(k *key.Key) {
	if k.Spec.Principal.Kind != key.PrincipalUser || k.Spec.Principal.ID == "" {
		return
	}
	have := s.hashesByUser[k.Spec.Principal.ID]
	out := make([]string, 0, len(have))
	for _, h := range have {
		if h != k.Spec.KeyHash && h != k.Spec.PreviousKeyHash {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		delete(s.hashesByUser, k.Spec.Principal.ID)
		return
	}
	s.hashesByUser[k.Spec.Principal.ID] = out
}

// SubjectsForKey returns the precomputed subjects the key's principal acts
// under. The returned slice must not be mutated.
func (s *Snapshot) SubjectsForKey(keyID string) []string {
	return s.subjectsByKey[keyID]
}

// TokenVersion returns the user's current token version. Absent (ok=false)
// means the user is unknown to this snapshot, which invalidates any token
// claiming to be theirs.
func (s *Snapshot) TokenVersion(userID string) (int, bool) {
	v, ok := s.tokenVersionByUser[userID]
	return v, ok
}

// UserEnabled reports whether a user may still act. True when no users
// source is attached: the snapshot cannot tell, and keys predate users.
func (s *Snapshot) UserEnabled(userID string) bool {
	if !s.usersLoaded {
		return true
	}
	_, ok := s.tokenVersionByUser[userID]
	return ok
}
