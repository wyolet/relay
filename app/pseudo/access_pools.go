// What access.accountIds expands to, shared by the agent and access kinds: the shares an owner scope needs and, per host, the accounts a pool lists in the order given.

package pseudo

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
)

// accountSources is what expanding access.accountIds for an owner scope reads.
type accountSources struct {
	accounts  map[string]*account.Account
	hostNames map[string]string
	keyless   map[string]bool
	reaches   func(a *account.Account) bool
	shares    map[string]*account.Account
	ownerName string
	names     *Namer
	newID     func() string
}

// rowAdder appends one step to a kind's rows.
type rowAdder func(kind, pointer string, m *meta.Metadata, v any)

// poolMembers groups accountIDs by host, in the order each host first appears, each host's members in the order given: an account owner reaches, or a share, as it is; a real account owner neither reaches nor holds a share of through a share made first and added through add.
func poolMembers(src accountSources, owner meta.Owner, accountIDs []string, add rowAdder) ([]string, map[string][]string, error) {
	var hosts []string
	members := map[string][]string{}
	for i, id := range accountIDs {
		pointer := fmt.Sprintf("/spec/access/accountIds/%d", i)
		a := src.accounts[id]
		if a == nil {
			return nil, nil, &BodyError{Pointer: pointer, Message: fmt.Sprintf("account %q not found", id)}
		}
		if src.keyless[a.Spec.HostID] {
			return nil, nil, &BodyError{Pointer: pointer, Message: fmt.Sprintf("account %q is at host %q, which takes no key, so a pool would ignore it; name that host's pool in poolIds instead", a.Meta.Name, src.hostNames[a.Spec.HostID])}
		}
		member := id
		if a.IsProvider() && !a.IsShared() && !src.reaches(a) {
			if s := src.shares[id]; s != nil {
				member = s.Meta.ID
			} else {
				member = shareWith(src, owner, a, pointer, add).Meta.ID
			}
		}
		if _, ok := members[a.Spec.HostID]; !ok {
			hosts = append(hosts, a.Spec.HostID)
		}
		members[a.Spec.HostID] = append(members[a.Spec.HostID], member)
	}
	return hosts, members, nil
}

// shareWith is src shared with the owner scope, added as a step.
func shareWith(in accountSources, owner meta.Owner, src *account.Account, pointer string, add rowAdder) *account.Account {
	s := account.Share(src, account.Receiver{Kind: account.ReceiverKind(owner.Kind), ID: owner.ID}, owner, in.ownerName)
	s.Meta.ID = in.newID()
	s.Meta.Name = in.names.Mint("account", s.Meta.Name, "account")
	add("account", pointer, &s.Meta, s)
	return s
}

// checkAccess refuses access that names its pools twice, not at all, or both with everyCredential.
func checkAccess(a *AgentAccess) error {
	switch {
	case a.EveryCredential && len(a.AccountIDs) > 0:
		return &BodyError{Pointer: "/spec/access/accountIds", Message: "accountIds must be empty when everyCredential is true"}
	case a.EveryCredential && len(a.PoolIDs) > 0:
		return &BodyError{Pointer: "/spec/access/poolIds", Message: "poolIds must be empty when everyCredential is true"}
	case !a.EveryCredential && len(a.AccountIDs) == 0 && len(a.PoolIDs) == 0:
		return &BodyError{Pointer: "/spec/access", Message: "accountIds or poolIds is required unless everyCredential is true"}
	}
	if err := listedOnce("account", "accountIds", a.AccountIDs); err != nil {
		return err
	}
	return listedOnce("pool", "poolIds", a.PoolIDs)
}
