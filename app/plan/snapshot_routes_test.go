package plan

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/pkg/catalogkind/model"
)

// Without a binding that lists snapshots every snapshot name of a model shares one fill, so such a catalog costs the preview and the checks one fill per model; a model with a binding that lists snapshots is filled per name.
func TestSnapshotRoutes_OneFillWithoutANarrowedBinding(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Snapshots = append(b.m1.Spec.Snapshots, model.Snapshot{Name: "m1-x"}, model.Snapshot{Name: "m1-y"})
	m2 := b.w.model(b.prov, "m2")
	m2.Spec.Snapshots = append(m2.Spec.Snapshots, model.Snapshot{Name: "m2-x"})
	b.w.bind(m2, b.hostA).Spec.Snapshots = []string{"m2-x"}
	b.w.bind(m2, b.hostB)
	h := b.w.start(t)
	rt := h.keyPlan(b.key).Caller.Routes

	shared, _ := rt.idx.Snap.Model(b.m1.Meta.ID)
	if rs := rt.snapshotRoutes(shared); len(rs) != 3 || rs[0] != rs[1] || rs[1] != rs[2] || len(rs[0].Hops) == 0 {
		t.Errorf("m1 routes = %v, want one routed fill shared by its 3 snapshots", rs)
	}
	narrowed, _ := rt.idx.Snap.Model(m2.Meta.ID)
	rs := rt.snapshotRoutes(narrowed)
	if len(rs) != 2 || rs[0] == rs[1] {
		t.Fatalf("m2 routes = %v, want a fill per snapshot", rs)
	}
	if got, want := [][]string{hopHosts(rs[0]), hopHosts(rs[1])}, [][]string{{"host-b"}, {"host-a", "host-b"}}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("m2 hosts = %v, want %v: m2 at host-b and m2-x at both hosts", got, want)
	}
}

// hopHosts lists the hosts of a route's hops, each once, in hop order.
func hopHosts(r *Route) []string {
	var out []string
	for _, hop := range r.Hops {
		if !slices.Contains(out, hop.Host.Meta.Name) {
			out = append(out, hop.Host.Meta.Name)
		}
	}
	return out
}
