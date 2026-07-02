package authz

import (
	"context"
	"strings"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

// scopedReadKinds are the kinds whose read handlers scope rows to the
// caller: catalog kinds per-row via Visible on meta.Owner, and the
// usage/logs read surface via the caller's owned relay-key hashes
// (relayKeyScope in httpapi/control). Reads on any other kind (settings,
// debug, ...) have no scoped read path, so OwnerScoped grants them to
// admins only.
var scopedReadKinds = map[string]bool{
	"provider":     true,
	"host":         true,
	"model":        true,
	"host-key":     true,
	"rate-limit":   true,
	"policy":       true,
	"pricing":      true,
	"host-binding": true,
	"relay-key":    true,
	"usage":        true,
	"logs":         true,
}

// OwnerScoped grants each authenticated user access to catalog reads and
// to rows they own, and reserves everything else for admins (the "admin"
// role or the RELAY_ADMIN_TOKEN break-glass).
//
// The policy, per row owner:
//
//   - system / provider / host (catalog rows): readable by everyone,
//     mutable by admins only (still behind settings.Governs).
//   - user rows with owner.ID == caller: fully owned.
//   - user rows with empty owner.ID (operator/grandfathered rows, incl.
//     everything the admin token creates): admins only.
//
// Mutations fail closed: an Authorize call that carries no Resource.Owner
// is treated as "not provably owned" and denied for non-admins. Handlers
// that operate on a specific row must fetch it and pass its owner.
type OwnerScoped struct{}

// Authorize implements Authorizer.
func (OwnerScoped) Authorize(ctx context.Context, action string, res Resource) error {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return ErrUnauthenticated
	}
	if adminActor(a) {
		return nil
	}
	switch verbOf(action) {
	case "list", "read":
		if scopedReadKinds[res.Kind] {
			return nil // rows are gated per-owner via Visible
		}
		return ErrForbidden
	}
	if res.Owner != nil && ownedBy(*res.Owner, a) {
		return nil
	}
	return ErrForbidden
}

// Visible implements Scoper.
func (OwnerScoped) Visible(ctx context.Context, _ string, owner meta.Owner) bool {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return false
	}
	if adminActor(a) {
		return true
	}
	if owner.Kind != meta.OwnerUser {
		return true // catalog rows are shared
	}
	return ownedBy(owner, a)
}

func ownedBy(o meta.Owner, a *actor.Actor) bool {
	return o.Kind == meta.OwnerUser && o.ID != "" && o.ID == a.UserID
}

func adminActor(a *actor.Actor) bool {
	return a.AdminToken || a.HasRole(user.RoleAdmin)
}

func verbOf(action string) string {
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		return action[i+1:]
	}
	return action
}
