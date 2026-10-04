package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
)

// HostKey returns the enabled HostKey with this id, or false.
func (s *Snapshot) HostKey(id string) (*hostkey.HostKey, bool) {
	k, ok := s.hostKeysByID[id]
	return k, ok
}

// AllHostKeys returns every HostKey in the snapshot, sorted by slug.
func (s *Snapshot) AllHostKeys() []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, 0, len(s.hostKeysByID))
	for _, k := range s.hostKeysByID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllKeys returns every Key in the snapshot, sorted by slug.
func (s *Snapshot) AllKeys() []*key.Key {
	out := make([]*key.Key, 0, len(s.keysByID))
	for _, k := range s.keysByID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// HostKeysForHost returns every enabled HostKey whose Spec.HostID
// matches hostID. Order is by hostkey slug — stable across snapshots.
// Used by routing's policy-less flow (settings.Inference.AllowMissingPolicy)
// where the policy doesn't narrow the pool. The returned slice is the
// snapshot's own: callers must not mutate or append to it.
func (s *Snapshot) HostKeysForHost(hostID string) []*hostkey.HostKey {
	return s.hostKeysByHost[hostID]
}

// rebuildHostKeysByHost recomputes the per-host pool from hostKeysByID.
// Cheap enough to run whole on any host-key write — the map is small and
// this runs off the request path.
func (s *Snapshot) rebuildHostKeysByHost() {
	byHost := make(map[string][]*hostkey.HostKey, len(s.hostKeysByHost))
	for _, k := range s.hostKeysByID {
		if !k.IsEnabled() {
			continue
		}
		byHost[k.Spec.HostID] = append(byHost[k.Spec.HostID], k)
	}
	for _, list := range byHost {
		sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	}
	s.hostKeysByHost = byHost
}

// Key returns the enabled Key with this id, or false.
func (s *Snapshot) Key(id string) (*key.Key, bool) {
	k, ok := s.keysByID[id]
	return k, ok
}

// KeyByHash is the hot-path inbound-auth lookup. matchedPrevious is true
// when the hash matched the pre-rotation credential, which the caller must
// then check against the grace window. Caller checks IsActive.
func (s *Snapshot) KeyByHash(hash string) (k *key.Key, matchedPrevious bool) {
	k, ok := s.keysByHash[hash]
	if !ok {
		return nil, false
	}
	return k, k.Spec.KeyHash != hash
}
