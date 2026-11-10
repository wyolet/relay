package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/rolebinding"
)

// refs is the shared cross-row rule set, reading referenced rows from the
// stores. A store that is not wired skips the checks needing it.
func refs(d Deps) refcheck.Checker {
	c := refcheck.Checker{Authz: d.Authz}
	if d.PlanView != nil {
		c.Rows.PersonalAccountOff = func(_ context.Context, userID string) (bool, string) {
			if p := personalAccountState(d, userID); p != nil && !p.On {
				return true, p.Username
			}
			return false, ""
		}
	}
	if d.Users != nil {
		c.Rows.MissingUsers = d.Users.MissingIDs
		c.Rows.UserVisible = d.userVisible
		c.Rows.UserName = func(ctx context.Context, id string) string {
			if u, err := d.Users.Get(ctx, id); err == nil && u != nil {
				return u.Username
			}
			return ""
		}
		if d.Authz != nil {
			c.Rows.UserMayBindIn = userMayBindIn(d)
		}
	}
	s := d.Stores
	if s == nil {
		return c
	}
	if s.Account != nil && d.Users != nil && s.ServiceAccount != nil {
		c.Rows.DefaultAccount = defaultAccount(d)
	}
	if s.Policy != nil {
		c.Rows.Policy = getOrNil(s.Policy.Get)
	}
	if s.RateLimit != nil {
		c.Rows.RateLimit = getOrNil(s.RateLimit.Get)
	}
	if s.HostKey != nil {
		c.Rows.HostKey = getOrNil(s.HostKey.Get)
	}
	if s.Host != nil {
		c.Rows.Host = getOrNil(s.Host.Get)
	}
	if s.Account != nil {
		c.Rows.Account = getOrNil(s.Account.Get)
	}
	if s.Pool != nil {
		c.Rows.Pool = getOrNil(s.Pool.Get)
	}
	if s.Budget != nil {
		c.Rows.Budget = getOrNil(s.Budget.Get)
	}
	if s.Project != nil {
		c.Rows.Project = getOrNil(s.Project.Get)
	}
	if s.Team != nil {
		c.Rows.Team = getOrNil(s.Team.Get)
	}
	if s.Role != nil {
		c.Rows.Role = getOrNil(s.Role.Get)
	}
	if s.ServiceAccount != nil {
		c.Rows.ServiceAccount = getOrNil(s.ServiceAccount.Get)
	}
	if s.RoleBinding != nil {
		c.Rows.RoleBindingsFor = func(ctx context.Context, roleID string) ([]*rolebinding.RoleBinding, error) {
			all, err := s.RoleBinding.List(ctx)
			if err != nil {
				return nil, err
			}
			var out []*rolebinding.RoleBinding
			for _, rb := range all {
				if rb.Spec.RoleID == roleID {
					out = append(out, rb)
				}
			}
			return out, nil
		}
		c.Rows.RoleBindings = s.RoleBinding.List
	}
	if s.PolicyBinding != nil {
		c.Rows.PolicyBindings = s.PolicyBinding.List
		c.Rows.PolicyBindingsFor = func(ctx context.Context, policyID string) ([]*policybinding.PolicyBinding, error) {
			all, err := s.PolicyBinding.List(ctx)
			if err != nil {
				return nil, err
			}
			var out []*policybinding.PolicyBinding
			for _, pb := range all {
				if pb.Spec.PolicyID == policyID {
					out = append(out, pb)
				}
			}
			return out, nil
		}
	}
	return c
}

// defaultAccount returns the id of a principal's own account, making it when relay has not yet: a key may be created for a user or service account written a moment ago.
func defaultAccount(d Deps) func(context.Context, key.Principal) (string, error) {
	return func(ctx context.Context, p key.Principal) (string, error) {
		switch p.Kind {
		case key.PrincipalUser:
			u, err := d.Users.Get(ctx, p.ID)
			if err != nil {
				return "", err
			}
			if u == nil {
				return "", fmt.Errorf("user %q does not exist", p.ID)
			}
			return d.Stores.Account.EnsureManaged(ctx, account.Personal(u.ID, u.Username))
		case key.PrincipalServiceAccount:
			sa, err := d.Stores.ServiceAccount.Get(ctx, p.ID)
			if err != nil {
				return "", err
			}
			if sa == nil {
				return "", fmt.Errorf("service account %q does not exist", p.ID)
			}
			return d.Stores.Account.EnsureManaged(ctx, account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Spec.ProjectID))
		}
		return "", fmt.Errorf("principal kind %q has no account of its own", p.Kind)
	}
}

// userMayBindIn reports whether a user may create policy bindings owned by scope, asking the Authorizer as that user.
func userMayBindIn(d Deps) func(context.Context, string, meta.Owner) (bool, error) {
	return func(ctx context.Context, userID string, scope meta.Owner) (bool, error) {
		return authorizeAsUser(ctx, d, userID, "policy-bindings.create", "policy-binding", scope)
	}
}

// authorizeAsUser asks the Authorizer whether userID may perform action on a row of kind owned by scope.
func authorizeAsUser(ctx context.Context, d Deps, userID, action, kind string, scope meta.Owner) (bool, error) {
	u, err := d.Users.Get(ctx, userID)
	if err != nil || u == nil {
		return false, err
	}
	as := &actor.Actor{UserID: u.ID, Username: u.Username, Roles: u.Roles}
	if d.Catalog != nil {
		if snap := d.Catalog.Current(); snap != nil {
			as.Subjects = appcatalog.UserSubjects(u.ID, snap.GroupsForUser(u.ID), nil)
		}
	}
	err = d.Authz.Authorize(actor.WithActor(ctx, as), action, authz.Resource{Kind: kind, Owner: &scope})
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, authz.ErrForbidden):
		return false, err
	}
	return false, nil
}

func getOrNil[T any](get func(context.Context, string) (*T, error)) func(context.Context, string) *T {
	return func(ctx context.Context, id string) *T {
		v, err := get(ctx, id)
		if err != nil {
			return nil
		}
		return v
	}
}

// refErr renders a refcheck refusal as the HTTP error it carries.
func refErr(err error) error {
	var re *refcheck.Error
	if errors.As(err, &re) {
		if re.Code != "" {
			return &httpapi.APIError{
				Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: re.Code, Message: re.Msg},
				HTTPStatus: re.Status,
			}
		}
		return huma.NewError(re.Status, re.Msg)
	}
	return err
}
