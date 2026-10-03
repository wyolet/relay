// Package refcheck holds the cross-row write rules a row's own Validate
// cannot see: a referenced row must exist, be visible to the caller, and
// belong to a scope the referrer may draw on; owners that mirror a spec
// field are re-derived from it. The control API's CRUD guards and the apply
// loader both call these, one function per kind, so the two write paths
// cannot drift.
//
// Rows are read through Lookup, which each caller backs with its own source
// (CRUD: the stores; apply: stored rows plus the bundle). Out of scope:
// catalog-graph checks (policy model refs) and single-row validation.
package refcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// Error is a refused write with the HTTP status it answers.
type Error struct {
	Status int
	Msg    string
	Err    error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

func badRequest(format string, args ...any) error {
	return &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// notVisible is the answer for a referenced row the caller may not see:
// confirming it exists is itself the leak.
func notVisible(kind, id string) error {
	return &Error{Status: http.StatusNotFound, Msg: fmt.Sprintf("%s %q not found", kind, id)}
}

func forbidden(err error) error {
	return &Error{Status: http.StatusForbidden, Msg: err.Error(), Err: err}
}

// Lookup reads referenced rows by id; nil means absent. A nil func skips
// every check that needs that kind.
type Lookup struct {
	Policy         func(ctx context.Context, id string) *policy.Policy
	RateLimit      func(ctx context.Context, id string) *ratelimit.RateLimit
	HostKey        func(ctx context.Context, id string) *hostkey.HostKey
	Project        func(ctx context.Context, id string) *project.Project
	Team           func(ctx context.Context, id string) *team.Team
	Role           func(ctx context.Context, id string) *role.Role
	ServiceAccount func(ctx context.Context, id string) *serviceaccount.ServiceAccount
	// MissingUsers returns the ids that name no user.
	MissingUsers func(ctx context.Context, ids []string) ([]string, error)
}

// Checker applies the rules for the caller in ctx. A nil Authz is a loader
// running as the deployment itself: visibility and grant checks pass.
type Checker struct {
	Authz authz.Authorizer
	Rows  Lookup
}

// Visible reports whether the caller may see the row; true when a doesn't
// scope reads (the single-user default).
func Visible(ctx context.Context, a authz.Authorizer, kind, id string, owner meta.Owner) bool {
	s, ok := a.(authz.Scoper)
	if !ok {
		return true
	}
	return s.Visible(ctx, kind, id, owner)
}

// RequireAdmin refuses a non-admin caller under a scoping authorizer. Without
// one (single-user mode) every authenticated caller is already the operator.
func RequireAdmin(ctx context.Context, a authz.Authorizer, what string) error {
	if _, scoped := a.(authz.Scoper); !scoped || authz.IsAdmin(ctx) {
		return nil
	}
	return forbidden(fmt.Errorf("%w: %s requires an admin", authz.ErrForbidden, what))
}

// StampOwnerID fills Owner.ID from the acting user on a user-owned row.
// Admin-token callers carry no UserID, so their rows keep an empty owner id
// and behave as operator rows. A supplied owner.id must be the caller's own;
// only the break-glass token may name someone else.
func StampOwnerID(ctx context.Context, o *meta.Owner) error {
	if o.Kind != meta.OwnerUser {
		return nil
	}
	a := actor.From(ctx)
	if a == nil {
		return nil
	}
	switch {
	case o.ID == "":
		o.ID = a.UserID
	case o.ID == a.UserID || a.AdminToken:
	default:
		return errors.New("owner.id must be empty or match the calling user")
	}
	return nil
}

// SharedOwner requires a project-owned referrer's target to live in the same
// project or be shared. Visible is not enough: a read grant on another
// tenant's row must not become inference through its credentials and limits.
// Referrers owned by the catalog tiers are unconstrained.
func SharedOwner(kind, name string, owner, refOwner meta.Owner) error {
	if refOwner.Kind != meta.OwnerProject {
		return nil
	}
	switch owner.Kind {
	case meta.OwnerSystem:
		return nil
	case meta.OwnerUser:
		// A user owner with no id is an operator row every scope may use.
		if owner.ID == "" {
			return nil
		}
	case meta.OwnerProject:
		if owner.ID == refOwner.ID {
			return nil
		}
	}
	return badRequest("%s %q belongs to another scope (%s) and cannot be referenced from this project", kind, name, owner.Kind)
}

// personalToProject refuses a personal row drawing on a project's row unless
// the caller may create keys in that project — a member is inside the
// project's attribution and limits, anyone else is not.
func (c Checker) personalToProject(ctx context.Context, owner, refOwner meta.Owner) error {
	if refOwner.Kind != meta.OwnerUser || owner.Kind != meta.OwnerProject {
		return nil
	}
	if c.Authz == nil || c.Authz.Authorize(ctx, "keys.create", authz.Resource{Kind: "key", Owner: &owner}) == nil {
		return nil
	}
	return badRequest("personal rows cannot reference project resources")
}

// PolicyRef checks a policy named by refOwner's row.
func (c Checker) PolicyRef(ctx context.Context, policyID string, refOwner meta.Owner) error {
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
	// A host-owned policy is a tier menu with no host keys behind it.
	if p.Meta.Owner.Kind == meta.OwnerHost {
		return badRequest("policy %q is a host tier policy and cannot be bound", p.Meta.Name)
	}
	if err := c.personalToProject(ctx, p.Meta.Owner, refOwner); err != nil {
		return err
	}
	return SharedOwner("policy", p.Meta.Name, p.Meta.Owner, refOwner)
}

// RateLimitRef applies the policy-ref rule to a rate limit a policy names.
func (c Checker) RateLimitRef(ctx context.Context, rateLimitID string, refOwner meta.Owner) error {
	if rateLimitID == "" || c.Rows.RateLimit == nil {
		return nil
	}
	rl := c.Rows.RateLimit(ctx, rateLimitID)
	if rl == nil {
		return badRequest("rate-limit %q does not exist", rateLimitID)
	}
	if !Visible(ctx, c.Authz, "rate-limit", rl.Meta.ID, rl.Meta.Owner) {
		return notVisible("rate-limit", rateLimitID)
	}
	if refOwner.ID != "" {
		if err := c.personalToProject(ctx, rl.Meta.Owner, refOwner); err != nil {
			return err
		}
	}
	return SharedOwner("rate-limit", rl.Meta.Name, rl.Meta.Owner, refOwner)
}

// HostKeyRefs checks the host keys a policy spends. Missing ids pass: the
// inference path skips a key it cannot find.
func (c Checker) HostKeyRefs(ctx context.Context, keyIDs []string, refOwner meta.Owner) error {
	if c.Rows.HostKey == nil {
		return nil
	}
	_, scoped := c.Authz.(authz.Scoper)
	for _, id := range keyIDs {
		k := c.Rows.HostKey(ctx, id)
		if k == nil {
			continue
		}
		if !Visible(ctx, c.Authz, "host-key", k.Meta.ID, k.Meta.Owner) {
			return notVisible("host-key", id)
		}
		if scoped && refOwner.Kind == meta.OwnerUser && k.Meta.Owner.Kind == meta.OwnerProject {
			return badRequest("personal rows cannot reference project resources")
		}
	}
	return nil
}

func (c Checker) projectRef(ctx context.Context, projectID string) error {
	if c.Rows.Project == nil {
		return nil
	}
	p := c.Rows.Project(ctx, projectID)
	if p == nil {
		return badRequest("project %q does not exist", projectID)
	}
	if !Visible(ctx, c.Authz, "project", p.Meta.ID, p.Meta.Owner) {
		return notVisible("project", projectID)
	}
	return nil
}

func (c Checker) teamRef(ctx context.Context, teamID string) error {
	if c.Rows.Team == nil {
		return nil
	}
	t := c.Rows.Team(ctx, teamID)
	if t == nil {
		return badRequest("team %q does not exist", teamID)
	}
	if !Visible(ctx, c.Authz, "team", t.Meta.ID, t.Meta.Owner) {
		return notVisible("team", teamID)
	}
	return nil
}

func (c Checker) roleRef(ctx context.Context, roleID string) (*role.Role, error) {
	if c.Rows.Role == nil {
		return nil, nil
	}
	r := c.Rows.Role(ctx, roleID)
	if r == nil {
		return nil, badRequest("role %q does not exist", roleID)
	}
	if !Visible(ctx, c.Authz, "role", r.Meta.ID, r.Meta.Owner) {
		return nil, notVisible("role", roleID)
	}
	return r, nil
}

// subjectsExist refuses an id-bearing subject naming no row. Group subjects
// carry a name an IdP may supply and are never checked.
func (c Checker) subjectsExist(ctx context.Context, subjects []rolebinding.Subject) error {
	for i := range subjects {
		sub := &subjects[i]
		switch sub.Kind {
		case rolebinding.SubjectUser:
			if c.Rows.MissingUsers == nil {
				continue
			}
			missing, err := c.Rows.MissingUsers(ctx, []string{sub.ID})
			if err != nil {
				return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
			}
			if len(missing) > 0 {
				return badRequest("user %q does not exist", sub.ID)
			}
		case rolebinding.SubjectServiceAccount:
			if c.Rows.ServiceAccount == nil {
				continue
			}
			if c.Rows.ServiceAccount(ctx, sub.ID) == nil {
				return badRequest("service account %q does not exist", sub.ID)
			}
		}
	}
	return nil
}

// Key derives the key's owner from its principal and checks the policy it
// carries.
func (c Checker) Key(ctx context.Context, k *key.Key) error {
	if err := c.KeyOwner(ctx, k); err != nil {
		return err
	}
	return c.PolicyRef(ctx, k.Spec.PolicyID, k.Meta.Owner)
}

// KeyOwner derives the key's owner from its principal — a service account's
// project, or the user it names. An account the caller may not see is
// reported as absent.
func (c Checker) KeyOwner(ctx context.Context, k *key.Key) error {
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		if c.Rows.ServiceAccount == nil {
			return badRequest("service accounts are not available on this relay")
		}
		sa := c.Rows.ServiceAccount(ctx, k.Spec.Principal.ID)
		if sa == nil || !Visible(ctx, c.Authz, "service-account", sa.Meta.ID, sa.Meta.Owner) {
			return notVisible("service-account", k.Spec.Principal.ID)
		}
		k.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}
	case key.PrincipalUser:
		k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: k.Spec.Principal.ID}
		if err := StampOwnerID(ctx, &k.Meta.Owner); err != nil {
			return badRequest("%s", err.Error())
		}
		k.Spec.Principal.ID = k.Meta.Owner.ID
		if c.Rows.MissingUsers != nil && k.Meta.Owner.ID != "" {
			missing, err := c.Rows.MissingUsers(ctx, []string{k.Meta.Owner.ID})
			if err != nil || len(missing) > 0 {
				return notVisible("user", k.Meta.Owner.ID)
			}
		}
	default:
		return badRequest("spec.principal.kind must be serviceaccount or user")
	}
	return nil
}

