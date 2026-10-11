package authz

import (
	"context"
	"sync"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	coreauthz "github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
)

var globalScope = rbac.Scope{Kind: string(meta.OwnerSystem)}

// evaluation is one decision's view of the catalog for the rbac engine: its Source and ScopeResolver over a snapshot read at most once, so every lookup in the decision sees the same one. The buffers back the slices it hands the engine, which is done with each before the next call; pooling them keeps a decision free of conversion allocations.
type evaluation struct {
	snapFn    func() Snapshot
	snap      Snapshot
	loaded    bool
	principal rbac.Principal
	owner     coreauthz.Owner
	metaOwner meta.Owner
	rules     [6]rbac.ProductRule
	chain     []rbac.Scope
	bindings  []rbac.Binding
	roleRules []rbac.Rule
}

var evaluations = sync.Pool{New: func() any {
	ev := &evaluation{}
	ev.rules = [6]rbac.ProductRule{
		personalRowRule{},
		catalogReadRule{},
		sharedPolicyReadRule{},
		systemRoleReadRule{},
		scopedListRule{},
		parentTeamReadRule{ev},
	}
	return ev
}}

// acquireEvaluation returns an evaluation for a; release it once the decision is made.
func acquireEvaluation(snap func() Snapshot, a *actor.Actor) *evaluation {
	ev := evaluations.Get().(*evaluation)
	ev.snapFn = snap
	ev.principal = rbac.Principal{ID: a.UserID, Subjects: a.Subjects, Admin: adminActor(a)}
	return ev
}

// release drops what pins the caller and the snapshot, keeping the buffers.
func (ev *evaluation) release() {
	ev.snapFn, ev.snap, ev.loaded = nil, nil, false
	ev.principal, ev.owner, ev.metaOwner = rbac.Principal{}, coreauthz.Owner{}, meta.Owner{}
	evaluations.Put(ev)
}

func (ev *evaluation) engine() rbac.Engine {
	return rbac.Engine{Source: ev, Scopes: ev, Rules: ev.rules[:], Global: globalScope}
}

// resource converts res for the engine, keeping the owner in ev.
func (ev *evaluation) resource(res Resource) coreauthz.Resource {
	out := coreauthz.Resource{Kind: res.Kind, ID: res.ID, Name: res.Name}
	if res.Owner != nil {
		ev.owner = coreauthz.Owner{Kind: string(res.Owner.Kind), ID: res.Owner.ID}
		out.Owner = &ev.owner
	}
	return out
}

func (ev *evaluation) snapshot() Snapshot {
	if !ev.loaded {
		ev.loaded = true
		if ev.snapFn != nil {
			ev.snap = ev.snapFn()
		}
	}
	return ev.snap
}

// BindingsForSubject implements rbac.Source.
func (ev *evaluation) BindingsForSubject(_ context.Context, subject string) ([]rbac.Binding, error) {
	snap := ev.snapshot()
	if snap == nil {
		return nil, nil
	}
	out := ev.bindings[:0]
	for _, b := range snap.RoleBindingsForSubject(subject) {
		out = append(out, rbac.Binding{
			RoleID: b.Spec.RoleID,
			Scope:  rbac.Scope{Kind: string(b.Spec.Scope.Kind), ID: b.Spec.Scope.ID},
		})
	}
	ev.bindings = out
	return out, nil
}

// Role implements rbac.Source.
func (ev *evaluation) Role(_ context.Context, id string) (rbac.Role, bool, error) {
	snap := ev.snapshot()
	if snap == nil {
		return rbac.Role{}, false, nil
	}
	r, ok := snap.Role(id)
	if !ok {
		return rbac.Role{}, false, nil
	}
	rules := ev.roleRules[:0]
	for _, rule := range r.Spec.Rules {
		rules = append(rules, rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs})
	}
	ev.roleRules = rules
	return rbac.Role{ID: r.Meta.ID, Rules: rules}, true, nil
}

// ChainFor implements rbac.ScopeResolver over Snapshot.ScopeChainFor.
func (ev *evaluation) ChainFor(_ context.Context, res coreauthz.Resource) ([]rbac.Scope, error) {
	snap := ev.snapshot()
	if snap == nil {
		ev.chain = append(ev.chain[:0], globalScope)
		return ev.chain, nil
	}
	var owner *meta.Owner
	if res.Owner != nil {
		ev.metaOwner = meta.Owner{Kind: meta.OwnerKind(res.Owner.Kind), ID: res.Owner.ID}
		owner = &ev.metaOwner
	}
	out := ev.chain[:0]
	for _, o := range snap.ScopeChainFor(res.Kind, res.ID, owner) {
		out = append(out, rbac.Scope{Kind: string(o.Kind), ID: o.ID})
	}
	ev.chain = out
	return out, nil
}
