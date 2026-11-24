package plan_test

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
)

// snapshotWorld is m1, whose snapshots m1 and m1-remote are served one at host-a (m1-remote) and one at host-b (m1); m2, with snapshots m2 and m2-remote, bound at both hosts by bindings listing no snapshots; m3, bound at host-a by a binding listing only a name m3 does not have; and a pool per host.
type snapshotWorld struct {
	w      *plantest.World
	pa, pb *pool.Pool
	proj   *project.Project
}

func newSnapshotWorld() *snapshotWorld {
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha, hb := w.Host("host-a", "http://a.invalid"), w.Host("host-b", "http://b.invalid")
	m1 := w.Model(prov, "m1", "m1-remote")
	w.Bind(m1, ha, adapters.OpenAI, "m1-remote")
	w.Bind(m1, hb, adapters.OpenAI, "m1")
	m2 := w.Model(prov, "m2", "m2-remote")
	w.Bind(m2, ha, adapters.OpenAI)
	w.Bind(m2, hb, adapters.OpenAI)
	w.Bind(w.Model(prov, "m3"), ha, adapters.OpenAI, "m3-retired")
	return &snapshotWorld{
		w:    w,
		pa:   w.Pool(ha, w.Credential(ha, nil, plantest.System, "sk-a")),
		pb:   w.Pool(hb, w.Credential(hb, nil, plantest.System, "sk-b")),
		proj: w.Project(w.Team()),
	}
}

// snapshotLane is a service account whose bound policy lists models and links pools.
type snapshotLane struct {
	key    *key.Key
	acct   *account.Account
	policy *policy.Policy
}

func (s *snapshotWorld) lane(models []string, pools ...*pool.Pool) snapshotLane {
	sa := s.w.ServiceAccount(s.proj)
	spec := policy.Spec{Models: models}
	for _, p := range pools {
		spec.PoolIDs = append(spec.PoolIDs, p.Meta.ID)
	}
	l := snapshotLane{key: s.w.ServiceAccountKey(sa, "sk-"+sa.Meta.ID), acct: s.w.ServiceAccountAccount(sa), policy: s.w.Policy("lane", spec)}
	s.w.BindPolicy(l.policy, l.acct)
	return l
}

// answer is where a name goes: the hosts in order, or the refusal code.
type answer struct {
	Hosts   []string
	Refused string
}

func answerOf(hosts []*host.Host, refused *plan.Refusal) answer {
	a := answer{}
	for _, h := range hosts {
		a.Hosts = append(a.Hosts, h.Meta.Name)
	}
	if refused != nil {
		a.Refused = refused.Code
	}
	return a
}

// previewAnswers is the preview's answer per snapshot name, and per model.
func previewAnswers(r *plan.Reach) (bySnapshot, byModel map[string]answer) {
	bySnapshot, byModel = map[string]answer{}, map[string]answer{}
	for _, list := range [2][]plan.ModelReach{r.Models, r.Excluded} {
		for _, mr := range list {
			byModel[mr.Model.Meta.Name] = answerOf(mr.Hosts, mr.Refused)
			for _, s := range mr.Snapshots {
				bySnapshot[s.Name] = answerOf(s.Hosts, s.Refused)
			}
		}
	}
	return bySnapshot, byModel
}

// routeAnswers is what Routes.Get answers for each name, each host once in hop order.
func routeAnswers(cp *plan.CallerPlan, names ...string) map[string]answer {
	out := map[string]answer{}
	for _, name := range names {
		r := cp.Routes.Get(name)
		var hosts []*host.Host
		for i := range r.Hops {
			if !slices.Contains(hosts, r.Hops[i].Host) {
				hosts = append(hosts, r.Hops[i].Host)
			}
		}
		out[name] = answerOf(hosts, r.Refused)
	}
	return out
}