// ServiceAccount re-derives the owner from spec.projectId and checks the
// project and policy it names.
func (c Checker) ServiceAccount(ctx context.Context, sa *serviceaccount.ServiceAccount) error {
	sa.StampOwner()
	if err := c.projectRef(ctx, sa.Spec.ProjectID); err != nil {
		return err
	}
	return c.PolicyRef(ctx, sa.Spec.PolicyID, sa.Meta.Owner)
}

// Project re-derives the owner from spec.teamId and checks the team.
func (c Checker) Project(ctx context.Context, p *project.Project) error {
	p.StampOwner()
	return c.teamRef(ctx, p.Spec.TeamID)
}

// Group refuses a member id that is not a user: a typo'd id would silently
// grant nothing.
func (c Checker) Group(ctx context.Context, g *group.Group) error {
	if c.Rows.MissingUsers == nil {
		return nil
	}
	missing, err := c.Rows.MissingUsers(ctx, g.Spec.MemberIDs)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	if len(missing) > 0 {
		return badRequest("user %q does not exist", missing[0])
	}
	return nil
}

// Policy checks the host keys and rate limits a policy draws on.
func (c Checker) Policy(ctx context.Context, p *policy.Policy) error {
	if err := c.HostKeyRefs(ctx, p.Spec.HostKeyIDs, p.Meta.Owner); err != nil {
		return err
	}
	if err := c.RateLimitRef(ctx, p.Spec.RateLimitID, p.Meta.Owner); err != nil {
		return err
	}
	for _, b := range p.Spec.RLBindings {
		if err := c.RateLimitRef(ctx, b.RateLimitID, p.Meta.Owner); err != nil {
			return err
		}
	}
	return nil
}

