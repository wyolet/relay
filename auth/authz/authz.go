// Package authz holds what every authorization decision shares: the resource a check names and the two refusals. It decides nothing itself; auth/rbac does. Kinds, ids and owner kinds are opaque product strings.
package authz

import "errors"

// ErrUnauthenticated refuses a caller with no authenticated principal.
var ErrUnauthenticated = errors.New("authz: unauthenticated")

// ErrForbidden refuses an authenticated caller that lacks the permission.
var ErrForbidden = errors.New("authz: forbidden")

// Owner is who or what a resource belongs to.
type Owner struct {
	Kind string
	ID   string
}

// Resource is the target of a check. Populate what the action needs.
type Resource struct {
	Kind string
	ID   string
	Name string
	// Owner nil means the owner is unknown, as in a list or a create without one; rules must not read that as an owner they would allow.
	Owner *Owner
}
