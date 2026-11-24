package plan_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// coverageWorld serves models of acme at host-a, host-b, host-c (disabled) and host-d:
//   - m1 (m1, m1-remote): host-a serves m1-remote, host-b m1
//   - m3: host-a lists only a name m3 does not have
//   - m4 (m4, m4-x, m4-y), alias m4@20250514: host-a serves m4 and m4-x, host-b m4-x and m4-y, host-c m4-y, and a disabled binding at host-d m4
//   - m5 (m5, m5-x), aliases m5-latest, m1 (shadowed by m1's snapshot name, but m1 has no binding at host-d) and m1-host-b (which reads like m1's pin at host-b): host-a and host-d serve all
//
// with two pools at host-a and one at each other host, each pool holding a system credential of its own.
type coverageWorld struct {
	w                  *plantest.World
	pa1, pa2, pb, pc   *pool.Pool
	pd                 *pool.Pool
	proj               *project.Project
	hostNames, snapRef []string
	// owner is each snapshot name's model.
	owner map[string]string
}

func newCoverageWorld() *coverageWorld {
	w := &plantest.World{}
	acme := w.Provider("acme")
	ha, hb, hc, hd := w.Host("host-a", "http://a.invalid"), w.Host("host-b", "http://b.invalid"), w.Host("host-c", "http://c.invalid"), w.Host("host-d", "http://d.invalid")
	hc.Spec.Enabled = off()
	m1 := w.Model(acme, "m1", "m1-remote")
	w.Bind(m1, ha, adapters.OpenAI, "m1-remote")
	w.Bind(m1, hb, adapters.OpenAI, "m1")
	w.Bind(w.Model(acme, "m3"), ha, adapters.OpenAI, "m3-retired")
	m4 := w.Model(acme, "m4", "m4-x", "m4-y")
	m4.Spec.Aliases = []string{"m4@20250514"}
	w.Bind(m4, ha, adapters.OpenAI, "m4", "m4-x")
	w.Bind(m4, hb, adapters.OpenAI, "m4-x", "m4-y")
	w.Bind(m4, hc, adapters.OpenAI, "m4-y")
	w.Bind(m4, hd, adapters.OpenAI, "m4").Spec.Enabled = off()
	m5 := w.Model(acme, "m5", "m5-x")
	m5.Spec.Aliases = []string{"m5-latest", "m1", "m1-host-b"}
	w.Bind(m5, ha, adapters.OpenAI)
	w.Bind(m5, hd, adapters.OpenAI)
	pool := func(h int) *pool.Pool {
		return w.Pool(w.Hosts[h], w.Credential(w.Hosts[h], nil, plantest.System, "sk-"+meta.NewID()))
	}
	c := &coverageWorld{w: w, pa1: pool(0), pa2: pool(0), pb: pool(1), pc: pool(2), pd: pool(3), proj: w.Project(w.Team()), owner: map[string]string{}}
	for _, h := range w.Hosts {
		c.hostNames = append(c.hostNames, h.Meta.Name)
	}
	for _, m := range w.Models {
		for _, s := range m.Spec.Snapshots {
			c.snapRef = append(c.snapRef, s.Name)
			c.owner[s.Name] = m.Meta.Name
		}
	}
	return c
}

// routedTo names where a request goes: model@hosts, or the refusal code.
func routedTo(cp *plan.CallerPlan, ref string) string {
	r := cp.Routes.Get(ref)
	if r.Refused != nil {
		return r.Refused.Code
	}
	a := routeAnswers(cp, ref)[ref]
	return r.Model.Meta.Name + "@" + strings.Join(a.Hosts, ",")
}

func (c *coverageWorld) lane(pools ...*pool.Pool) snapshotLane {
	s := &snapshotWorld{w: c.w, proj: c.proj}
	return s.lane(nil, pools...)
}

