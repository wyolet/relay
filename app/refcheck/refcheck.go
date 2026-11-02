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
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
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
	// Code is the machine-readable error code, when the refusal has one.
	Code string
	Msg  string
	Err  error
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
	Host           func(ctx context.Context, id string) *host.Host
	Account        func(ctx context.Context, id string) *account.Account
	Pool           func(ctx context.Context, id string) *pool.Pool
	Budget         func(ctx context.Context, id string) *budget.Budget
	Project        func(ctx context.Context, id string) *project.Project
	Team           func(ctx context.Context, id string) *team.Team
	Role           func(ctx context.Context, id string) *role.Role
	ServiceAccount func(ctx context.Context, id string) *serviceaccount.ServiceAccount
	// RoleBindingsFor returns the stored bindings naming a role.
	RoleBindingsFor func(ctx context.Context, roleID string) ([]*rolebinding.RoleBinding, error)
	// RoleBindings and PolicyBindings return every stored binding.
	RoleBindings   func(ctx context.Context) ([]*rolebinding.RoleBinding, error)
	PolicyBindings func(ctx context.Context) ([]*policybinding.PolicyBinding, error)
	// PolicyBindingsFor returns the bindings using a policy.
	PolicyBindingsFor func(ctx context.Context, policyID string) ([]*policybinding.PolicyBinding, error)
	// MissingUsers returns the ids that name no user.
	MissingUsers func(ctx context.Context, ids []string) ([]string, error)
	// DefaultAccount returns the id of a principal's own account (a service account's account, a user's personal account), making it on first use.
	DefaultAccount func(ctx context.Context, p key.Principal) (string, error)
	// UserMayBindIn reports whether a user may manage policy bindings in a team or project scope.
	UserMayBindIn func(ctx context.Context, userID string, scope meta.Owner) (bool, error)
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

// StampOwnerID fills Owner.ID from the acting user on a user-owned row. A
// caller that is no user (the admin token, a loader with no actor) creates
// shared infrastructure, so an id-less user owner becomes system. A supplied
// owner.id must be the caller's own; only the admin token may name someone else.
func StampOwnerID(ctx context.Context, o *meta.Owner) error {
	if o.Kind != meta.OwnerUser {
		return nil
	}
	a := actor.From(ctx)
	switch {
	case o.ID == "" && (a == nil || a.AdminToken):
		*o = meta.Owner{Kind: meta.OwnerSystem}
	case o.ID == "":
		o.ID = a.UserID
	case a == nil || o.ID == a.UserID || a.AdminToken:
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
	case meta.OwnerProject:
		if owner.ID == refOwner.ID {
			return nil
		}
	}
	return badRequest("%s %q belongs to another scope (%s) and cannot be referenced from this project", kind, name, owner.Kind)
}

