package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/rolebinding"
)

// refs is the shared cross-row rule set, reading referenced rows from the
// stores. A store that is not wired skips the checks needing it.
func refs(d Deps) refcheck.Checker {
	c := refcheck.Checker{Authz: d.Authz}
	if d.Users != nil {
		c.Rows.MissingUsers = d.Users.MissingIDs
	}
	s := d.Stores
	if s == nil {
		return c
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
	}
	return c
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
		return huma.NewError(re.Status, re.Msg)
	}
	return err
}

func checkPolicyRefVisible(ctx context.Context, d Deps, policyID string, refOwner meta.Owner) error {
	return refErr(refs(d).PolicyRef(ctx, policyID, refOwner))
}

func checkRateLimitRefVisible(ctx context.Context, d Deps, rateLimitID string, refOwner meta.Owner) error {
	return refErr(refs(d).RateLimitRef(ctx, rateLimitID, refOwner))
}

func checkHostKeyRefsVisible(ctx context.Context, d Deps, keyIDs []string, refOwner meta.Owner) error {
	return refErr(refs(d).HostKeyRefs(ctx, keyIDs, refOwner))
}
