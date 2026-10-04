package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

// HostKeysForHost reads a materialized index and answers the same
// enabled, slug-ordered pool the per-call scan did.
func TestHostKeysForHostUsesIndex(t *testing.T) {
	hostID := meta.NewID()
	off := false
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "h", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h"}}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}}
	mk := func(name string, enabled *bool) *hostkey.HostKey {
		return &hostkey.HostKey{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{
				HostID: hostID, PolicyID: tier.Meta.ID, Value: "sk", Enabled: enabled,
				ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored},
			},
		}
	}
	zed, alpha, dead := mk("zed", nil), mk("alpha", nil), mk("dead", &off)
	c := New(provList{}, hostList{h}, polList{tier}, modList{}, keyList{zed, alpha, dead}, rlList{},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := c.Current().HostKeysForHost(hostID)
	if len(got) != 2 || got[0].Meta.Name != "alpha" || got[1].Meta.Name != "zed" {
		t.Fatalf("pool = %v, want [alpha zed]", names(got))
	}
	if err := c.ApplyHostKeyDelete(alpha.Meta.ID); err != nil {
		t.Fatalf("delete host key: %v", err)
	}
	got = c.Current().HostKeysForHost(hostID)
	if len(got) != 1 || got[0].Meta.Name != "zed" {
		t.Fatalf("pool after delete = %v, want [zed]", names(got))
	}
}

func names(keys []*hostkey.HostKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Meta.Name)
	}
	return out
}

// BenchmarkHostKeysForHost is the evidence for the index: the policy-less
// pool lookup turned from a per-call scan-and-sort into a slice-header read.
func BenchmarkHostKeysForHost(b *testing.B) {
	hostID := meta.NewID()
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "h", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h"}}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}}
	keys := make([]*hostkey.HostKey, 0, 64)
	for i := 0; i < 64; i++ {
		keys = append(keys, &hostkey.HostKey{
			Meta: meta.Metadata{ID: meta.NewID(), Name: string(rune('a'+i%26)) + meta.NewID(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: hostID, PolicyID: tier.Meta.ID, Value: "sk", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
		})
	}
	c := New(provList{}, hostList{h}, polList{tier}, modList{}, keyList(keys), rlList{},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	s := c.Current()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(s.HostKeysForHost(hostID)) != 64 {
			b.Fatal("unexpected pool size")
		}
	}
}
