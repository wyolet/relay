package control

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/engine"
)

type reachableOut struct {
	Body Reachable
}

type draftReachInput struct {
	Owner string   `query:"owner" required:"true" pattern:"^(system|(team|project|user):[^:]+)$" doc:"The scope the draft policy is owned by: system, team:<id>, project:<id> or user:<id>. The caller must be allowed to create a policy there."`
	Pool  []string `query:"pool,explode" doc:"Pool ids the draft links, in failover order (repeatable). Omit to read what a policy linking no pools reaches by its scope's ownership. Pools the caller may not read are left out."`
}

const reachDoc = " Worked out by the same fold and route fill a request runs, on the plans loaded now: models the lane reaches with their hosts, the pools and accounts that bring them and each upstream tier's verdict, and the models at its hosts it may not use with the rung that refuses them. Rows the caller may not read are left out."

func registerReachable(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "account_reachable", Method: http.MethodGet, Path: "/accounts/{ref}/reachable",
		Summary:     "Read what an account's lane may use",
		Description: "What a request in the account may use, under every rung above it and the upstream tiers. A team's or project's own account is read as a lane directly under it. Readable by whoever may read the account and by the user who acts in it. {ref} is a slug or id." + reachDoc,
		Tags:        []string{"accounts"}, Middlewares: protect, Errors: []int{401, 404, 422, 503},
	}, func(ctx context.Context, in *refInput) (*reachableOut, error) {
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		acc := byRef[account.Account](v, in.Ref)
		if acc == nil || !mayReadAccount(ctx, d.Authz, acc, in.Ref) {
			return nil, huma.Error404NotFound("account " + strconv.Quote(in.Ref) + " not found")
		}
		r := plan.ReachOf(v, acc.Meta.ID, nil)
		if r == nil {
			return nil, huma.Error422UnprocessableEntity("account " + acc.Meta.Name + " heads no ladder: its holder or scope is gone")
		}
		return &reachableOut{Body: reachView{ctx, d.Authz, v}.project(r)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "catalog_reachable", Method: http.MethodGet, Path: "/catalog/reachable",
		Summary:     "Read what a draft policy would reach",
		Description: "What a policy owned by owner and linking the given pools would reach, read in the owner's own account (a team's or project's account, a user's personal account) with the draft as its policy; a system-owned draft sits alone above system-owned keys. Outer rungs narrow it as at request time: a project-owned draft sits under its team's policy. The draft's model list is not read; every model the reach serves is listed." + reachDoc,
		Tags:        []string{"catalog"}, Middlewares: protect, Errors: []int{401, 403, 404, 503},
	}, func(ctx context.Context, in *draftReachInput) (*reachableOut, error) {
		owner := parseOwner(in.Owner)
		if err := d.Authz.Authorize(ctx, "policies.create", authz.Resource{Kind: "policy", Owner: &owner}); err != nil {
			return nil, huma.Error403Forbidden("you may not create a policy owned by " + in.Owner)
		}
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		draft := &policy.Policy{Meta: meta.Metadata{Owner: owner}}
		for _, id := range in.Pool {
			if p := engine.Get[pool.Pool](v, id); p != nil && visibleTo(ctx, d.Authz, "pool", id, p.Meta.Owner) {
				draft.Spec.PoolIDs = append(draft.Spec.PoolIDs, id)
			}
		}
		return reachInOwner(ctx, d, v, draft)
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_reachable", Method: http.MethodGet, Path: "/policies/{ref}/reachable",
		Summary:     "Read what a saved policy reaches",
		Description: "The policy read in its owner's own account as GET /catalog/reachable reads a draft, its model list and budgets included, with the accounts bound to it in boundAccounts; GET /accounts/{ref}/reachable reads one of them under its own ladder. {ref} is a slug or id." + reachDoc,
		Tags:        []string{"policies"}, Middlewares: protect, Errors: []int{401, 404, 422, 503},
	}, func(ctx context.Context, in *refInput) (*reachableOut, error) {
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		p := byRef[policy.Policy](v, in.Ref)
		if p == nil || !visibleTo(ctx, d.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
			return nil, huma.Error404NotFound("policy " + strconv.Quote(in.Ref) + " not found")
		}
		if p.Meta.Owner.Kind == meta.OwnerHost {
			return nil, huma.Error422UnprocessableEntity("policy " + p.Meta.Name + " is a host tier, bound to provider accounts; read their reach instead")
		}
		out, err := reachInOwner(ctx, d, v, p)
		if err != nil {
			return nil, err
		}
		out.Body.BoundAccounts = boundAccounts(ctx, d, v, p.Meta.ID)
		return out, nil
	})
}

// reachInOwner reads p's reach in its owner's own account with p as that account's policy.
func reachInOwner(ctx context.Context, d Deps, v *engine.View, p *policy.Policy) (*reachableOut, error) {
	acc, ok := plan.OwnerAccount(v, p.Meta.Owner)
	if !ok {
		return nil, huma.Error404NotFound("no account for " + ownerKeyOf(p.Meta.Owner) + " yet")
	}
	over := &plan.Override{Policy: p}
	if acc != nil {
		over.AccountID = acc.Meta.ID
	}
	r := plan.ReachOf(v, over.AccountID, over)
	if r == nil {
		return nil, huma.Error404NotFound("no account for " + ownerKeyOf(p.Meta.Owner) + " yet")
	}
	return &reachableOut{Body: reachView{ctx, d.Authz, v}.project(r)}, nil
}

// boundAccounts lists the accounts enabled bindings of the policy name, those the caller may read.
func boundAccounts(ctx context.Context, d Deps, v *engine.View, policyID string) []ReachRef {
	out := []ReachRef{}
	rv := reachView{ctx, d.Authz, v}
	for b := range engine.All[policybinding.PolicyBinding](v) {
		if b.Spec.PolicyID != policyID || !b.IsEnabled() {
			continue
		}
		for _, id := range b.Spec.AccountIDs {
			if ref := rv.accountRef(engine.Get[account.Account](v, id)); ref != nil {
				out = append(out, *ref)
			}
		}
	}
	return out
}

func parseOwner(s string) meta.Owner {
	kind, id, _ := strings.Cut(s, ":")
	return meta.Owner{Kind: meta.OwnerKind(kind), ID: id}
}

func ownerKeyOf(o meta.Owner) string {
	if o.ID == "" {
		return string(o.Kind)
	}
	return string(o.Kind) + ":" + o.ID
}
