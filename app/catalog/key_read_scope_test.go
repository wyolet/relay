package catalog

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
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
	must(t, c.Reload(context.Background()))
	hashes := slices.Sorted(slices.Values(c.Current().KeyHashesForUser(userID)))
	if want := []string{"hash-live", "hash-old"}; !slices.Equal(hashes, want) {
		t.Errorf("hashes = %v, want %v", hashes, want)
	}
	if got := c.Current().KeyHashesForUser(meta.NewID()); len(got) != 0 {
		t.Errorf("another user's read scope holds %v", got)
	}

	// Disabling a live key through the NOTIFY path keeps its hash too.
	off2 := false
	turnedOff := *live
	turnedOff.Spec.Enabled = &off2
	must(t, c.ApplyKeyUpsert(&turnedOff))
	if _, ok := c.Current().KeyByHash("hash-live"); ok {
		t.Error("a disabled key still routes")
	}
	if !slices.Contains(c.Current().KeyHashesForUser(userID), "hash-live") {
		t.Error("disabling dropped the key from its owner's read scope")
	}
}
