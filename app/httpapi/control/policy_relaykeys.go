// Attach / detach RelayKey ↔ Policy from the policy side. The
// authoritative field is RelayKey.Spec.PolicyID (1:N — one policy per
// key, many keys per policy); these endpoints mutate it so the policy
// form can manage its key roster without round-tripping through the
// relay-key form.
//
//	POST   /policies/by-id/{id}/relay-keys/{relayKeyId}   — attach
//	DELETE /policies/by-id/{id}/relay-keys/{relayKeyId}   — detach
//
// Attach overwrites any existing PolicyID on the relay key (no
// confirmation): moving a key from policy A to policy B is the
// common case. Detach succeeds when the key currently points at this
// policy; mismatched detach returns 409 to surface the drift instead
// of silently no-op'ing.
package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/relaykey"
)

type policyRelayKeyInput struct {
	PolicyID   string `path:"id"          doc:"Policy id (UUIDv7)."`
	RelayKeyID string `path:"relayKeyId"  doc:"RelayKey id (UUIDv7)."`
}

type policyRelayKeyResponse struct {
	Body *relaykey.RelayKey `json:"body"`
}

func registerPolicyRelayKeys(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "attach_relay_key_to_policy",
		Method:      http.MethodPost,
		Path:        "/policies/by-id/{id}/relay-keys/{relayKeyId}",
		Summary:     "Attach a RelayKey to this Policy",
		Description: "Sets RelayKey.Spec.PolicyID to this policy. " +
			"Overwrites any prior attachment (moving a key from A to B " +
			"is the common case).",
		Tags:        []string{"policies"},
		Middlewares: protect,
		Errors:      []int{401, 404, 500},
	}, func(ctx context.Context, in *policyRelayKeyInput) (*policyRelayKeyResponse, error) {
		if err := d.Authz.Authorize(ctx, "relay-keys.update", authz.Resource{Kind: "relay-key", ID: in.RelayKeyID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if _, err := d.Stores.Policy.Get(ctx, in.PolicyID); err != nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("policy %q not found", in.PolicyID))
		}
		rk, err := d.Stores.RelayKey.Get(ctx, in.RelayKeyID)
		if err != nil || rk == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("relay-key %q not found", in.RelayKeyID))
		}
		rk.Spec.PolicyID = in.PolicyID
		if err := d.Stores.RelayKey.Upsert(ctx, rk); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		updated, err := d.Stores.RelayKey.Get(ctx, in.RelayKeyID)
		if err != nil {
			return nil, huma.Error500InternalServerError("attached but could not read back: " + err.Error())
		}
		return &policyRelayKeyResponse{Body: updated}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "detach_relay_key_from_policy",
		Method:      http.MethodDelete,
		Path:        "/policies/by-id/{id}/relay-keys/{relayKeyId}",
		Summary:     "Detach a RelayKey from this Policy",
		Description: "Clears RelayKey.Spec.PolicyID. Returns 409 if the " +
			"key currently points at a different policy (the caller is " +
			"acting on stale state).",
		Tags:        []string{"policies"},
		Middlewares: protect,
		Errors:      []int{401, 404, 409, 500},
	}, func(ctx context.Context, in *policyRelayKeyInput) (*policyRelayKeyResponse, error) {
		if err := d.Authz.Authorize(ctx, "relay-keys.update", authz.Resource{Kind: "relay-key", ID: in.RelayKeyID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		rk, err := d.Stores.RelayKey.Get(ctx, in.RelayKeyID)
		if err != nil || rk == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("relay-key %q not found", in.RelayKeyID))
		}
		if rk.Spec.PolicyID != in.PolicyID {
			return nil, huma.Error409Conflict(fmt.Sprintf(
				"relay-key %q is attached to policy %q, not %q",
				rk.Meta.Name, rk.Spec.PolicyID, in.PolicyID))
		}
		rk.Spec.PolicyID = ""
		if err := d.Stores.RelayKey.Upsert(ctx, rk); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		updated, err := d.Stores.RelayKey.Get(ctx, in.RelayKeyID)
		if err != nil {
			return nil, huma.Error500InternalServerError("detached but could not read back: " + err.Error())
		}
		return &policyRelayKeyResponse{Body: updated}, nil
	})
}
