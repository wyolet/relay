package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

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
// snapshot instead of failing the whole build.
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
