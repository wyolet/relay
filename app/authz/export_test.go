package authz

import (
	"context"

	"github.com/wyolet/relay/auth/rbac"
)

// ScopeOf exposes RBAC.scopeOf to the external test package.
func ScopeOf(ctx context.Context, r RBAC, verb, kind string) (rbac.Filter, error) {
	return r.scopeOf(ctx, verb, kind)
}
