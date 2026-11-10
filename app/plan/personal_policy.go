// A personal policy is bound by its owner: to themselves, or from a team or project where the owner may manage policy bindings, the rule app/refcheck enforces on save. The plan holds to it as well, so a binding whose owner lost that permission (a role binding revoked) is ignored for its subjects rather than applied. The permission is decided by the RBAC authorizer over the roles, role bindings and groups the engine holds, so a change to them re-folds the rungs that asked.

package plan

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// bindingsPermitted keeps the bindings whose policy their owner may use and files an issue for each one it drops. Only personal policies are checked here; the other owner rules have no state that changes after save.
func (c *controllers) bindingsPermitted(tx *engine.Txn, subject string, bs []*policybinding.PolicyBinding) []*policybinding.PolicyBinding {
	kept := bs[:0:0]
	for _, b := range bs {
		p := engine.Get[policy.Policy](tx, b.Spec.PolicyID)
		if p == nil || p.Meta.Owner.Kind != meta.OwnerUser {
			kept = append(kept, b)
			continue
		}
		why := c.personalPolicyRefusal(tx, p, b.Meta.Owner)
		if why == "" {
			kept = append(kept, b)
			continue
		}
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssuePolicyNotBindable, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Target: engine.Ref{Kind: KindPolicy, ID: p.Meta.ID}, Field: "spec.policyId",
			Message: fmt.Sprintf("policy %s is owned by %s, %s; the binding is ignored for %s", p.Meta.Name, ownerKey(p.Meta.Owner), why, subject),
		})
	}
	return kept
}

// personalPolicyRefusal says why a binding owned by scope may not use the personal policy p, "" when it may.
func (c *controllers) personalPolicyRefusal(tx *engine.Txn, p *policy.Policy, scope meta.Owner) string {
	switch scope.Kind {
	case meta.OwnerUser:
		if scope.ID == p.Meta.Owner.ID {
			return ""
		}
	case meta.OwnerTeam, meta.OwnerProject:
		if !c.rbac || mayBindIn(tx, p.Meta.Owner.ID, scope) {
			return ""
		}
		return "who may not manage policy bindings in " + ownerKey(scope)
	}
	return "and only that user or a team or project they manage binds it, not " + ownerKey(scope)
}

// mayBindIn asks the RBAC authorizer, as user userID, whether they may create a policy binding owned by scope. Any refusal, including a missing user, counts as no.
func mayBindIn(tx *engine.Txn, userID string, scope meta.Owner) bool {
	u := engine.Get[user.User](tx, userID)
	if u == nil {
		return false
	}
	rows := &rbacRows{tx: tx}
	a := &actor.Actor{UserID: u.ID, Username: u.Username, Roles: u.Roles, Subjects: appcatalog.UserSubjects(u.ID, rows.groupsOf(u.ID), nil)}
	ctx := actor.WithActor(context.Background(), a)
	return authz.RBAC{Snap: func() authz.Snapshot { return rows }}.Authorize(ctx, "policy-bindings.create", authz.Resource{Kind: "policy-binding", Owner: &scope}) == nil
}

// rbacRows is authz.Snapshot over the engine's rows, each read recorded so a change to them re-runs the reconcile that asked. Disabled rows are left out, as the catalog snapshot leaves them out.
type rbacRows struct {
	tx        *engine.Txn
	bySubject map[string][]*rolebinding.RoleBinding
}

// ScopeChainFor resolves from the owner alone: the plans ask only about rows that do not define a scope.
func (r *rbacRows) ScopeChainFor(_, _ string, o *meta.Owner) []meta.Owner {
	global := meta.Owner{Kind: meta.OwnerSystem}
	if o == nil {
		return []meta.Owner{global}
	}
	switch o.Kind {
	case meta.OwnerProject:
		if p := engine.Get[project.Project](r.tx, o.ID); p != nil && p.IsEnabled() {
			return []meta.Owner{*o, {Kind: meta.OwnerTeam, ID: p.Spec.TeamID}, global}
		}
	case meta.OwnerTeam:
		if t := engine.Get[team.Team](r.tx, o.ID); t != nil && t.IsEnabled() {
			return []meta.Owner{*o, global}
		}
	}
	return []meta.Owner{global}
}

func (r *rbacRows) RoleBindingsForSubject(subject string) []*rolebinding.RoleBinding {
	if r.bySubject == nil {
		r.bySubject = map[string][]*rolebinding.RoleBinding{}
		for b := range engine.All[rolebinding.RoleBinding](r.tx) {
			if !b.IsEnabled() {
				continue
			}
			for i := range b.Spec.Subjects {
				k := b.Spec.Subjects[i].Key()
				r.bySubject[k] = append(r.bySubject[k], b)
			}
		}
	}
	return r.bySubject[subject]
}

func (r *rbacRows) ProjectsInTeam(teamID string) []*project.Project {
	var out []*project.Project
	for p := range engine.All[project.Project](r.tx) {
		if p.Spec.TeamID == teamID && p.IsEnabled() {
			out = append(out, p)
		}
	}
	return out
}

func (r *rbacRows) Role(id string) (*role.Role, bool) {
	ro := engine.Get[role.Role](r.tx, id)
	if ro == nil || !ro.IsEnabled() {
		return nil, false
	}
	return ro, true
}

// groupsOf returns the names of the enabled groups userID is a member of.
func (r *rbacRows) groupsOf(userID string) []string {
	var out []string
	for g := range engine.All[group.Group](r.tx) {
		if g.IsEnabled() && slices.Contains(g.Spec.MemberIDs, userID) {
			out = append(out, g.Meta.Name)
		}
	}
	return out
}
