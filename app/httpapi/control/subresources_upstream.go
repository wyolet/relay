package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/catalogview"
)

type upstreamKeysOut struct {
	Body struct {
		Owner catalogview.UpstreamRef   `json:"owner" doc:"The pool or account the keys belong to."`
		Keys  []catalogview.HostKeyView `json:"keys" doc:"The keys, secret-free; for a pool, in pool order."`
	}
}

func registerUpstreamSubresources(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "pool_keys", Method: "GET", Path: "/pools/{ref}/keys",
		Summary:     "List the pool's keys in pool order (secret-free)",
		Description: "The pool's host keys in the order prioritized selection tries them, without secrets: each of accountIds' keys in creation order, then credentialIds, a key reached twice at its first place. A shared account's keys are its real account's, marked readOnly; other keys the caller may not see are left out. {ref} is a slug or id.",
		Tags:        []string{"pools"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*upstreamKeysOut, error) {
		p, rows, err := viewService(ctx, d).PoolKeys(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "pool not found")
		}
		out := &upstreamKeysOut{}
		out.Body.Owner, out.Body.Keys = p, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "account_keys", Method: "GET", Path: "/accounts/{ref}/keys",
		Summary:     "List the host keys belonging to this account (secret-free)",
		Description: "Host keys whose accountId names the account, without secrets; they share its upstream limits. Keys the caller may not see are left out. For a shared account, the keys of its real account, marked readOnly: whoever sees the shared account sees them, but never their secret, and edits them only on the real account. {ref} is a slug or id.",
		Tags:        []string{"accounts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*upstreamKeysOut, error) {
		a, rows, err := viewService(ctx, d).AccountKeys(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "account not found")
		}
		out := &upstreamKeysOut{}
		out.Body.Owner, out.Body.Keys = a, rows
		return out, nil
	})
	registerAccountShares(api, d, protect)
}
