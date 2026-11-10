package plan

import (
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// personalPolicy is a policy owned by u.
func (w *world) personalPolicy(u *user.User, spec policy.Spec) *policy.Policy {
	p := w.policy("mine-"+seq(), spec)
	p.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	return p
}

// userSubject names a user in a role binding.
func userSubject(id string) rolebinding.Subject {
	return rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id}
}

// policyBindingsAdmin is a role managing policy bindings, as the built-in scope admins do.
func (w *world) policyBindingsAdmin() *role.Role {
	r := &role.Role{Meta: md("pb-admin-"+seq(), system), Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"policy-bindings"}, Verbs: []string{"*"}}}}}
	w.roles = append(w.roles, r)
	return r
}

func (w *world) grant(r *role.Role, scope meta.Owner, sub rolebinding.Subject) *rolebinding.RoleBinding {
	rb := &rolebinding.RoleBinding{Meta: md("rb-"+seq(), scope), Spec: rolebinding.Spec{RoleID: r.Meta.ID, Scope: scope, Subjects: []rolebinding.Subject{sub}}}
	w.rbs = append(w.rbs, rb)
	return rb
}

// A user-owned binding written around the save check is ignored for anyone but its owner, and when it uses another user's policy.
func TestUserOwnBindingStaysWithItsOwnersPolicy(t *testing.T) {
	b := newBasic()
	alice, bob := b.w.user(), b.w.user()
	bobKey := b.w.userKey(bob)
	alicesPolicy := b.w.personalPolicy(alice, policy.Spec{Models: []string{"acme/none"}})
	onBob := ownedBy(b.w.bindPolicy(alicesPolicy, b.w.personal(bob)), meta.Owner{Kind: meta.OwnerUser, ID: alice.ID})
	bobsOwn := ownedBy(b.w.bindPolicy(alicesPolicy, b.w.personal(bob)), meta.Owner{Kind: meta.OwnerUser, ID: bob.ID})
	h := b.w.start(t)

	if rung := h.keyPlan(bobKey).Caller.LaneRung(); rung.Policy != nil || h.keyPlan(bobKey).Caller.Refused != nil {
		t.Fatalf("bob governed by alice's policy: rung %+v, refused %+v", rung.Policy, h.keyPlan(bobKey).Caller.Refused)
	}
	if !hasIssue(h.issues(KindPolicyBinding, onBob.Meta.ID), IssueSubjectOutOfScope) {
		t.Fatalf("alice's binding of bob: issues %+v, want %s", h.issues(KindPolicyBinding, onBob.Meta.ID), IssueSubjectOutOfScope)
	}
	if !hasIssue(h.issues(KindPolicyBinding, bobsOwn.Meta.ID), IssuePolicyNotBindable) {
		t.Fatalf("bob's binding of alice's policy: issues %+v, want %s", h.issues(KindPolicyBinding, bobsOwn.Meta.ID), IssuePolicyNotBindable)
	}
}

// A project binds a member's personal policy to its service account while the member may manage the project's policy bindings, however that permission is granted; without it the binding is ignored with an issue.
func TestPersonalPolicyBoundWhereItsOwnerManages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grant func(w *world, b *basic, u *user.User)
		ok    bool
	}{
		{"no grant", func(*world, *basic, *user.User) {}, false},
		{"project admin", func(w *world, b *basic, u *user.User) {
			w.grant(w.policyBindingsAdmin(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}, userSubject(u.ID))
		}, true},
		{"team admin", func(w *world, b *basic, u *user.User) {
			w.grant(w.policyBindingsAdmin(), meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}, userSubject(u.ID))
		}, true},
		{"admin of another project", func(w *world, b *basic, u *user.User) {
			w.grant(w.policyBindingsAdmin(), meta.Owner{Kind: meta.OwnerProject, ID: w.project(b.team).Meta.ID}, userSubject(u.ID))
		}, false},
		{"through a group", func(w *world, b *basic, u *user.User) {
			g := &group.Group{Meta: md("leads-"+seq(), system), Spec: group.Spec{MemberIDs: []string{u.ID}}}
			w.groups = append(w.groups, g)
			w.grant(w.policyBindingsAdmin(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: g.Meta.Name})
		}, true},
		{"deployment admin", func(_ *world, _ *basic, u *user.User) { u.Roles = []string{user.RoleAdmin} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBasic()
			b.w.rbac = true
			u := b.w.user()
			tc.grant(b.w, b, u)
			mine := b.w.personalPolicy(u, policy.Spec{Models: []string{"acme/none"}})
			pb := ownedBy(b.w.bindPolicy(mine, b.w.saAccount(b.sa)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
			h := b.w.start(t)

			applied := h.keyPlan(b.key).Caller.LaneRung().Policy != nil
			ignored := hasIssue(h.issues(KindPolicyBinding, pb.Meta.ID), IssuePolicyNotBindable)
			if applied != tc.ok || ignored == tc.ok {
				t.Fatalf("binding applied %v, %s issue %v; want applied %v", applied, IssuePolicyNotBindable, ignored, tc.ok)
			}
		})
	}
}

// Revoking the owner's permission after the binding was saved drops the binding from the service account's plan with an issue; granting it again brings it back.
func TestPersonalPolicyBindingFollowsItsOwnersPermission(t *testing.T) {
	b := newBasic()
	b.w.rbac = true
	u := b.w.user()
	rb := b.w.grant(b.w.policyBindingsAdmin(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}, userSubject(u.ID))
	mine := b.w.personalPolicy(u, policy.Spec{Models: []string{"acme/none"}})
	pb := ownedBy(b.w.bindPolicy(mine, b.w.saAccount(b.sa)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	h := b.w.start(t)

	if r := h.route(b.key, "m1"); r.Refused == nil || r.Refused.Code != CodeModelNotAllowed {
		t.Fatalf("route under the bound personal policy = %+v, want model_not_allowed", r.Refused)
	}
	del(h, KindRoleBinding, rb.Meta.ID)
	if r := h.route(b.key, "m1"); r.Refused != nil {
		t.Fatalf("route after the owner lost the permission refused: %+v", r.Refused)
	}
	if !hasIssue(h.issues(KindPolicyBinding, pb.Meta.ID), IssuePolicyNotBindable) {
		t.Fatalf("issues = %+v, want %s", h.issues(KindPolicyBinding, pb.Meta.ID), IssuePolicyNotBindable)
	}
	apply(h, rb)
	if r := h.route(b.key, "m1"); r.Refused == nil || r.Refused.Code != CodeModelNotAllowed {
		t.Fatalf("route after the permission came back = %+v, want model_not_allowed", r.Refused)
	}
	if hasIssue(h.issues(KindPolicyBinding, pb.Meta.ID), IssuePolicyNotBindable) {
		t.Fatal("the issue stayed after the permission came back")
	}
}

// Under the single-user authorizer every user may manage every binding, so the plans apply a project's binding of a personal policy without asking.
func TestPersonalPolicyBindingWithoutRBAC(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	mine := b.w.personalPolicy(u, policy.Spec{Models: []string{"acme/none"}})
	ownedBy(b.w.bindPolicy(mine, b.w.saAccount(b.sa)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	h := b.w.start(t)

	if h.keyPlan(b.key).Caller.LaneRung().Policy == nil {
		t.Fatal("binding of a personal policy ignored without RBAC")
	}
}
