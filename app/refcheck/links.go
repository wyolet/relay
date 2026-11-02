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

// accountRef checks the account a host key names: visible, owned by the key's own owner, and held at the key's host.
func (c Checker) accountRef(ctx context.Context, k *hostkey.HostKey) error {
	if k.Spec.AccountID == "" || c.Rows.Account == nil {
		return nil
	}
	a := c.Rows.Account(ctx, k.Spec.AccountID)
	if a == nil {
		return badRequest("account %q does not exist", k.Spec.AccountID)
	}
	if err := c.joinable(ctx, k.Spec.AccountID, a.Meta, k.Meta.Owner); err != nil {
		return err
	}
	if !a.IsProvider() {
		return badRequest("account %q is a %s account; a host key joins a provider account", a.Meta.Name, a.Spec.Kind)
	}
	if a.IsShared() {
		return conflict(CodeAccountShared, "%s is a share and holds no keys of its own: its keys are the ones of the account it came from. Add the host key to that account, or join an account of %s.",
			c.rowText(ctx, "account", a.Meta), c.scopeText(ctx, k.Meta.Owner))
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
		if err := c.linkRef(ctx, "policy", "pool", p.Meta, refOwner); err != nil {
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
		if err := c.linkRef(ctx, "policy", "budget", b.Meta, refOwner); err != nil {
			return err
		}
	}
	return nil
}

// credentialRefs checks host keys linked by a policy or a pool (from names which); a missing key is refused. A non-empty hostID requires every key to authenticate to that host.
func (c Checker) credentialRefs(ctx context.Context, from string, ids []string, refOwner meta.Owner, hostID string) error {
	if c.Rows.HostKey == nil {
		return nil
	}
	for _, id := range ids {
		k := c.Rows.HostKey(ctx, id)
		if k == nil {
			return badRequest("host-key %q does not exist", id)
		}
		if err := c.credentialUsable(ctx, from, k, refOwner); err != nil {
			return err
		}
		if hostID != "" && k.Spec.HostID != hostID {
			return badRequest("host-key %q authenticates to another host than %q", k.Meta.Name, hostID)
		}
	}
	return nil
}

// credentialUsable is the owner rule for one host key a row owned by refOwner holds: visible, and in a scope the holder may draw on. Under a scoping authorizer a personal row never holds a project's key, member or not: it would spend the project's credential outside the project's limits.
func (c Checker) credentialUsable(ctx context.Context, from string, k *hostkey.HostKey, refOwner meta.Owner) error {
	if !Visible(ctx, c.Authz, "host-key", k.Meta.ID, k.Meta.Owner) {
		return notVisible("host-key", k.Meta.ID)
	}
	if _, scoped := c.Authz.(authz.Scoper); scoped && refOwner.Kind == meta.OwnerUser && k.Meta.Owner.Kind == meta.OwnerProject {
		return badRequest("a %s of %s cannot hold %s: a personal %s cannot hold a project's host keys. Use a host key of your own or of the system, or create the %s in that project.",
			kindWord(from), c.scopeText(ctx, refOwner), c.rowText(ctx, "host-key", k.Meta), kindWord(from), kindWord(from))
	}
	return c.linkable(ctx, from, "host-key", k.Meta, refOwner)
}

// CredentialRefs checks host keys a policy owned by refOwner links.
func (c Checker) CredentialRefs(ctx context.Context, ids []string, refOwner meta.Owner) error {
	return c.credentialRefs(ctx, "policy", ids, refOwner, "")
}

// AccountRef checks that a host key owned by refOwner may name the account.
func (c Checker) AccountRef(ctx context.Context, id string, refOwner meta.Owner) error {
	if c.Rows.Account == nil {
		return nil
	}
	a := c.Rows.Account(ctx, id)
	if a == nil {
		return badRequest("account %q does not exist", id)
	}
	return c.joinable(ctx, id, a.Meta, refOwner)
}

// joinable lets a key join only an account its own owner owns: a key in an account shares the account's counters and its 429 cooldown, which changes state the account's other keys depend on, so using a shared account is not enough.
func (c Checker) joinable(ctx context.Context, id string, m meta.Metadata, keyOwner meta.Owner) error {
	if !Visible(ctx, c.Authz, "account", m.ID, m.Owner) {
		return notVisible("account", id)
	}
	if m.Owner == keyOwner {
		return nil
	}
	return badRequest("a host key of %s cannot join %s: a host key joins only an account of its own owner. Create the host key in %s, or pick an account of %s.",
		c.scopeText(ctx, keyOwner), c.rowText(ctx, "account", m), c.scopeText(ctx, m.Owner), c.scopeText(ctx, keyOwner))
}

// PoolRefs checks pools a policy owned by refOwner links.
func (c Checker) PoolRefs(ctx context.Context, ids []string, refOwner meta.Owner) error {
	return c.poolRefs(ctx, ids, refOwner)
}

// BudgetRefs checks budgets a policy owned by refOwner links.
func (c Checker) BudgetRefs(ctx context.Context, ids []string, refOwner meta.Owner) error {
	return c.budgetRefs(ctx, ids, refOwner)
}

// linkRef applies the shared rules to one row of kind a row of kind from, owned by refOwner, links: the caller sees it, and it lives in a scope the referrer may draw on.
func (c Checker) linkRef(ctx context.Context, from, kind string, m meta.Metadata, refOwner meta.Owner) error {
	if !Visible(ctx, c.Authz, kind, m.ID, m.Owner) {
		return notVisible(kind, m.ID)
	}
	if err := c.personalToProject(ctx, from, kind, m, refOwner); err != nil {
		return err
	}
	return c.linkable(ctx, from, kind, m, refOwner)
}

// linkable extends SharedOwner to team-owned rows: a team's row is usable by the team itself and by its projects, and a team's row may draw only on shared rows and its own.
func (c Checker) linkable(ctx context.Context, from, kind string, m meta.Metadata, refOwner meta.Owner) error {
	if c.reaches(ctx, m.Owner, refOwner) {
		return nil
	}
	return badRequest("a %s of %s cannot use %s: %s %s can use %s of %s. Create the %s in %s, or move this one there.",
		kindWord(from), c.scopeText(ctx, refOwner), c.rowText(ctx, kind, m),
		scopeArticle(refOwner), kindWord(from), kindWords(kind), reachText(refOwner),
		kindWord(kind), homeText(refOwner))
}

// reaches reports whether a row owned by refOwner may draw on a row owned by owner: a project on its own, its team's and the system's rows, a team on its own and the system's, anyone else on any row (the caller's visibility and personalToProject narrow those).
func (c Checker) reaches(ctx context.Context, owner, refOwner meta.Owner) bool {
	switch {
	case owner.Kind == meta.OwnerTeam && refOwner.Kind == meta.OwnerProject:
		return c.projectInTeam(ctx, refOwner.ID, owner.ID)
	case refOwner.Kind == meta.OwnerTeam:
		return owner.Kind == meta.OwnerSystem || owner == refOwner
	}
	return sharedOwnerReaches(owner, refOwner)
}

// projectInTeam reports whether the project belongs to the team; without a project lookup the check is skipped.
func (c Checker) projectInTeam(ctx context.Context, projectID, teamID string) bool {
	if c.Rows.Project == nil {
		return true
	}
	p := c.Rows.Project(ctx, projectID)
	return p != nil && p.Spec.TeamID == teamID
}

// personalRefusal is the refusal of a personal row drawing on a project's row the caller may not create keys in.
func (c Checker) personalRefusal(ctx context.Context, from, kind string, m meta.Metadata, refOwner meta.Owner) error {
	return badRequest("a %s of %s cannot use %s: a personal row uses a project's rows only where you may create keys in that project. Use a %s of your own or of the system, or ask for a role in %s.",
		kindWord(from), c.scopeText(ctx, refOwner), c.rowText(ctx, kind, m), kindWord(kind), c.scopeText(ctx, m.Owner))
}
