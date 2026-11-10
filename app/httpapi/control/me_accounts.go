package control

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// MyAccount is one account the signed-in user acts in or owns.
type MyAccount struct {
	Account   AccountSummary `json:"account"   doc:"The account."`
	Kind      account.Kind   `json:"kind"      enum:"personal,member,provider" doc:"personal is the user's own account; member is the user's account in a team or project; provider is a provider account at a host the user owns personally."`
	Scope     AccountScope   `json:"scope"     doc:"Where the account sits: the user for a personal or provider account, the team or project for a member account."`
	Policy    *BoundPolicy   `json:"policy,omitempty" doc:"The policy bound to the account. Omitted when no policy binding names it, or when two do (the account then refuses requests until one remains)."`
	Keys      []AccountKey   `json:"keys"      doc:"The user's relay keys whose default lane is this account, revoked keys left out."`
	TokenOnly bool           `json:"tokenOnly" doc:"True for a member account opened on first use under a group or team-level grant relay cannot list: it serves relay-minted tokens only, and keys cannot be created in it."`
}

// AccountSummary names an account.
type AccountSummary struct {
	ID          string `json:"id"          doc:"Account id."`
	Name        string `json:"name"        doc:"Account slug."`
	DisplayName string `json:"displayName" doc:"Account display name."`
	Closed      bool   `json:"closed"      doc:"The membership the account stood for has ended: its keys are refused with account_closed."`
}

// AccountScope is the user, team or project an account sits in.
type AccountScope struct {
	Kind meta.OwnerKind `json:"kind" enum:"user,team,project" doc:"Kind of the scope."`
	ID   string         `json:"id"   doc:"Id of the user, team or project."`
	Name string         `json:"name" doc:"Username of the user, or slug of the team or project; empty when the row is gone."`
}

// BoundPolicy names the policy bound to an account.
type BoundPolicy struct {
	ID   string `json:"id"   doc:"Policy id."`
	Name string `json:"name" doc:"Policy slug."`
}

// AccountKey names a relay key without its secret.
type AccountKey struct {
	ID     string `json:"id"     doc:"Key id."`
	Name   string `json:"name"   doc:"Key slug."`
	Prefix string `json:"prefix" doc:"Leading characters of the key's plaintext, for recognising it."`
}

type myAccountsOut struct {
	Body []MyAccount
}

func registerMyAccounts(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_my_accounts",
		Method:      http.MethodGet,
		Path:        "/me/accounts",
		Summary:     "List the signed-in user's accounts",
		Description: "The accounts the signed-in user can run requests in: the personal account, a member account per team or project membership (closed and token-only ones included and flagged), and provider accounts the user owns personally. Each row carries the bound policy and the user's keys that run in it by default. Service accounts' accounts are not listed. The admin token is no user and gets an empty list. Read from the published plans, so a write shows within about a second.",
		Tags:        []string{"accounts"},
		Middlewares: protect,
		Errors:      []int{401, 403, 503},
	}, func(ctx context.Context, _ *struct{}) (*myAccountsOut, error) {
		if err := d.Authz.Authorize(ctx, "accounts.list", authz.Resource{Kind: "account"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		a := actor.From(ctx)
		if a == nil || a.UserID == "" {
			return &myAccountsOut{Body: []MyAccount{}}, nil
		}
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		return &myAccountsOut{Body: myAccounts(v, a.UserID)}, nil
	})
}

// planView is the published plan view, or 503 before the first publish.
func planView(d Deps) (*engine.View, error) {
	if d.PlanView != nil {
		if v := d.PlanView(); v != nil {
			return v, nil
		}
	}
	return nil, huma.Error503ServiceUnavailable("plans are not loaded yet")
}

// myAccounts lists userID's accounts in v: personal first, then member accounts and provider accounts, each by name.
func myAccounts(v *engine.View, userID string) []MyAccount {
	mine := meta.Owner{Kind: meta.OwnerUser, ID: userID}
	keys := map[string][]AccountKey{}
	for k := range engine.All[key.Key](v) {
		if k.Spec.Principal.Kind != key.PrincipalUser || k.Spec.Principal.ID != userID || k.Spec.RevokedAt != nil {
			continue
		}
		if lane := plan.KeyLane(v, k); lane != "" {
			keys[lane] = append(keys[lane], AccountKey{ID: k.Meta.ID, Name: k.Meta.Name, Prefix: k.Spec.Prefix})
		}
	}
	out := []MyAccount{}
	for a := range engine.All[account.Account](v) {
		kind := a.DerivedKind()
		switch kind {
		case account.KindPersonal, account.KindMember:
			if a.Spec.Holder.ID != userID {
				continue
			}
		case account.KindProvider:
			if a.Meta.Owner != mine {
				continue
			}
		default:
			continue
		}
		row := MyAccount{
			Account:   AccountSummary{ID: a.Meta.ID, Name: a.Meta.Name, DisplayName: a.Meta.DisplayName, Closed: a.Spec.Closed},
			Kind:      kind,
			Scope:     accountScope(v, a.Meta.Owner),
			Keys:      keys[a.Meta.ID],
			TokenOnly: kind == account.KindMember && !a.FollowsRoleBindings,
		}
		if row.Keys == nil {
			row.Keys = []AccountKey{}
		}
		if p := plan.AccountPolicy(v, a.Meta.ID); p != nil {
			row.Policy = &BoundPolicy{ID: p.Meta.ID, Name: p.Meta.Name}
		}
		out = append(out, row)
	}
	rank := map[account.Kind]int{account.KindPersonal: 0, account.KindMember: 1, account.KindProvider: 2}
	slices.SortStableFunc(out, func(a, b MyAccount) int {
		return cmp.Or(cmp.Compare(rank[a.Kind], rank[b.Kind]), cmp.Compare(a.Account.Name, b.Account.Name))
	})
	return out
}

// accountScope names the user, team or project o.
func accountScope(v *engine.View, o meta.Owner) AccountScope {
	s := AccountScope{Kind: o.Kind, ID: o.ID}
	switch o.Kind {
	case meta.OwnerUser:
		if u := engine.Get[user.User](v, o.ID); u != nil {
			s.Name = u.Username
		}
	case meta.OwnerTeam:
		if t := engine.Get[team.Team](v, o.ID); t != nil {
			s.Name = t.Meta.Name
		}
	case meta.OwnerProject:
		if p := engine.Get[project.Project](v, o.ID); p != nil {
			s.Name = p.Meta.Name
		}
	}
	return s
}