// catalogOwned reports a row the catalog tiers own. Its rate limits and
// policies are shared configuration any scope may draw on; host keys stay
// under SharedOwner because they are credentials.
func catalogOwned(o meta.Owner) bool {
	switch o.Kind {
	case meta.OwnerSystem, meta.OwnerProvider, meta.OwnerHost:
		return true
	}
	return false
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
	// A host-owned policy is a menu of upstream limits for host keys, with no
	// models or host keys behind it: as an inbound policy it grants nothing.
	if p.Meta.Owner.Kind == meta.OwnerHost {
		return badRequest("policy %q is a host tier policy and cannot be bound", p.Meta.Name)
	}
	if catalogOwned(p.Meta.Owner) {
		return nil
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
	if catalogOwned(rl.Meta.Owner) {
		return nil
	}
	if err := c.personalToProject(ctx, rl.Meta.Owner, refOwner); err != nil {
		return err
	}
	return c.linkable(ctx, "rate-limit", rl.Meta.Name, rl.Meta.Owner, refOwner)
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

// subjectsExist refuses a role-binding subject naming no row. Group subjects
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

// Key derives the key's owner from its principal and checks its default lane.
func (c Checker) Key(ctx context.Context, k *key.Key) error {
	if err := c.KeyOwner(ctx, k); err != nil {
		return err
	}
	return c.KeyAccount(ctx, k)
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
// project.
func (c Checker) ServiceAccount(ctx context.Context, sa *serviceaccount.ServiceAccount) error {
	sa.StampOwner()
	return c.projectRef(ctx, sa.Spec.ProjectID)
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

// Policy checks the pools, rate limits and budgets a policy draws on, and that every binding using it may use its pools.
func (c Checker) Policy(ctx context.Context, p *policy.Policy) error {
	if err := c.linksOnPolicy(ctx, p); err != nil {
		return err
	}
	if err := c.poolRefs(ctx, p.Spec.PoolIDs, p.Meta.Owner); err != nil {
		return err
	}
	for _, l := range p.Spec.RateLimits {
		if err := c.RateLimitRef(ctx, l.RateLimitID, p.Meta.Owner); err != nil {
			return err
		}
	}
	return c.budgetRefs(ctx, p.BudgetIDs(), p.Meta.Owner)
}

// Account checks the host a provider account is held at. Caller accounts are made with their team, project, user, service account or membership, never written as new rows.
func (c Checker) Account(ctx context.Context, a *account.Account) error {
	if !a.IsProvider() {
		return nil
	}
	return c.hostRef(ctx, a.Spec.HostID)
}

// KeyAccount checks the key's default lane against account.TakesKeysOf, and refuses a closed one. An empty accountId takes the principal's own account (Rows.DefaultAccount), made on first use.
func (c Checker) KeyAccount(ctx context.Context, k *key.Key) error {
	if k.Spec.AccountID == "" {
		if c.Rows.DefaultAccount == nil {
			return nil
		}
		id, err := c.Rows.DefaultAccount(ctx, k.Spec.Principal)
		if err != nil {
			return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
		}
		k.Spec.AccountID = id
		return nil
	}
	if c.Rows.Account == nil {
		return nil
	}
	a := c.Rows.Account(ctx, k.Spec.AccountID)
	if a == nil {
		return badRequest("account %q does not exist", k.Spec.AccountID)
	}
	principal := account.Holder{Kind: account.HolderKind(k.Spec.Principal.Kind), ID: k.Spec.Principal.ID}
	switch {
	case a.Spec.Closed:
		return badRequest("account %q is closed: the membership it stood for has ended", a.Meta.Name)
	case !a.TakesKeysOf(principal):
		return &Error{Status: http.StatusForbidden, Code: CodeAccountNotHeld, Msg: fmt.Sprintf(
			"account %q does not take keys of %s: a key lands in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns", a.Meta.Name, principal.Key())}
	}
	return nil
}

// CodeAccountNotHeld is the error code of a key naming an account it may not run in.
const CodeAccountNotHeld = "account_not_held"

// Pool checks the pool's host and requires every key in it to authenticate to that host.
func (c Checker) Pool(ctx context.Context, p *pool.Pool) error {
	if err := c.hostRef(ctx, p.Spec.HostID); err != nil {
		return err
	}
	return c.credentialRefs(ctx, p.Spec.CredentialIDs, p.Meta.Owner, p.Spec.HostID)
}

// HostKey checks the host the key authenticates to and, when named, its
// account, which must be held at that host.
func (c Checker) HostKey(ctx context.Context, k *hostkey.HostKey) error {
	// An env reference reads the relay's own environment; through the key's
	// health probe it would answer for any variable the caller names.
	if k.Spec.ValueFrom.Kind == hostkey.ValueKindEnv {
		if err := RequireAdmin(ctx, c.Authz, "an env-sourced host key"); err != nil {
			return err
		}
	}
	if err := c.hostRef(ctx, k.Spec.HostID); err != nil {
		return err
	}
	return c.accountRef(ctx, k)
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

// Role re-checks an edited role against every scope it is already bound at:
// the edit changes what each binding grants, so the editor must hold the new
// rules there, as creating the binding would have required.
func (c Checker) Role(ctx context.Context, r *role.Role) error {
	if c.Rows.RoleBindingsFor == nil {
		return nil
	}
	bindings, err := c.Rows.RoleBindingsFor(ctx, r.Meta.ID)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	for _, rb := range bindings {
		if err := authz.CheckGrant(ctx, c.Authz, r, rb.Spec.Scope); err != nil {
			return forbidden(err)
		}
	}
	return nil
}

// PolicyBinding checks the owning scope, the policy, and the accounts, each of which must sit inside that scope and be held by no other binding.
func (c Checker) PolicyBinding(ctx context.Context, pb *policybinding.PolicyBinding) error {
	switch pb.Meta.Owner.Kind {
	case meta.OwnerProject:
		if err := c.projectRef(ctx, pb.Meta.Owner.ID); err != nil {
			return err
		}
	case meta.OwnerTeam:
		if err := c.teamRef(ctx, pb.Meta.Owner.ID); err != nil {
			return err
		}
	}
	if err := c.BindablePolicy(ctx, pb.Spec.PolicyID, pb.Meta.Owner); err != nil {
		return err
	}
	if err := c.accountsInScope(ctx, pb); err != nil {
		return err
	}
	return c.accountsUnbound(ctx, pb)
}

// CodeDuplicateBinding is the error code of a binding naming an account another binding already holds.
const CodeDuplicateBinding = "duplicate_binding"

// accountsUnbound refuses an account another binding already holds: an account has one policy, with no priority between two.
func (c Checker) accountsUnbound(ctx context.Context, pb *policybinding.PolicyBinding) error {
	if c.Rows.PolicyBindings == nil {
		return nil
	}
	all, err := c.Rows.PolicyBindings(ctx)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	for _, other := range all {
		if other.Meta.ID == pb.Meta.ID {
			continue
		}
		for _, held := range other.Spec.AccountIDs {
			if !slices.Contains(pb.Spec.AccountIDs, held) {
				continue
			}
			by := "another policy binding"
			if Visible(ctx, c.Authz, "policy-binding", other.Meta.ID, other.Meta.Owner) {
				by = fmt.Sprintf("policy binding %q", other.Meta.Name)
			}
			return &Error{Status: http.StatusConflict, Code: CodeDuplicateBinding, Msg: fmt.Sprintf("account %s is already bound by %s; an account is bound to one policy", held, by)}
		}
	}
	return nil
}