// HostKey requires the key's tier policy to be host-owned by the key's own
// host; a mismatched key drops out of the snapshot and answers no_keys.
func (c Checker) HostKey(ctx context.Context, k *hostkey.HostKey) error {
	if c.Rows.Policy == nil {
		return nil
	}
	pol := c.Rows.Policy(ctx, k.Spec.PolicyID)
	if pol == nil {
		return badRequest("policy %q does not exist", k.Spec.PolicyID)
	}
	if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != k.Spec.HostID {
		return badRequest("policy %q is not host-owned by host %q (owner=%s/%s)",
			pol.Meta.Name, k.Spec.HostID, pol.Meta.Owner.Kind, pol.Meta.Owner.ID)
	}
	return nil
}

// RoleBinding re-derives the owner from spec.scope and checks the role, the
// scope target, and the subjects. Binding a role hands out every permission
// in it, so the binder must already hold each one at that scope.
func (c Checker) RoleBinding(ctx context.Context, rb *rolebinding.RoleBinding) error {
	rb.StampOwner()
	r, err := c.roleRef(ctx, rb.Spec.RoleID)
	if err != nil {
		return err
	}
	if err := authz.CheckGrant(ctx, c.Authz, r, rb.Spec.Scope); err != nil {
		return forbidden(err)
	}
	switch rb.Spec.Scope.Kind {
	case meta.OwnerTeam:
		if err := c.teamRef(ctx, rb.Spec.Scope.ID); err != nil {
			return err
		}
	case meta.OwnerProject:
		if err := c.projectRef(ctx, rb.Spec.Scope.ID); err != nil {
			return err
		}
	}
	return c.subjectsExist(ctx, rb.Spec.Subjects)
}

// PolicyBinding re-derives the owner from spec.projectId, fills in the
// default priority, and checks the project, policy, and subjects.
func (c Checker) PolicyBinding(ctx context.Context, pb *policybinding.PolicyBinding) error {
	pb.StampOwner()
	// Absent means the default; an explicit 0 is a real priority.
	if pb.Spec.Priority == nil {
		def := policybinding.DefaultPriority
		pb.Spec.Priority = &def
	}
	if err := c.projectRef(ctx, pb.Spec.ProjectID); err != nil {
		return err
	}
	if err := c.PolicyRef(ctx, pb.Spec.PolicyID, pb.Meta.Owner); err != nil {
		return err
	}
	return c.subjectsExist(ctx, pb.Spec.Subjects)
}