// The preview answers for each snapshot name what a request naming it gets, so a host serving only one snapshot of a model is listed under that name alone; a model none of whose names reaches anything is excluded. A binding listing no snapshots serves every name.
func TestReach_SnapshotsAgreeWithRoutes(t *testing.T) {
	s := newSnapshotWorld()
	both, aOnly := s.lane(nil, s.pa, s.pb), s.lane(nil, s.pa)
	plans := s.w.Load(t)
	names := []string{"m1", "m1-remote", "m2", "m2-remote", "m3"}

	for _, tc := range []struct {
		name              string
		lane              snapshotLane
		snapshots, models map[string]answer
	}{
		{
			name: "pools at both hosts", lane: both,
			snapshots: map[string]answer{
				"m1": {Hosts: []string{"host-b"}}, "m1-remote": {Hosts: []string{"host-a"}},
				"m2": {Hosts: []string{"host-a", "host-b"}}, "m2-remote": {Hosts: []string{"host-a", "host-b"}},
				"m3": {Refused: plan.CodeNoBinding},
			},
			models: map[string]answer{
				"m1": {Hosts: []string{"host-a", "host-b"}}, "m2": {Hosts: []string{"host-a", "host-b"}},
				"m3": {Hosts: []string{"host-a"}, Refused: plan.CodeNoBinding},
			},
		},
		{
			name: "a pool at host-a alone", lane: aOnly,
			snapshots: map[string]answer{
				"m1": {Refused: plan.CodeNoCredentials}, "m1-remote": {Hosts: []string{"host-a"}},
				"m2": {Hosts: []string{"host-a"}}, "m2-remote": {Hosts: []string{"host-a"}},
				"m3": {Refused: plan.CodeNoBinding},
			},
			models: map[string]answer{
				"m1": {Hosts: []string{"host-a"}}, "m2": {Hosts: []string{"host-a"}},
				"m3": {Hosts: []string{"host-a"}, Refused: plan.CodeNoBinding},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshots, models := previewAnswers(plan.ReachOf(plans.Engine.View(), tc.lane.acct.Meta.ID, nil))
			if diff := cmp.Diff(tc.snapshots, snapshots); diff != "" {
				t.Errorf("preview per snapshot (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.snapshots, routeAnswers(keyCaller(t, plans, tc.lane.key), names...)); diff != "" {
				t.Errorf("Routes.Get per snapshot (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.models, models); diff != "" {
				t.Errorf("preview per model (-want +got):\n%s", diff)
			}
		})
	}

	pinned := map[string]answer{
		"m1@host-a": {Refused: plan.CodeModelNotFound}, "m1-remote@host-a": {Hosts: []string{"host-a"}},
		"m1@host-b": {Hosts: []string{"host-b"}}, "m1-remote@host-b": {Refused: plan.CodeModelNotFound},
		"m2@host-a": {Hosts: []string{"host-a"}}, "m2-remote@host-b": {Hosts: []string{"host-b"}},
	}
	if diff := cmp.Diff(pinned, routeAnswers(keyCaller(t, plans, both.key), "m1@host-a", "m1-remote@host-a", "m1@host-b", "m1-remote@host-b", "m2@host-a", "m2-remote@host-b")); diff != "" {
		t.Errorf("pinned names (-want +got):\n%s", diff)
	}
}

type issueAt struct {
	Code, Field string
	Severity    engine.Severity
}

// A policy model entry grants every snapshot of the model it names, as the request path's allow-list does, so the entry is reachable when a linked pool's host serves any of them and unreachable when the binding there serves none. An entry naming a snapshot rather than its model grants nothing.
func TestPolicyCheck_ModelRefFollowsServedSnapshots(t *testing.T) {
	s := newSnapshotWorld()
	m1 := s.lane([]string{"acme/m1"}, s.pa)
	m2 := s.lane([]string{"acme/m2"}, s.pa)
	m3 := s.lane([]string{"acme/m3"}, s.pa)
	bySnapshot := s.lane([]string{"acme/m1-remote"}, s.pa)
	plans := s.w.Load(t)
	unreachable := []issueAt{
		{Code: plan.IssueModelRefUnreachable, Field: "spec.models[0]", Severity: engine.SeverityError},
		{Code: plan.IssuePoolOutsideModels, Field: "spec.poolIds[0]", Severity: engine.SeverityWarning},
	}
	for _, tc := range []struct {
		name  string
		lane  snapshotLane
		ref   string
		route answer
		want  []issueAt
	}{
		{"host-a serves another snapshot of the model", m1, "m1-remote", answer{Hosts: []string{"host-a"}}, nil},
		{"a binding listing no snapshots", m2, "m2", answer{Hosts: []string{"host-a"}}, nil},
		{"host-a serves no snapshot of the model", m3, "m3", answer{Refused: plan.CodeNoBinding}, unreachable},
		{"the entry names a snapshot", bySnapshot, "m1-remote", answer{Refused: plan.CodeModelNotAllowed}, unreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []issueAt
			for _, is := range plans.Engine.View().Issues(engine.Ref{Kind: plan.KindPolicy, ID: tc.lane.policy.Meta.ID}) {
				got = append(got, issueAt{Code: is.Code, Field: is.Field, Severity: is.Severity})
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("issues (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(map[string]answer{tc.ref: tc.route}, routeAnswers(keyCaller(t, plans, tc.lane.key), tc.ref)); diff != "" {
				t.Errorf("request (-want +got):\n%s", diff)
			}
		})
	}
}

// issuesOf lists the code, field and severity of every issue on a row.
func issuesOf(v *engine.View, ref engine.Ref) []issueAt {
	var out []issueAt
	for _, is := range v.Issues(ref) {
		out = append(out, issueAt{Code: is.Code, Field: is.Field, Severity: is.Severity})
	}
	return out
}

// A model whose only binding lists none of its snapshots is unservable: every request for it is refused, so a policy granting it is told.
func TestModelUnservable_BindingServingNoSnapshot(t *testing.T) {
	s := newSnapshotWorld()
	grant := s.w.Policy("grants-m3", policy.Spec{Models: []string{"acme/m3"}})
	plans := s.w.Load(t)
	var m3 string
	for _, m := range s.w.Models {
		if m.Meta.Name == "m3" {
			m3 = m.Meta.ID
		}
	}
	got := plans.Engine.View().Issues(engine.Ref{Kind: plan.KindModel, ID: m3})
	if len(got) != 1 || got[0].Code != plan.IssueModelUnservable || got[0].Target != (engine.Ref{Kind: plan.KindPolicy, ID: grant.Meta.ID}) || got[0].Severity != engine.SeverityError {
		t.Errorf("m3 issues = %+v, want one %s error naming %s", got, plan.IssueModelUnservable, grant.Meta.Name)
	}
}

// A rung above that allows a model only at a host whose binding serves none of its snapshots blocks the entry: every request is refused by that rung, though the binding there is enabled.
func TestModelRefBlocked_RungAllowsOnlyAHostServingNoSnapshot(t *testing.T) {
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha, hb := w.Host("host-a", "http://a.invalid"), w.Host("host-b", "http://b.invalid")
	m1 := w.Model(prov, "m1")
	w.Bind(m1, ha, adapters.OpenAI, "m1-retired")
	w.Bind(m1, hb, adapters.OpenAI)
	pa := w.Pool(ha, w.Credential(ha, nil, plantest.System, "sk-a"))
	pb := w.Pool(hb, w.Credential(hb, nil, plantest.System, "sk-b"))
	tm := w.Team()
	proj := w.Project(tm)
	sa := w.ServiceAccount(proj)
	k := w.ServiceAccountKey(sa, "sk-"+sa.Meta.ID)
	w.BindPolicy(w.Policy("team", policy.Spec{Models: []string{"acme/m1@host-a"}}), w.TeamAccount(tm))
	projPol := w.Policy("project", policy.Spec{Models: []string{"acme/m1"}, PoolIDs: []string{pa.Meta.ID, pb.Meta.ID}})
	w.BindPolicy(projPol, w.ProjectAccount(proj))
	plans := w.Load(t)

	if r := keyCaller(t, plans, k).Routes.Get("m1"); r.Refused == nil || r.Refused.Code != plan.CodeModelNotAllowed {
		t.Fatalf("m1 = %+v hops %v, want refused %s", r.Refused, routeCredentials(r), plan.CodeModelNotAllowed)
	}
	want := []issueAt{
		{Code: plan.IssueModelRefBlocked, Field: "spec.models[0]", Severity: engine.SeverityWarning},
		{Code: plan.IssuePoolOutsideModels, Field: "spec.poolIds[0]", Severity: engine.SeverityWarning},
	}
	if diff := cmp.Diff(want, issuesOf(plans.Engine.View(), engine.Ref{Kind: plan.KindPolicy, ID: projPol.Meta.ID})); diff != "" {
		t.Errorf("project policy issues (-want +got):\n%s", diff)
	}
}

// A model with no snapshot has no name a request can send, so the preview refuses it as a request does, rather than listing it reached with no host.
func TestReach_ModelWithoutSnapshotsIsRefused(t *testing.T) {
	s := newSnapshotWorld()
	empty := s.w.Model(s.w.Providers[0], "m0")
	empty.Spec.Snapshots = nil
	s.w.Bind(empty, s.w.Hosts[0], adapters.OpenAI)
	l := s.lane(nil, s.pa)
	plans := s.w.Load(t)
	_, models := previewAnswers(plan.ReachOf(plans.Engine.View(), l.acct.Meta.ID, nil))
	want := answer{Hosts: []string{"host-a"}, Refused: plan.CodeModelNotFound}
	if diff := cmp.Diff(want, models["m0"]); diff != "" {
		t.Errorf("preview of m0 (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]answer{"m0": {Refused: plan.CodeModelNotFound}}, routeAnswers(keyCaller(t, plans, l.key), "m0")); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}
