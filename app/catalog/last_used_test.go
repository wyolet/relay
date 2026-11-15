package catalog

import (
	"context"
	"testing"
	"time"
)

// A key's last-used time is runtime state: neither a load nor an applied write carries it into the snapshot.
func TestSnapshotLeavesLastUsedOut(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	used := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	keys[0].Status.LastUsedAt = &used
	rks[0].Status.LastUsedAt = &used
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	must(t, c.Reload(context.Background()))
	check := func(when string) {
		t.Helper()
		s := c.Current()
		if hk, ok := s.HostKey(keys[0].Meta.ID); !ok || hk.Status.LastUsedAt != nil {
			t.Errorf("%s: host key in snapshot = %v (present %v), want no lastUsedAt", when, hk, ok)
		}
		if k, ok := s.Key(rks[0].Meta.ID); !ok || k.Status.LastUsedAt != nil {
			t.Errorf("%s: key in snapshot = %v (present %v), want no lastUsedAt", when, k, ok)
		}
	}
	check("load")

	hk, k := *keys[0], *rks[0]
	must(t, c.ApplyHostKeyUpsert(&hk))
	must(t, c.ApplyKeyUpsert(&k))
	check("upsert")
	if keys[0].Status.LastUsedAt == nil || rks[0].Status.LastUsedAt == nil {
		t.Error("the snapshot cleared the stored rows' lastUsedAt")
	}
}
