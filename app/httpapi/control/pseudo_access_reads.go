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
	policyID := ""
	if p := in.Policy.Policy; p != nil {
		policyID = p.Meta.ID
	}
	if in.Labelled, in.LinkedBy, in.LabelledDeletable, err = d.labelledPools(ctx, acc, policyID); err != nil {
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
		if p == nil {
			continue
		}
		pseudo.Record(ctx, id, p)
		if !visibleTo(ctx, d.Authz, "pool", p.Meta.ID, p.Meta.Owner) {
			if in.HiddenPools == nil {
				in.HiddenPools = map[string]bool{}
			}
			in.HiddenPools[id] = true
		} else {
			poolNames[id] = p.Meta.Name
		}
		upstreamAccounts = append(upstreamAccounts, p.Spec.AccountIDs...)
		upstreamKeys = append(upstreamKeys, p.Spec.CredentialIDs...)
	}
	return in, poolNames, d.recordUpstream(ctx, upstreamAccounts, upstreamKeys)
}

// labelledPools are the pools the access kind made for acc: labelled pseudo.AccessLabel=<acc's id>, owned by acc's scope, seen by the caller and linked by no policy but policyID (the one acc is under, "" for none), with the ids of every policy linking each, seen or not, by pool id, and those the caller may delete. A label anyone who makes a pool can write, so a labelled pool that fails any of these is not the kind's and is left alone.
func (d Deps) labelledPools(ctx context.Context, acc *account.Account, policyID string) ([]*pool.Pool, map[string][]string, map[string]bool, error) {
	pools, err := d.Stores.Pool.List(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	candidates := map[string]*pool.Pool{}
	for _, p := range pools {
		if p.Meta.Labels[pseudo.AccessLabel] == acc.Meta.ID && p.Meta.Owner == acc.Meta.Owner && visibleTo(ctx, d.Authz, "pool", p.Meta.ID, p.Meta.Owner) {
			candidates[p.Meta.ID] = p
		}
	}
	linkedBy := map[string][]string{}
	if len(candidates) == 0 {
		return nil, linkedBy, nil, nil
	}
	policies, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, p := range policies {
		for _, id := range p.Spec.PoolIDs {
			if candidates[id] != nil {
				linkedBy[id] = append(linkedBy[id], p.Meta.ID)
			}
		}
	}
	pd := d
	pd.Authz = unaudited(d.Authz)
	checks := poolChecks(pd)
	var labelled []*pool.Pool
	deletable := map[string]bool{}
	for _, p := range pools {
		if candidates[p.Meta.ID] == nil || slices.ContainsFunc(linkedBy[p.Meta.ID], func(id string) bool { return id != policyID }) {
			continue
		}
		pseudo.Record(ctx, p.Meta.ID, p)
		labelled = append(labelled, p)
		if deletable[p.Meta.ID], err = probeAllowed(mayDeleteRow(ctx, checks, &p.Meta, p)); err != nil {
			return nil, nil, nil, err
		}
	}
	return labelled, linkedBy, deletable, nil
}
