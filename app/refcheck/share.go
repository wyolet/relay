// Rules for shared accounts: a share names a real provider account at its own host, never another share, and is owned by an existing receiver the sharer sees and that does not reach the account already; only the real account's owner shares it. A shared account holds no keys, and a pool lists one only in the scope it was shared with.

package refcheck

import (
	"context"
	"errors"
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
			return c.reshareRefusal(ctx, src)
		case !src.IsProvider():
			return badRequest("%s is a %s account; only a provider account is shared", c.rowText(ctx, "account", src.Meta), src.Spec.Kind)
		case src.Spec.HostID != a.Spec.HostID:
			return badRequest("a share is held at the host of the account it comes from, %s", c.rowText(ctx, "account", src.Meta))
		}
	}
	return c.receiverExists(ctx, a)
}

// newShare checks what only a new share must meet: the caller sees the receiver, so ids cannot be probed and names do not leak through the share's name; a user receiver's personal account is on; and the receiver does not already reach the account by ownership, which would let it bypass the share's narrowing.
func (c Checker) newShare(ctx context.Context, src, share *account.Account) error {
	if err := c.receiverVisible(ctx, share.Receiver()); err != nil {
		return err
	}
	if r := share.Receiver(); r.Kind == account.ReceiverUser {
		if off, name := c.personalAccountOff(ctx, r.ID); off {
			return personalAccountOffError("personal accounts are turned off for %s, so a share with them would serve nothing; share with their team or project instead", name)
		}
	}
	if c.containsScope(ctx, src.Meta.Owner, share.Meta.Owner) {
		return conflict(CodeAlreadyReachable, "%s already reaches %s as its owner or a scope inside it, so a share adds nothing. List the account in a pool of %s instead.",
			c.scopeText(ctx, share.Meta.Owner), c.rowText(ctx, "account", src.Meta), c.scopeText(ctx, share.Meta.Owner))
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
		if home := (meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}); a.Meta.Owner != home {
			return badRequest("a share with %s belongs to its project, %s; leave the owner out or set it to that project", c.rowText(ctx, "service-account", sa.Meta), c.scopeText(ctx, home))
		}
	default:
		return badRequest("an account is shared with a user, a team, a project or a service account, not a %s", a.Meta.Owner.Kind)
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
		return forbidden(fmt.Errorf("%w: %s is personal: only its user shares it; ask them to share it", authz.ErrForbidden, c.rowText(ctx, "account", src.Meta)))
	}
	if err := c.Authz.Authorize(ctx, "accounts.update", authz.Resource{Kind: "account", ID: src.Meta.ID, Name: src.Meta.Name, Owner: &o}); err != nil {
		return forbidden(fmt.Errorf("%w: only an admin of %s shares %s; ask one to share it", authz.ErrForbidden, c.scopeText(ctx, o), c.rowText(ctx, "account", src.Meta)))
	}
	return nil
}

// reshareRefusal refuses sharing a share on: only the owner of the account it came from gives it away.
func (c Checker) reshareRefusal(ctx context.Context, src *account.Account) error {
	return conflict(CodeReshare, "%s is a share, so it cannot be shared on: only the owner of the account it came from shares that account. Ask them to share it with the receiver.",
		c.rowText(ctx, "account", src.Meta))
}

// ShareExists is the refusal of a second share of src with r.
func (c Checker) ShareExists(ctx context.Context, src *account.Account, r account.Receiver) error {
	return conflict(CodeShareExists, "%s is already shared with %s: one share per receiver. Use the existing share, or delete it first.",
		c.rowText(ctx, "account", src.Meta), c.receiverText(ctx, r))
}

// receiverText names who a share goes to: "you", alice, `team "Platform"`, `service account "ci"`.
func (c Checker) receiverText(ctx context.Context, r account.Receiver) string {
	switch r.Kind {
	case account.ReceiverUser:
		if s := c.userSubject(ctx, r.ID); s != "its owner" {
			return s
		}
		return "that user"
	case account.ReceiverServiceAccount:
		if c.Rows.ServiceAccount != nil {
			if sa := c.Rows.ServiceAccount(ctx, r.ID); sa != nil && Visible(ctx, c.Authz, "service-account", sa.Meta.ID, sa.Meta.Owner) {
				return fmt.Sprintf("service account %q", label(sa.Meta))
			}
		}
		return "that service account"
	}
	return c.scopeText(ctx, meta.Owner{Kind: meta.OwnerKind(r.Kind), ID: r.ID})
}

// Share checks a new share of src: who shares it, the row itself, and what only a new share must meet.
func (c Checker) Share(ctx context.Context, src, share *account.Account) error {
	if src.IsShared() {
		return c.reshareRefusal(ctx, src)
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
	for i, id := range ids {
		a := c.Rows.Account(ctx, id)
		if a == nil {
			return badRequest("accountIds[%d]: account %q does not exist", i, id)
		}
		err := c.poolAccountUsable(ctx, a, refOwner)
		if err == nil && a.Spec.HostID != hostID {
			err = badRequest("%s is held at another host than the pool's; a pool draws on accounts of its own host", c.rowText(ctx, "account", a.Meta))
		}
		var re *Error
		if errors.As(err, &re) && re.Status == http.StatusBadRequest {
			re.Msg = fmt.Sprintf("accountIds[%d]: %s", i, re.Msg)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// poolAccountUsable is the scope rule for one account a pool owned by refOwner lists, the host aside: visible, a provider account, and either a share made with refOwner itself or a real account refOwner may draw on as it may on keys.
func (c Checker) poolAccountUsable(ctx context.Context, a *account.Account, refOwner meta.Owner) error {
	if !Visible(ctx, c.Authz, "account", a.Meta.ID, a.Meta.Owner) {
		return notVisible("account", a.Meta.ID)
	}
	_, scoped := c.Authz.(authz.Scoper)
	switch {
	case !a.IsProvider():
		return badRequest("%s is a %s account; a pool draws on provider accounts", c.rowText(ctx, "account", a.Meta), a.Spec.Kind)
	case a.IsShared():
		if a.Meta.Owner != refOwner {
			return badRequest("account %q was shared with %s, so only a pool of %s lists it. Create the pool there, or ask the owner of the account it came from to share it with %s.",
				label(a.Meta), c.scopeText(ctx, a.Meta.Owner), c.scopeText(ctx, a.Meta.Owner), c.scopeText(ctx, refOwner))
		}
	case scoped && refOwner.Kind == meta.OwnerUser && a.Meta.Owner.Kind == meta.OwnerProject:
		return badRequest("a pool of %s cannot draw on %s: a personal pool cannot draw on a project's accounts. Use an account of your own or of the system, or create the pool in that project.",
			c.scopeText(ctx, refOwner), c.rowText(ctx, "account", a.Meta))
	default:
		return c.linkable(ctx, "pool", "account", a.Meta, refOwner)
	}
	return nil
}
