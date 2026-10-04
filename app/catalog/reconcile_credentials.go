package catalog

import (
	"log/slog"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
)

// ── HostKey ───────────────────────────────────────────────────────────────

func (c *Catalog) ApplyHostKeyUpsert(k *hostkey.HostKey) error {
	if !k.IsEnabled() {
		return c.ApplyHostKeyDelete(k.Meta.ID)
	}
	if err := k.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refHostKey, k.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeHostKey(k, snapIDs(s.hostsByID), snapIDs(s.projectsByID), s.policyLookup)
	if !keep {
		deleteHostKey(s, k.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertHostKey(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyHostKeyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteHostKey(s, id)
	c.snap.Store(s)
	return nil
}

func insertHostKey(s *Snapshot, k *hostkey.HostKey) {
	if old, ok := s.hostKeysByID[k.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refHostKey, ID: old.Meta.ID}, outboundHostKeyRefs(old))
		for polID, keys := range s.hostKeysByPolicy {
			s.hostKeysByPolicy[polID] = removeHostKeyFromSlice(keys, old.Meta.ID)
		}
		delete(s.hostKeysByID, old.Meta.ID)
	}
	s.hostKeysByID[k.Meta.ID] = k
	s.registerRefs(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	rebuildHostKeysByPolicy(s)
	s.rebuildHostKeysByHost()
}

func deleteHostKey(s *Snapshot, id string) {
	k, ok := s.hostKeysByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refHostKey, ID: id}, outboundHostKeyRefs(k))
	delete(s.hostKeysByID, id)
	for polID, keys := range s.hostKeysByPolicy {
		s.hostKeysByPolicy[polID] = removeHostKeyFromSlice(keys, id)
	}
	s.rebuildHostKeysByHost()
	cascadeDelete(s, refHostKey, id)
	resanitizePoliciesAfterParentChange(s)
}

// ── Key ──────────────────────────────────────────────────────────────

func (c *Catalog) ApplyKeyUpsert(k *key.Key) error {
	if !k.IsEnabled() {
		// Disabling stops the key routing but leaves its hashes in its
		// owner's read scope — the traffic it already produced is theirs.
		if err := c.ApplyKeyDelete(k.Meta.ID); err != nil {
			return err
		}
		c.rmu.Lock()
		defer c.rmu.Unlock()
		s := c.snap.Load().clone()
		s.indexUserKeyHashes(k)
		c.snap.Store(s)
		return nil
	}
	if err := k.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizeKey(k, s.policyResolvable, snapIDs(s.serviceAccountsByID))
	if !keep {
		deleteKey(s, k.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertKey(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyKeyDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteKey(s, id)
	c.snap.Store(s)
	return nil
}

func insertKey(s *Snapshot, k *key.Key) {
	if old, ok := s.keysByID[k.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRelayKey, ID: old.Meta.ID}, outboundKeyRefs(old))
		unindexKeyHashes(s, old)
		deindexKeyPrincipal(s, old)
		delete(s.keysByID, old.Meta.ID)
	}
	s.keysByID[k.Meta.ID] = k
	s.indexUserKeyHashes(k)
	indexKeyPrincipal(s, k)
	s.subjectsByKey[k.Meta.ID] = keySubjects(s, k)
	if k.Spec.KeyHash != "" {
		indexKeyHash(s, k.Spec.KeyHash, k)
	}
	// The pre-rotation hash is indexed only while its grace window is open,
	// so an expired grace drops out on the next reconcile or reload.
	if k.InGrace(s.clock()) {
		indexKeyHash(s, k.Spec.PreviousKeyHash, k)
	}
	s.registerRefs(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundKeyRefs(k))
}

// indexKeyHash claims a hash slot for k. A slot already held by a different
// key is left alone: overwriting it would move that key's live traffic
// (including a rotation grace window) onto this one.
func indexKeyHash(s *Snapshot, hash string, k *key.Key) {
	if held, ok := s.keysByHash[hash]; ok && held.Meta.ID != k.Meta.ID {
		slog.Warn("catalog: key hash already indexed by another key; keeping the first",
			"held_key", held.Meta.ID, "rejected_key", k.Meta.ID)
		return
	}
	s.keysByHash[hash] = k
}

func deleteKey(s *Snapshot, id string) {
	k, ok := s.keysByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRelayKey, ID: id}, outboundKeyRefs(k))
	unindexKeyHashes(s, k)
	s.dropUserKeyHashes(k)
	deindexKeyPrincipal(s, k)
	delete(s.keysByID, id)
	delete(s.subjectsByKey, id)
}

// unindexKeyHashes drops only the slots this key actually holds — a hash it
// lost to another key (see indexKeyHash) still belongs to that one.
func unindexKeyHashes(s *Snapshot, k *key.Key) {
	for _, h := range []string{k.Spec.KeyHash, k.Spec.PreviousKeyHash} {
		if h == "" {
			continue
		}
		if held, ok := s.keysByHash[h]; ok && held.Meta.ID == k.Meta.ID {
			delete(s.keysByHash, h)
		}
	}
}
