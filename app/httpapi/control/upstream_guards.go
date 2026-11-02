package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
)

// codeAccountManaged refuses a write to the place relay keeps a caller account in.
const codeAccountManaged = "account_managed"

func accountManaged(msg string) error {
	return &httpapi.APIError{
		Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeAccountManaged, Message: msg},
		HTTPStatus: http.StatusConflict,
	}
}

// guardAccount checks the account's host and refuses an owner move that
// would leave a host key naming an account it may not use. Caller accounts
// are relay's: none is created through the API, and only a closed member
// account is deleted; the others go with their team, project, user or
// service account.
func guardAccount(d Deps) mutationGuard[account.Account] {
	return func(ctx context.Context, action string, existing, incoming *account.Account) error {
		switch {
		case action == "delete":
			if k := existing.DerivedKind(); k != account.KindProvider && !(k == account.KindMember && existing.Spec.Closed) {
				return accountManaged(fmt.Sprintf("account %q is a %s account: it goes with its %s; only a closed member account is deleted on its own", existing.Meta.Name, k, accountSource(k)))
			}
			return nil
		case incoming == nil:
			return nil
		case action == "create" && !incoming.IsProvider():
			return huma.Error400BadRequest("spec.hostId is required: team, project, personal, member and service accounts are made by relay with their team, project, user, membership or service account")
		case action == "create" && incoming.IsShared():
			return huma.Error400BadRequest("spec.sourceAccountId is set by sharing: POST /accounts/{ref}/shares shares the real account")
		}
		if err := refs(d).Account(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action == "update" {
			if err := refs(d).AccountPayloadLogging(ctx, existing, incoming); err != nil {
				return refErr(err)
			}
		}
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Account = func(context.Context, string) *account.Account { return incoming }
		return refuseStrandingMove(ctx, d, "account", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			switch it.Kind {
			case "host-key":
				return c.AccountRef(ctx, incoming.Meta.ID, it.owner) == nil
			case "pool":
				return c.PoolAccountRefs(ctx, []string{incoming.Meta.ID}, it.owner, incoming.Spec.HostID) == nil
			}
			return false
		})
	}
}

func accountSource(k account.Kind) string {
	switch k {
	case account.KindPersonal:
		return "user"
	case account.KindService:
		return "service account"
	case account.KindMember:
		return "membership"
	}
	return string(k)
}

// mergeAccountServerFields keeps what relay owns on an account through a PUT: the kind, holder, closed mark and the account it was shared from, for a caller account its owner and host (none), which fix its place, and for a shared account its owner and host, which follow the share.
func mergeAccountServerFields(existing, incoming *account.Account) {
	incoming.Spec.Kind = existing.Spec.Kind
	incoming.Spec.Holder = existing.Spec.Holder
	incoming.Spec.Closed = existing.Spec.Closed
	incoming.Spec.SourceAccountID, incoming.Spec.SharedBy = existing.Spec.SourceAccountID, existing.Spec.SharedBy
	incoming.FollowsRoleBindings = existing.FollowsRoleBindings
	switch {
	case !existing.IsProvider():
		incoming.Meta.Owner = existing.Meta.Owner
		incoming.Spec.HostID = ""
	case existing.IsShared():
		incoming.Meta.Owner = existing.Meta.Owner
		incoming.Spec.HostID = existing.Spec.HostID
	}
}

// guardKeyAccount checks a key's lane when an update changes it.
func guardKeyAccount(d Deps) mutationGuard[key.Key] {
	return func(ctx context.Context, action string, existing, incoming *key.Key) error {
		if action != "update" || incoming.Spec.AccountID == existing.Spec.AccountID {
			return nil
		}
		if err := refs(d).KeyAccount(ctx, incoming); err != nil {
			return refErr(err)
		}
		return nil
	}
}

// guardPool checks the pool's host and keys, and refuses an owner move away
// from the policies drawing on it.
func guardPool(d Deps) mutationGuard[pool.Pool] {
	return func(ctx context.Context, action string, existing, incoming *pool.Pool) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if err := refs(d).Pool(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Pool = func(context.Context, string) *pool.Pool { return incoming }
		return refuseStrandingMove(ctx, d, "pool", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			return it.Kind == "policy" && c.PoolRefs(ctx, []string{incoming.Meta.ID}, it.owner) == nil
		})
	}
}

// guardBudgetMove refuses an owner move away from the policies counting
// against the budget.
func guardBudgetMove(d Deps) mutationGuard[budget.Budget] {
	return func(ctx context.Context, action string, existing, incoming *budget.Budget) error {
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Budget = func(context.Context, string) *budget.Budget { return incoming }
		return refuseStrandingMove(ctx, d, "budget", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			return it.Kind == "policy" && c.BudgetRefs(ctx, []string{incoming.Meta.ID}, it.owner) == nil
		})
	}
}
