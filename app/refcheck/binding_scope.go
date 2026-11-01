// Scope rules for a policy binding: it is owned by the system, a team or a project, it binds only subjects inside that scope, and it uses a policy of that scope, an ancestor of it, the system, or a host.

package refcheck

import (
	"context"
	"net/http"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// BindablePolicy checks the policy a binding owned by scope uses. A host's tier preset is shared like a system policy (a team binds the provider's tier to its own account); a personal policy belongs to no scope a binding can have.
func (c Checker) BindablePolicy(ctx context.Context, policyID string, scope meta.Owner) error {
	if policyID == "" || c.Rows.Policy == nil {
		return nil
	}
	p := c.Rows.Policy(ctx, policyID)
	if p == nil {
		return badRequest("policy %q does not exist", policyID)
	}
	if !Visible(ctx, c.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
		return notVisible("policy", policyID)
	}
	if p.Meta.Owner.Kind == meta.OwnerHost || c.containsScope(ctx, p.Meta.Owner, scope) {
		return nil
	}
	return badRequest("policy %q is owned by %s; a binding owned by %s may use a policy of its own scope, an ancestor of it, the system, or a host", p.Meta.Name, ownerText(p.Meta.Owner), ownerText(scope))
}

// subjectsInScope refuses a subject outside the binding owner's scope. A project binds itself, its service accounts, the accounts and credentials it owns, and its members; a team the same for itself and each of its projects; the system anything, and only the system binds a host. Group subjects are not rungs and are not checked.
func (c Checker) subjectsInScope(ctx context.Context, pb *policybinding.PolicyBinding) error {
	owner := pb.Meta.Owner
	for i := range pb.Spec.Subjects {
		sub := &pb.Spec.Subjects[i]
		var in meta.Owner // the scope the subject lives in
		switch sub.Kind {
		case policybinding.SubjectHost:
			if owner.Kind != meta.OwnerSystem {
				return badRequest("subjects[%d]: a host is bound only by a system-owned binding; this one is owned by %s", i, ownerText(owner))
			}
			continue
		case rolebinding.SubjectUser:
			if err := c.userInScope(ctx, i, sub.ID, owner); err != nil {
				return err
			}
			continue
		case rolebinding.SubjectServiceAccount:
			if c.Rows.ServiceAccount == nil {
				continue
			}
			sa := c.Rows.ServiceAccount(ctx, sub.ID)
			if sa == nil {
				continue
			}
			in = meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}
		case policybinding.SubjectTeam:
			in = meta.Owner{Kind: meta.OwnerTeam, ID: sub.ID}
		case policybinding.SubjectProject:
			in = meta.Owner{Kind: meta.OwnerProject, ID: sub.ID}
		case policybinding.SubjectAccount:
			if c.Rows.Account == nil {
				continue
			}
			a := c.Rows.Account(ctx, sub.ID)
			if a == nil {
				continue
			}
			in = a.Meta.Owner
		case policybinding.SubjectCredential:
			if c.Rows.HostKey == nil {
				continue
			}
			k := c.Rows.HostKey(ctx, sub.ID)
			if k == nil {
				continue
			}
			in = k.Meta.Owner
		default:
			continue
		}
		if !c.containsScope(ctx, owner, in) {
			return badRequest("subjects[%d]: %s is outside the scope of this binding's owner, %s", i, sub.Key(), ownerText(owner))
		}
	}
	return nil
}

// userInScope refuses a user who is not a member of a team or project owning the binding. Without a membership lookup the check is skipped.
func (c Checker) userInScope(ctx context.Context, i int, userID string, owner meta.Owner) error {
	if owner.Kind == meta.OwnerSystem || c.Rows.UserActsIn == nil {
		return nil
	}
	ok, err := c.Rows.UserActsIn(ctx, userID, owner)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	if !ok {
		return badRequest("subjects[%d]: user %q is not a member of %s, which owns this binding", i, userID, ownerText(owner))
	}
	return nil
}

// containsScope reports whether scope outer holds scope inner: the same scope, the system above everything, or a team above its projects. Without a project lookup a team is taken to hold every project.
func (c Checker) containsScope(ctx context.Context, outer, inner meta.Owner) bool {
	switch {
	case outer.Kind == meta.OwnerSystem, outer == inner:
		return true
	case outer.Kind == meta.OwnerTeam && inner.Kind == meta.OwnerProject:
		return c.projectInTeam(ctx, inner.ID, outer.ID)
	}
	return false
}

func ownerText(o meta.Owner) string {
	if o.ID == "" {
		return string(o.Kind)
	}
	return string(o.Kind) + " " + o.ID
}
