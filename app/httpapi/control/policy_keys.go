// Attach overwrites a key's existing PolicyID without confirmation because
// moving a key between policies is the common case. A detach naming a policy
// the key does not point at returns 409 rather than silently doing nothing.

package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
)

type policyKeyInput struct {
	PolicyID string `path:"id"          doc:"Policy id (UUIDv7)."`
	KeyID    string `path:"keyId"  doc:"Key id (UUIDv7)."`
}

type policyKeyResponse struct {
	Body *key.Key `json:"body"`
}

// writeKeyPolicy persists a policy attachment through the same checks a PUT
// on the key runs: the ref guard (a key must not be pointed at a policy the
// caller may not spend), the row's own Validate, and the hand-edit flag apply
// reads to know the row is no longer its own.
func writeKeyPolicy(ctx context.Context, d Deps, rk *key.Key) error {
	if err := guardKeyPolicy(d)(ctx, "update", rk, rk); err != nil {
		return mapGuardErr(err)
	}
	if err := rk.Validate(); err != nil {
		return huma.Error400BadRequest(err.Error())
	}
	rk.Meta.Dirty = true
	if err := d.Stores.Key.Upsert(ctx, rk); err != nil {
		return mapWriteErr("key", rk.Meta.ID, err)
	}
	return nil
}

func registerPolicyKeys(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "attach_relay_key_to_policy",
		Method:      http.MethodPost,
		Path:        "/policies/by-id/{id}/keys/{keyId}",
		Summary:     "Attach a Key to this Policy",
		Description: "Sets Key.Spec.PolicyID to this policy. " +
			"Overwrites any prior attachment (moving a key from A to B " +
			"is the common case).",
		Tags:        []string{"policies"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 409, 500},
	}, func(ctx context.Context, in *policyKeyInput) (*policyKeyResponse, error) {
		pol, err := d.Stores.Policy.Get(ctx, in.PolicyID)
		if err != nil || pol == nil || !visibleTo(ctx, d.Authz, "policy", pol.Meta.ID, pol.Meta.Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("policy %q not found", in.PolicyID))
		}
		rk, err := d.Stores.Key.Get(ctx, in.KeyID)
		if err != nil || rk == nil || !visibleTo(ctx, d.Authz, "key", rk.Meta.ID, rk.Meta.Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("key %q not found", in.KeyID))
		}
		if err := d.Authz.Authorize(ctx, "policies.attach", authz.Resource{Kind: "key", ID: in.KeyID, Owner: &rk.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		rk.Spec.PolicyID = in.PolicyID
		if err := writeKeyPolicy(ctx, d, rk); err != nil {
			return nil, err
		}
		updated, err := d.Stores.Key.Get(ctx, in.KeyID)
		if err != nil {
			return nil, huma.Error500InternalServerError("attached but could not read back: " + err.Error())
		}
		return &policyKeyResponse{Body: updated}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "detach_relay_key_from_policy",
		Method:      http.MethodDelete,
		Path:        "/policies/by-id/{id}/keys/{keyId}",
		Summary:     "Detach a Key from this Policy",
		Description: "Clears Key.Spec.PolicyID. Returns 409 if the " +
			"key currently points at a different policy (the caller is " +
			"acting on stale state).",
		Tags:        []string{"policies"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 409, 500},
	}, func(ctx context.Context, in *policyKeyInput) (*policyKeyResponse, error) {
		rk, err := d.Stores.Key.Get(ctx, in.KeyID)
		if err != nil || rk == nil || !visibleTo(ctx, d.Authz, "key", rk.Meta.ID, rk.Meta.Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("key %q not found", in.KeyID))
		}
		if err := d.Authz.Authorize(ctx, "policies.detach", authz.Resource{Kind: "key", ID: in.KeyID, Owner: &rk.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if rk.Spec.PolicyID != in.PolicyID {
			return nil, huma.Error409Conflict(fmt.Sprintf(
				"key %q is attached to policy %q, not %q",
				rk.Meta.Name, rk.Spec.PolicyID, in.PolicyID))
		}
		rk.Spec.PolicyID = ""
		if err := writeKeyPolicy(ctx, d, rk); err != nil {
			return nil, err
		}
		updated, err := d.Stores.Key.Get(ctx, in.KeyID)
		if err != nil {
			return nil, huma.Error500InternalServerError("detached but could not read back: " + err.Error())
		}
		return &policyKeyResponse{Body: updated}, nil
	})
}
