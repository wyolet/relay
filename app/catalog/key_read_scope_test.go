package catalog

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// A user's own read scope covers every key they own, disabled ones
// included: disabling a key does not un-own the traffic it produced.
func TestKeyHashesForUserCoversDisabledKeys(t *testing.T) {
	userID := meta.NewID()
	off := false
	live := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "live", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: userID}, KeyHash: "hash-live"},
	}
	disabled := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "old", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
			KeyHash:   "hash-old", Enabled: &off,
		},
	}
	c := New(provList{}, hostList{}, polList{}, modList{}, keyList{}, rlList{},
		rkList{live, disabled}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	hashes := c.Current().KeyHashesForUser(userID)
	for _, want := range []string{"hash-live", "hash-old"} {
		if !slices.Contains(hashes, want) {
			t.Errorf("hashes = %v, want %q included", hashes, want)
		}
	}

	// Disabling a live key through the NOTIFY path keeps its hash too.
	off2 := false
	turnedOff := *live
	turnedOff.Spec.Enabled = &off2
	if err := c.ApplyKeyUpsert(&turnedOff); err != nil {
		t.Fatalf("disable key: %v", err)
	}
	if _, ok := c.Current().KeyByHash("hash-live"); ok {
		t.Error("a disabled key still routes")
	}
	if !slices.Contains(c.Current().KeyHashesForUser(userID), "hash-live") {
		t.Error("disabling dropped the key from its owner's read scope")
	}
}
