// Deleting a share revokes it: the owner of the real account, or an admin of the receiving scope, always gets through, and the answer lists what the revoke changed (names where the caller may see the row, counts otherwise). Deleting a real account its shares still name is refused until they are revoked.

package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/share"
)

// accountRows is the account store as the CRUD routes write through it: a real account a share was made for while its delete ran answers 409 with the blockers, never takes the share along.
type accountRows struct {
	*account.Store
	d Deps
}

func (s accountRows) Delete(ctx context.Context, id string) error {
	err := s.Store.Delete(ctx, id)
	if errors.Is(err, account.ErrShared) {
		name := id
		if a, _ := s.Get(ctx, id); a != nil {
			name = a.Meta.Name
		}
		if _, inUse := s.d.refuseInUse(ctx, "account", id, name); inUse != nil {
			return inUse
		}
		return huma.Error409Conflict("a share was made while the account was being deleted; retry")
	}
	return err
}

// accountInUse answers an account delete: a share is revoked here, in one transaction, and the answer lists what that changed; any other account goes through refuseInUse.
func (d Deps) accountInUse(ctx context.Context, kind, id, name string) (*droppedLinks, error) {
	a, err := d.Stores.Account.Get(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if a == nil || !a.IsShared() {
		return d.refuseInUse(ctx, kind, id, name)
	}
	var changes []share.Change
	err = d.Stores.InTx(ctx, func(ctx context.Context, tx *appcatalog.Stores) error {
		var err error
		changes, err = share.Revoke(ctx, d.boundTo(tx).shareStores(), id)
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	return d.revokedLinks(ctx, changes), nil
}

// shareStores are the stores a revoke writes through.
func (d Deps) shareStores() share.Stores {
	s := share.Stores{Account: d.Stores.Account, PolicyBinding: d.Stores.PolicyBinding, Key: d.Stores.Key, Pool: d.Stores.Pool}
	if d.Users != nil && d.Stores.ServiceAccount != nil {
		own := defaultAccount(d)
		s.OwnAccount = func(ctx context.Context, p key.Principal) (string, error) {
			// A key moved into a personal account that is off would only stop there; it is left without a lane and serves again in the personal account once it is on.
			if p.Kind == key.PrincipalUser {
				if st := personalAccountState(d, p.ID); st != nil && !st.On {
					return "", nil
				}
			}
			return own(ctx, p)
		}
	}
	return s
}

// revokeItems are a revoke's changes as references, grouped like blockers in the answer.
func revokeItems(changes []share.Change) []referenceItem {
	out := make([]referenceItem, 0, len(changes))
	for _, c := range changes {
		out = append(out, referenceItem{Kind: c.Kind, ID: c.ID, Name: c.Name, Via: c.Field, owner: c.Owner, deleted: c.Deleted})
	}
	return out
}

// revokedLinks is the delete answer for a revoke, and an audit row per changed row.
func (d Deps) revokedLinks(ctx context.Context, changes []share.Change) *droppedLinks {
	if len(changes) == 0 {
		return nil
	}
	items := revokeItems(changes)
	dl := &droppedLinks{groups: groupBlockers(ctx, d.Authz, items)}
	for _, it := range items {
		action := authz.Plural(it.Kind) + ".update"
		if it.deleted {
			action = authz.Plural(it.Kind) + ".delete"
		}
		dl.rows = append(dl.rows, audit.Row{Action: action, Fields: []string{it.Via}, Resource: d.auditResource(it.Kind, it.ID, it.Name, it.owner)})
	}
	return dl
}
