// Rules for shared accounts: a share names a real provider account at its own host, never another share, and is owned by an existing receiver the sharer sees and that does not reach the account already; only the real account's owner shares it. A shared account holds no keys, and a pool lists one only in the scope it was shared with.

package refcheck

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
)

// Codes of refused share writes.
const (
	// CodeAccountShared refuses a host key on a shared account: keys live on the real account.
	CodeAccountShared = "account_shared"
	// CodeReshare refuses sharing a shared account: only the real account's owner shares it.
	CodeReshare = "reshare_refused"
	// CodeShareExists refuses a second share of one account with one receiver.
	CodeShareExists = "share_exists"
	// CodeAlreadyReachable refuses sharing with a scope that already reaches the account through ownership.
	CodeAlreadyReachable = "already_reachable"
)

func conflict(code, format string, args ...any) error {
	return &Error{Status: http.StatusConflict, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// shareRef checks what a shared account names: its real account and its receiver. The real account need not be visible to the receiver.
func (c Checker) shareRef(ctx context.Context, a *account.Account) error {
	if c.Rows.Account != nil {
		src := c.Rows.Account(ctx, a.Spec.SourceAccountID)
		switch {
		case src == nil:
			return badRequest("account %q does not exist", a.Spec.SourceAccountID)
		case src.IsShared():
			return conflict(CodeReshare, "account %q is itself shared with this scope; only the owner of the real account shares it", src.Meta.Name)
		case !src.IsProvider():
			return badRequest("account %q is a %s account; only a provider account is shared", src.Meta.Name, src.Spec.Kind)
		case src.Spec.HostID != a.Spec.HostID:
			return badRequest("a shared account is held at the host of its real account %q", src.Meta.Name)
		}
	}
	return c.receiverExists(ctx, a)
}

// newShare checks what only a new share must meet: the caller sees the receiver, so ids cannot be probed and names do not leak through the share's name, and the receiver does not already reach the account by ownership, which would let it bypass the share's narrowing.
func (c Checker) newShare(ctx context.Context, src, share *account.Account) error {
	if err := c.receiverVisible(ctx, share.Receiver()); err != nil {
		return err
	}
	if c.containsScope(ctx, src.Meta.Owner, share.Meta.Owner) {
		return conflict(CodeAlreadyReachable, "%s already reaches account %q through ownership; a pool there can list the account itself, so sharing it adds nothing", ownerText(share.Meta.Owner), src.Meta.Name)
	}
	return nil
}

// receiverVisible answers a receiver the caller may not see as one that does not exist.
func (c Checker) receiverVisible(ctx context.Context, r account.Receiver) error {
	var visible bool
	switch r.Kind {
	case account.ReceiverUser:
		visible = c.Rows.UserVisible == nil || c.Rows.UserVisible(ctx, r.ID)
	case account.ReceiverTeam:
		t := c.Rows.Team
		visible = t == nil || (t(ctx, r.ID) != nil && Visible(ctx, c.Authz, "team", r.ID, t(ctx, r.ID).Meta.Owner))
	case account.ReceiverProject:
		p := c.Rows.Project
		visible = p == nil || (p(ctx, r.ID) != nil && Visible(ctx, c.Authz, "project", r.ID, p(ctx, r.ID).Meta.Owner))
	case account.ReceiverServiceAccount:
		s := c.Rows.ServiceAccount
		visible = s == nil || (s(ctx, r.ID) != nil && Visible(ctx, c.Authz, "service-account", r.ID, s(ctx, r.ID).Meta.Owner))
	}
	if !visible {
		return notVisible(string(r.Kind), r.ID)
	}
	return nil
}

// receiverExists refuses a share whose receiver names no row; a service account's share must be owned by its project.
func (c Checker) receiverExists(ctx context.Context, a *account.Account) error {
	r := a.Receiver()
	switch r.Kind {
	case account.ReceiverUser:
		if c.Rows.MissingUsers == nil {
			return nil
		}
		missing, err := c.Rows.MissingUsers(ctx, []string{r.ID})
		if err != nil {
			return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
		}
		if len(missing) > 0 {
			return notVisible("user", r.ID)
		}
	case account.ReceiverTeam:
		if c.Rows.Team != nil && c.Rows.Team(ctx, r.ID) == nil {
			return notVisible("team", r.ID)
		}
	case account.ReceiverProject:
		if c.Rows.Project != nil && c.Rows.Project(ctx, r.ID) == nil {
			return notVisible("project", r.ID)
		}
	case account.ReceiverServiceAccount:
		if c.Rows.ServiceAccount == nil {
			return nil
		}
		sa := c.Rows.ServiceAccount(ctx, r.ID)
		if sa == nil {
			return notVisible("service-account", r.ID)
		}
		if a.Meta.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}) {
			return badRequest("a service account's share is owned by its project %q", sa.Spec.ProjectID)
		}
	default:
		return badRequest("a shared account is owned by a user, team or project, got %q", a.Meta.Owner.Kind)
	}
	return nil
}

