// What expanding a pseudo access body reads: the policy step's reads, then (unless the step is refused) the pools and accounts access names, the target account's labelled pools with the policies linking them, and, for the plan check, what all of those draw on.

package control

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
)

// accessReads reads what expanding body needs; poolNames are the names of the pools read, by id, for the answer.
func (d Deps) accessReads(ctx context.Context, body pseudo.Access) (in pseudo.AccessReads, poolNames map[string]string, err error) {
	if in.Policy, err = d.policyReads(ctx, body.Spec.Target); err != nil || in.Policy.Case() == pseudo.CaseAbove {
		return in, nil, err
	}
	acc := in.Policy.Account
	owner := acc.Meta.Owner
	c := refs(d)
	in.Reaches = func(a *account.Account) bool { return c.Reaches(ctx, a.Meta.Owner, owner) }
	in.OwnerName = d.ownerName(ctx, owner)
	poolNames = map[string]string{}
	var listed []string
	var upstreamAccounts []string
	if a := body.Spec.Access; a != nil {
		for i, id := range a.PoolIDs {
			if err := c.PoolRefs(ctx, []string{id}, owner); err != nil {
				return in, nil, refusal(refErr(err), fmt.Sprintf("/spec/access/poolIds/%d", i))
			}
		}
		listed = a.PoolIDs
		var ar accountReads
		if ar, err = d.readAccessAccounts(ctx, a.AccountIDs, owner); err != nil {
			return in, nil, err
		}
		in.Accounts, in.HostNames, in.KeylessHosts, in.Shares, upstreamAccounts = ar.accounts, ar.hostNames, ar.keyless, ar.shares, ar.upstream
	}
	if in.Labelled, in.LinkedBy, err = d.labelledPools(ctx, acc.Meta.ID); err != nil {
		return in, nil, err
	}
	drawn := slices.Clone(listed)
	if p := in.Policy.Policy; p != nil {
		drawn = append(drawn, p.Spec.PoolIDs...)
	}
	for _, p := range in.Labelled {
		poolNames[p.Meta.ID] = p.Meta.Name
	}
	var upstreamKeys []string
	for _, id := range drawn {
		p, err := d.Stores.Pool.Get(ctx, id)
		if err != nil {
			return in, nil, err
		}
		if p != nil {
			pseudo.Record(ctx, id, p)
			poolNames[id] = p.Meta.Name
			upstreamAccounts = append(upstreamAccounts, p.Spec.AccountIDs...)
			upstreamKeys = append(upstreamKeys, p.Spec.CredentialIDs...)
		}
	}
	return in, poolNames, d.recordUpstream(ctx, upstreamAccounts, upstreamKeys)
}

// labelledPools are the pools labelled pseudo.AccessLabel=accountID the caller may see, and the ids of every policy linking each, seen or not, by pool id.
func (d Deps) labelledPools(ctx context.Context, accountID string) ([]*pool.Pool, map[string][]string, error) {
	pools, err := d.Stores.Pool.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	var labelled []*pool.Pool
	ids := map[string]bool{}
	for _, p := range pools {
		if p.Meta.Labels[pseudo.AccessLabel] == accountID && visibleTo(ctx, d.Authz, "pool", p.Meta.ID, p.Meta.Owner) {
			pseudo.Record(ctx, p.Meta.ID, p)
			labelled = append(labelled, p)
			ids[p.Meta.ID] = true
		}
	}
	linkedBy := map[string][]string{}
	if len(labelled) == 0 {
		return nil, linkedBy, nil
	}
	policies, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range policies {
		for _, id := range p.Spec.PoolIDs {
			if ids[id] {
				linkedBy[id] = append(linkedBy[id], p.Meta.ID)
			}
		}
	}
	return labelled, linkedBy, nil
}