// For every snapshot name, the preview's hosts are exactly the hosts a pinned request routes to the name's model at, in either name form, and the unpinned name gets the preview's answer; a pinned name that reaches another model does so only through that model's alias, where the name's own model has no binding. A declared alias gets its snapshot's answer. Checked for lanes linking every pool, two pools at one host, one host, and no pool (reach by ownership).
func TestReach_PinnedNamesRouteExactlyWherePreviewSays(t *testing.T) {
	c := newCoverageWorld()
	lanes := map[string]snapshotLane{
		"every pool":         c.lane(c.pa1, c.pa2, c.pb, c.pc, c.pd),
		"two pools at a":     c.lane(c.pa1, c.pa2),
		"host-b alone":       c.lane(c.pb),
		"ownership, no pool": c.lane(),
	}
	plans := c.w.Load(t)
	boundHosts := map[string][]string{}
	for _, b := range c.w.Bindings {
		if b.IsEnabled() {
			for _, m := range c.w.Models {
				for _, h := range c.w.Hosts {
					if m.Meta.ID == b.Spec.ModelID && h.Meta.ID == b.Spec.HostID {
						boundHosts[m.Meta.Name] = append(boundHosts[m.Meta.Name], h.Meta.Name)
					}
				}
			}
		}
	}
	for name, l := range lanes {
		t.Run(name, func(t *testing.T) {
			r := plan.ReachOf(plans.Engine.View(), l.acct.Meta.ID, nil)
			preview, _ := previewAnswers(r)
			cp := keyCaller(t, plans, l.key)
			for _, n := range c.snapRef {
				want := preview[n]
				if want.Hosts == nil && want.Refused == "" {
					want = routeAnswers(cp, n)[n]
					if len(want.Hosts) > 0 {
						t.Errorf("%s routes to %v but the preview leaves its model out", n, want.Hosts)
					}
				}
				for _, form := range []string{n, "acme/" + n} {
					if got := routeAnswers(cp, form)[form]; !cmp.Equal(want, got) {
						t.Errorf("%s: request %+v, preview %+v", form, got, want)
					}
				}
				for _, h := range c.hostNames {
					for _, form := range []string{n + "@" + h, "acme/" + n + "@" + h} {
						if route := cp.Routes.Get(form); route.Model != nil && route.Model.Meta.Name != c.owner[n] {
							if !strings.HasPrefix(route.ResolvedVia, "alias:") || slices.Contains(boundHosts[c.owner[n]], h) {
								t.Errorf("%s routes to model %s via %q, though %s is bound at %s: %v", form, route.Model.Meta.Name, route.ResolvedVia, c.owner[n], h, boundHosts[c.owner[n]])
							}
							continue
						}
						got := routeAnswers(cp, form)[form]
						if routed, previewed := len(got.Hosts) > 0, slices.Contains(want.Hosts, h); routed != previewed || routed && !slices.Equal(got.Hosts, []string{h}) {
							t.Errorf("%s routes to %v (refused %q); preview hosts for %s are %v", form, got.Hosts, got.Refused, n, want.Hosts)
						}
					}
				}
			}
			for _, mr := range r.Models {
				for _, s := range mr.Snapshots {
					for _, a := range s.Aliases {
						if got := routeAnswers(cp, a)[a]; !cmp.Equal(preview[s.Name], got) {
							t.Errorf("alias %s: request %+v, preview of %s %+v", a, got, s.Name, preview[s.Name])
						}
					}
				}
			}
		})
	}

	every := plan.ReachOf(plans.Engine.View(), lanes["every pool"].acct.Meta.ID, nil)
	got, _ := previewAnswers(every)
	want := map[string]answer{
		"m1": {Hosts: []string{"host-b"}}, "m1-remote": {Hosts: []string{"host-a"}},
		"m3": {Refused: plan.CodeNoBinding},
		"m4": {Hosts: []string{"host-a"}}, "m4-x": {Hosts: []string{"host-a", "host-b"}}, "m4-y": {Hosts: []string{"host-b"}},
		"m5": {Hosts: []string{"host-a", "host-d"}}, "m5-x": {Hosts: []string{"host-a", "host-d"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("every pool, per snapshot (-want +got):\n%s", diff)
	}
	aliases := map[string][]string{}
	for _, mr := range every.Models {
		for _, s := range mr.Snapshots {
			if s.Aliases != nil {
				aliases[s.Name] = s.Aliases
			}
		}
	}
	if diff := cmp.Diff(map[string][]string{"m4": {"m4@20250514"}, "m5": {"m5-latest"}}, aliases); diff != "" {
		t.Errorf("aliases by snapshot (-want +got):\n%s", diff)
	}

	cp := keyCaller(t, plans, lanes["every pool"].key)
	routes := map[string]string{}
	for _, ref := range []string{"m1@host-a", "m1@host-d", "acme/m1@host-d", "m4@20250514", "m1-host-b", "m1-host-b@host-a", "acme/m1-host-b@host-a"} {
		routes[ref] = routedTo(cp, ref)
	}
	if diff := cmp.Diff(map[string]string{
		"m1@host-a":             plan.CodeModelNotFound,
		"m1@host-d":             "m5@host-d",
		"acme/m1@host-d":        "m5@host-d",
		"m4@20250514":           "m4@host-a",
		"m1-host-b":             "m1@host-b",
		"m1-host-b@host-a":      "m5@host-a",
		"acme/m1-host-b@host-a": "m5@host-a",
	}, routes); diff != "" {
		t.Errorf("alias pins (-want +got):\n%s", diff)
	}

	r := plan.ReachOf(plans.Engine.View(), lanes["two pools at a"].acct.Meta.ID, nil)
	for _, mr := range r.Models {
		if mr.Model.Meta.Name != "m1" {
			continue
		}
		var pools []string
		for _, v := range mr.Snapshots[1].Via {
			pools = append(pools, v.Pool.Meta.Name)
		}
		if diff := cmp.Diff([]string{c.pa1.Meta.Name, c.pa2.Meta.Name}, pools); diff != "" {
			t.Errorf("m1-remote via two pools at host-a (-want +got):\n%s", diff)
		}
	}
}

// A saved policy read in two ladders merges per snapshot name: a name one ladder reaches is reached, with the hosts either reaches, though the other ladder refuses it.
func TestPolicyReach_MergesSnapshotsAcrossLadders(t *testing.T) {
	c := newCoverageWorld()
	var accounts []*account.Account
	for _, p := range []*pool.Pool{c.pa1, c.pb} {
		tm := c.w.Team()
		c.w.BindPolicy(c.w.Policy("team", policy.Spec{PoolIDs: []string{p.Meta.ID}}), c.w.TeamAccount(tm))
		accounts = append(accounts, c.w.ProjectAccount(c.w.Project(tm)))
	}
	saved := c.w.Policy("saved", policy.Spec{})
	c.w.BindPolicy(saved, accounts...)
	plans := c.w.Load(t)
	v := plans.Engine.View()

	alone := [2]map[string]answer{}
	for i, a := range accounts {
		alone[i], _ = previewAnswers(plan.ReachOf(v, a.Meta.ID, nil))
	}
	if alone[0]["m1"].Refused == "" || alone[1]["m1-remote"].Refused == "" {
		t.Fatalf("ladders read alone = %+v, want each to refuse the name the other reaches", alone)
	}
	got, _ := previewAnswers(plan.PolicyReach(v, engine.Get[policy.Policy](v, saved.Meta.ID)))
	want := map[string]answer{
		"m1": {Hosts: []string{"host-b"}}, "m1-remote": {Hosts: []string{"host-a"}},
		"m3": {Refused: plan.CodeNoBinding},
		"m4": {Hosts: []string{"host-a"}}, "m4-x": {Hosts: []string{"host-a", "host-b"}}, "m4-y": {Hosts: []string{"host-b"}},
		"m5": {Hosts: []string{"host-a"}}, "m5-x": {Hosts: []string{"host-a"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("merged per snapshot (-want +got):\n%s", diff)
	}
}
