// What access.accountIds reads, for the agent and access kinds alike.

package control

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/meta"
)

// accountReads is what access.accountIds reads: the accounts, their hosts' names and which take no key, the owner scope's shares by real account, and the accounts a pool of them draws on.
type accountReads struct {
	accounts  map[string]*account.Account
	hostNames map[string]string
	keyless   map[string]bool
	shares    map[string]*account.Account
	upstream  []string
}

// readAccessAccounts reads accountIDs for a pool of owner, refusing one the caller may not see with its pointer.
func (d Deps) readAccessAccounts(ctx context.Context, accountIDs []string, owner meta.Owner) (accountReads, error) {
	out := accountReads{accounts: map[string]*account.Account{}, hostNames: map[string]string{}, keyless: map[string]bool{}, shares: map[string]*account.Account{}}
	if len(accountIDs) == 0 {
		return out, nil
	}
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return out, err
	}
	receiver := account.Receiver{Kind: account.ReceiverKind(owner.Kind), ID: owner.ID}
	sees := sharerAuthz{Authorizer: d.Authz, d: d}
	for _, s := range all {
		// A share the caller may not see is not used: whether the owner holds one must not show in the answer.
		if s.IsShared() && s.Meta.Owner == owner && s.Receiver() == receiver && sees.Visible(ctx, "account", s.Meta.ID, s.Meta.Owner) {
			out.shares[s.Spec.SourceAccountID] = s
		}
	}
	for i, id := range accountIDs {
		acc, err := d.findAccount(ctx, id)
		if err != nil {
			return out, refusal(err, fmt.Sprintf("/spec/access/accountIds/%d", i))
		}
		out.accounts[id] = acc
		out.upstream = append(out.upstream, id)
		if s := out.shares[id]; s != nil {
			out.upstream = append(out.upstream, s.Meta.ID)
		}
		if h, err := d.Stores.Host.Get(ctx, acc.Spec.HostID); err == nil && h != nil {
			pseudo.Record(ctx, h.Meta.ID, h)
			out.hostNames[h.Meta.ID] = h.Meta.Name
			out.keyless[h.Meta.ID] = h.Spec.NoAuth
		}
	}
	return out, nil
}
