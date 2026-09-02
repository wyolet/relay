package catalog

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
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

// hostKeyCatalog wires a catalog holding one host, its tier policy and the
// supplied host keys.
func hostKeyCatalog(t *testing.T, hostID string, keys ...*hostkey.HostKey) *Catalog {
	t.Helper()
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.openai.com"},
	}
	tier := &policy.Policy{Meta: meta.Metadata{ID: "tier", Name: "tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}}
	c := New(provList{}, hostList{h}, polList{tier}, modList{}, keyList(keys), rlList{},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

// A host key whose secret could not be resolved is dropped from the
// snapshot instead of failing the whole build (R5-1).
func TestUnresolvedHostKeyDroppedFromSnapshot(t *testing.T) {
	hostID := meta.NewID()
	good := &hostkey.HostKey{
		Meta:     meta.Metadata{ID: meta.NewID(), Name: "good", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec:     hostkey.Spec{HostID: hostID, PolicyID: "tier", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "GOOD"}},
		Resolved: "sk-good",
	}
	bad := &hostkey.HostKey{
		Meta:   meta.Metadata{ID: meta.NewID(), Name: "bad", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec:   hostkey.Spec{HostID: hostID, PolicyID: "tier", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "MISSING"}},
		Status: hostkey.Status{Unresolved: &hostkey.UnresolvedStatus{Reason: "env MISSING not set"}},
	}

	c := hostKeyCatalog(t, hostID, good, bad)
	s := c.Current()
	if _, ok := s.HostKey(good.Meta.ID); !ok {
		t.Fatalf("resolvable key missing from the snapshot")
	}
	if _, ok := s.HostKey(bad.Meta.ID); ok {
		t.Fatalf("unresolved key %s must not reach the snapshot", bad.Meta.ID)
	}

	// The incremental NOTIFY path drops it too.
	if err := c.ApplyHostKeyUpsert(bad); err != nil {
		t.Fatalf("apply unresolved key: %v", err)
	}
	if _, ok := c.Current().HostKey(bad.Meta.ID); ok {
		t.Fatalf("unresolved key entered the snapshot through the NOTIFY path")
	}
}
