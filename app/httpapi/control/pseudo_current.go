// What the access and limits kinds answer of the policy a target's account is under now, so a form opens filled: only what the caller may see, the rest counted.

package control

import (
	"context"

	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// PolicyCurrent is the policy a target's account is under, as the caller may see it.
type PolicyCurrent struct {
	Policy       RowRef        `json:"policy"       doc:"The policy."`
	Pools        []CurrentPool `json:"pools"        doc:"The pools it links that the caller may see, in its order."`
	HiddenPools  int           `json:"hiddenPools"  doc:"Pools it links that the caller may not see: counted, not named."`
	Models       []string      `json:"models"       doc:"Its models; empty allows every model its pools serve."`
	LinksNoPools bool          `json:"linksNoPools" doc:"True when it links no pool: the account uses every credential its scope reaches."`
}

// CurrentPool is one pool the policy links.
type CurrentPool struct {
	ID             string   `json:"id"             doc:"Id of the pool."`
	Name           string   `json:"name"           doc:"Slug of the pool."`
	Host           RowRef   `json:"host"           doc:"The pool's host."`
	Accounts       []RowRef `json:"accounts"       doc:"The accounts the pool lists that the caller may see, in its order."`
	HiddenAccounts int      `json:"hiddenAccounts" doc:"Accounts it lists that the caller may not see: counted, not named."`
	Labelled       bool     `json:"labelled"       doc:"True for a pool POST /pseudo-access made for this account (label access-for), which it updates or deletes on a later call; any other pool is only linked or unlinked."`
}

// policyCurrent is p as the caller may see it; nil for no policy. accountID is the target's account, whose labelled pools are flagged.
func (d Deps) policyCurrent(ctx context.Context, p *policy.Policy, accountID string) (*PolicyCurrent, error) {
	if p == nil {
		return nil, nil
	}
	out := &PolicyCurrent{Policy: rowRef(&p.Meta), Pools: []CurrentPool{}, Models: append([]string{}, p.Spec.Models...), LinksNoPools: len(p.Spec.PoolIDs) == 0}
	for _, id := range p.Spec.PoolIDs {
		pl, err := d.Stores.Pool.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if pl == nil {
			continue
		}
		if !visibleTo(ctx, d.Authz, "pool", pl.Meta.ID, pl.Meta.Owner) {
			out.HiddenPools++
			continue
		}
		cp := CurrentPool{ID: pl.Meta.ID, Name: pl.Meta.Name, Host: RowRef{ID: pl.Spec.HostID}, Accounts: []RowRef{}, Labelled: pl.Meta.Labels[pseudo.AccessLabel] == accountID}
		if h, err := d.Stores.Host.Get(ctx, pl.Spec.HostID); err == nil && h != nil {
			cp.Host.Name = h.Meta.Name
		}
		for _, aid := range pl.Spec.AccountIDs {
			a, err := d.Stores.Account.Get(ctx, aid)
			if err != nil {
				return nil, err
			}
			if a == nil || !visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
				cp.HiddenAccounts++
				continue
			}
			cp.Accounts = append(cp.Accounts, rowRef(&a.Meta))
		}
		out.Pools = append(out.Pools, cp)
	}
	return out, nil
}

// policyPrefill reads the case and current policy of target t for a dry run with the change omitted; nothing is written. above answers its refusal.
func (d Deps) policyPrefill(ctx context.Context, t pseudo.Target) (pseudo.PolicyCase, *PolicyCurrent, error) {
	in, err := d.policyReads(ctx, t)
	if err != nil {
		return "", nil, d.pseudoError(ctx, err)
	}
	c := in.Case()
	if c == pseudo.CaseAbove {
		_, err := pseudo.ChangePolicy(in, func(*policy.Spec) {}, "/spec/target")
		return c, nil, policyStepError(err)
	}
	cur, err := d.policyCurrent(ctx, in.Policy, in.Account.Meta.ID)
	return c, cur, err
}