// MayShare refuses a caller who is not src's owner: sharing gives src's keys away. A personal account is shared by its user alone, never by an admin; a team's, project's or the system's by an admin of that scope (who may edit its accounts). A loader without an authorizer runs as the deployment.
func (c Checker) MayShare(ctx context.Context, src *account.Account) error {
	if c.Authz == nil {
		return nil
	}
	o := src.Meta.Owner
	if o.Kind == meta.OwnerUser {
		if a := actor.From(ctx); a != nil && !a.AdminToken && a.UserID == o.ID {
			return nil
		}
		return forbidden(fmt.Errorf("%w: account %q is personal; only its user shares it", authz.ErrForbidden, src.Meta.Name))
	}
	if err := c.Authz.Authorize(ctx, "accounts.update", authz.Resource{Kind: "account", ID: src.Meta.ID, Name: src.Meta.Name, Owner: &o}); err != nil {
		return forbidden(fmt.Errorf("%w: only an admin of %s shares account %q", authz.ErrForbidden, ownerText(o), src.Meta.Name))
	}
	return nil
}

// Share checks a new share of src: who shares it, the row itself, and what only a new share must meet.
func (c Checker) Share(ctx context.Context, src, share *account.Account) error {
	if src.IsShared() {
		return conflict(CodeReshare, "account %q is itself shared with this scope; only the owner of the real account shares it", src.Meta.Name)
	}
	if err := c.MayShare(ctx, src); err != nil {
		return err
	}
	if err := c.Account(ctx, share); err != nil {
		return err
	}
	return c.newShare(ctx, src, share)
}

// PoolAccountRefs checks that a pool owned by refOwner at hostID may draw on the accounts.
func (c Checker) PoolAccountRefs(ctx context.Context, ids []string, refOwner meta.Owner, hostID string) error {
	return c.accountRefs(ctx, ids, refOwner, hostID)
}

// accountRefs checks provider accounts a pool owned by refOwner draws on, all at hostID. A real account follows the rules for its keys; a shared account is listed only by a pool of the scope it was shared with.
func (c Checker) accountRefs(ctx context.Context, ids []string, refOwner meta.Owner, hostID string) error {
	if c.Rows.Account == nil {
		return nil
	}
	_, scoped := c.Authz.(authz.Scoper)
	for i, id := range ids {
		a := c.Rows.Account(ctx, id)
		if a == nil {
			return badRequest("accountIds[%d]: account %q does not exist", i, id)
		}
		if !Visible(ctx, c.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			return notVisible("account", id)
		}
		switch {
		case !a.IsProvider():
			return badRequest("accountIds[%d]: account %q is a %s account; a pool draws on provider accounts", i, a.Meta.Name, a.Spec.Kind)
		case a.Spec.HostID != hostID:
			return badRequest("accountIds[%d]: account %q is held at another host than %q", i, a.Meta.Name, hostID)
		case a.IsShared():
			if a.Meta.Owner != refOwner {
				return badRequest("accountIds[%d]: account %q is shared with %s; only a pool of that scope lists it", i, a.Meta.Name, ownerText(a.Meta.Owner))
			}
		case scoped && refOwner.Kind == meta.OwnerUser && a.Meta.Owner.Kind == meta.OwnerProject:
			return badRequest("personal rows cannot reference project resources")
		default:
			if err := c.linkable(ctx, "account", a.Meta.Name, a.Meta.Owner, refOwner); err != nil {
				return err
			}
		}
	}
	return nil
}
