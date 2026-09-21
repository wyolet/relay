package catalog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

func hashKey(id, name, hash, prev string, grace *time.Time) *key.Key {
	k := &key.Key{Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}}}
	k.Spec.KeyHash = hash
	k.Spec.PreviousKeyHash = prev
	k.Spec.GraceUntil = grace
	return k
}

// Two rows can hold the same hash only through a raw DB edit or a race with
// the store's guard. When it happens, the key that already owns the slot
// keeps its traffic — silently reassigning it is the worst outcome.
func TestKeyHashSlotIsNotStolen(t *testing.T) {
	s := Build(nil, nil, nil, nil, nil, nil, nil, nil, nil).clone()

	first := hashKey("k-1", "first", "hash-a", "", nil)
	insertKey(s, first)

	grace := time.Now().Add(time.Hour)
	second := hashKey("k-2", "second", "hash-a", "hash-a", &grace)
	insertKey(s, second)

	held, ok := s.keysByHash["hash-a"]
	if !ok || held.Meta.ID != first.Meta.ID {
		t.Fatalf("hash-a is held by %+v, want the first key", held)
	}

	// Deleting the interloper must not take the slot with it.
	deleteKey(s, second.Meta.ID)
	held, ok = s.keysByHash["hash-a"]
	if !ok || held.Meta.ID != first.Meta.ID {
		t.Fatalf("after deleting the second key, hash-a is %+v, want the first key", held)
	}

	// A key still owns its own slots.
	deleteKey(s, first.Meta.ID)
	if _, ok := s.keysByHash["hash-a"]; ok {
		t.Error("deleting the owning key left its hash indexed")
	}
}
