// Rules for the rows a policy, pool or host key links to: hosts, accounts, pools, credentials and budgets. A missing row answers 400, a row the caller may not see answers 404, as for the older references.

package refcheck

import (
	"context"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

func (c Checker) hostRef(ctx context.Context, hostID string) error {
	if c.Rows.Host == nil {
		return nil
	}
	h := c.Rows.Host(ctx, hostID)
	if h == nil {
		return badRequest("host %q does not exist", hostID)
	}
	if !Visible(ctx, c.Authz, "host", h.Meta.ID, h.Meta.Owner) {
		return notVisible("host", hostID)
	}
	return nil
}

// accountRef checks the account a host key names: visible, usable from the key's scope, and held at the key's host.
func (c Checker) accountRef(ctx context.Context, k *hostkey.HostKey) error {
	if k.Spec.AccountID == "" || c.Rows.Account == nil {
		return nil
	}
	a := c.Rows.Account(ctx, k.Spec.AccountID)
	if a == nil {
		return badRequest("account %q does not exist", k.Spec.AccountID)
	}
	if err := c.linkRef(ctx, "account", k.Spec.AccountID, a.Meta, k.Meta.Owner); err != nil {
		return err
	}
	if a.Spec.HostID != k.Spec.HostID {
		return badRequest("account %q is held at another host than the key's host %q", a.Meta.Name, k.Spec.HostID)
	}
	return nil
}

func (c Checker) poolRefs(ctx context.Context, ids []string, refOwner meta.Owner) error {
	if c.Rows.Pool == nil {
		return nil
	}
	for _, id := range ids {
		p := c.Rows.Pool(ctx, id)
		if p == nil {
			return badRequest("pool %q does not exist", id)
		}
		if err := c.linkRef(ctx, "pool", id, p.Meta, refOwner); err != nil {
			return err
		}
	}
	return nil
}

func (c Checker) budgetRefs(ctx context.Context, ids []string, refOwner meta.Owner) error {
	if c.Rows.Budget == nil {
		return nil
	}
	for _, id := range ids {
		b := c.Rows.Budget(ctx, id)
		if b == nil {
			return badRequest("budget %q does not exist", id)
		}
		if err := c.linkRef(ctx, "budget", id, b.Meta, refOwner); err != nil {
			return err
		}
	}
	return nil
}

// credentialRefs checks host keys linked by a policy or a pool. Unlike HostKeyRefs a missing key is refused. A non-empty hostID requires every key to authenticate to that host.
func (c Checker) credentialRefs(ctx context.Context, ids []string, refOwner meta.Owner, hostID string) error {
	if c.Rows.HostKey == nil {
		return nil
	}
	_, scoped := c.Authz.(authz.Scoper)
	for _, id := range ids {
		k := c.Rows.HostKey(ctx, id)
		if k == nil {
			return badRequest("host-key %q does not exist", id)
		}
		if !Visible(ctx, c.Authz, "host-key", k.Meta.ID, k.Meta.Owner) {
			return notVisible("host-key", id)
		}
		if scoped && refOwner.Kind == meta.OwnerUser && k.Meta.Owner.Kind == meta.OwnerProject {
			return badRequest("personal rows cannot reference project resources")
		}
		if err := c.linkable(ctx, "host-key", k.Meta.Name, k.Meta.Owner, refOwner); err != nil {
			return err
		}
		if hostID != "" && k.Spec.HostID != hostID {
			return badRequest("host-key %q authenticates to another host than %q", k.Meta.Name, hostID)
		}
	}
	return nil
}

// linkRef applies the shared rules to one linked row: the caller sees it, and it lives in a scope the referrer may draw on.
func (c Checker) linkRef(ctx context.Context, kind, id string, m meta.Metadata, refOwner meta.Owner) error {
	if !Visible(ctx, c.Authz, kind, m.ID, m.Owner) {
		return notVisible(kind, id)
	}
	if err := c.personalToProject(ctx, m.Owner, refOwner); err != nil {
		return err
	}
	return c.linkable(ctx, kind, m.Name, m.Owner, refOwner)
}

// linkable extends SharedOwner to team-owned rows: a team's row is usable by the team itself and by its projects, and a team's row may draw only on shared rows and its own.
func (c Checker) linkable(ctx context.Context, kind, name string, owner, refOwner meta.Owner) error {
	switch {
	case owner.Kind == meta.OwnerTeam && refOwner.Kind == meta.OwnerProject:
		if c.projectInTeam(ctx, refOwner.ID, owner.ID) {
			return nil
		}
	case refOwner.Kind == meta.OwnerTeam:
		if owner.Kind == meta.OwnerSystem || owner == refOwner {
			return nil
		}
	default:
		return SharedOwner(kind, name, owner, refOwner)
	}
	return badRequest("%s %q belongs to another scope (%s) and cannot be referenced from this %s", kind, name, owner.Kind, refOwner.Kind)
}

// projectInTeam reports whether the project belongs to the team; without a project lookup the check is skipped.
func (c Checker) projectInTeam(ctx context.Context, projectID, teamID string) bool {
	if c.Rows.Project == nil {
		return true
	}
	p := c.Rows.Project(ctx, projectID)
	return p != nil && p.Spec.TeamID == teamID
}
