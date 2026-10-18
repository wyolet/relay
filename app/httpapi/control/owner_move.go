package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
)

// refuseStrandingMove returns the 409 for moving kind's row to owner while a
// row referencing it could not reference it from there; stays answers for one
// referencing row. Counted like delete blockers: rows the caller can't see
// still refuse the move, unnamed.
func refuseStrandingMove(ctx context.Context, d Deps, kind, id, name string, owner meta.Owner, stays func(referenceItem) bool) error {
	scan, ok := referenceScans[kind]
	if !ok || d.Stores == nil {
		return nil
	}
	items, err := scan(ctx, d, id)
	if err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	var stranded []referenceItem
	for _, it := range items {
		if !stays(it) {
			stranded = append(stranded, it)
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	return &resourceInUseError{
		APIError: httpapi.APIError{
			Err: httpapi.APIErrorBody{
				Type:    "invalid_request_error",
				Code:    "resource_in_use",
				Message: fmt.Sprintf("%s %q cannot move to owner %s: %d referencing row(s) could not reference it from there; see blockers", kind, name, owner.Kind, len(stranded)),
			},
			HTTPStatus: http.StatusConflict,
		},
		Blockers: groupBlockers(ctx, d.Authz, stranded),
	}
}

// refusePolicyMove keeps keys, service accounts and policy bindings on a
// policy they may still reference. Host keys and hosts take only their own
// host's tier policy, so any move strands them.
func refusePolicyMove(ctx context.Context, d Deps, moved *policy.Policy) error {
	c := refs(d)
	c.Rows.Policy = func(context.Context, string) *policy.Policy { return moved }
	return refuseStrandingMove(ctx, d, "policy", moved.Meta.ID, moved.Meta.Name, moved.Meta.Owner, func(it referenceItem) bool {
		switch it.Kind {
		case "key", "service-account", "policy-binding":
			return c.PolicyRef(ctx, moved.Meta.ID, it.owner) == nil
		}
		return false
	})
}

func refuseRateLimitMove(ctx context.Context, d Deps, moved *ratelimit.RateLimit) error {
	c := refs(d)
	c.Rows.RateLimit = func(context.Context, string) *ratelimit.RateLimit { return moved }
	return refuseStrandingMove(ctx, d, "rate-limit", moved.Meta.ID, moved.Meta.Name, moved.Meta.Owner, func(it referenceItem) bool {
		return it.Kind == "policy" && c.RateLimitRef(ctx, moved.Meta.ID, it.owner) == nil
	})
}

// refuseHostKeyMove keeps policies on a host key they may still spend. A
// settings section names the stored secret by id, whoever owns the key.
func refuseHostKeyMove(ctx context.Context, d Deps, moved *hostkey.HostKey) error {
	c := refs(d)
	c.Rows.HostKey = func(context.Context, string) *hostkey.HostKey { return moved }
	return refuseStrandingMove(ctx, d, "host-key", moved.Meta.ID, moved.Meta.Name, moved.Meta.Owner, func(it referenceItem) bool {
		switch it.Kind {
		case "policy":
			return c.HostKeyRefs(ctx, []string{moved.Meta.ID}, it.owner) == nil
		case "settings":
			return true
		}
		return false
	})
}

// refuseServiceAccountMove refuses moving an account anything references: its
// keys take their owner from the account's project, and its bindings name it
// within the old project.
func refuseServiceAccountMove(ctx context.Context, d Deps, moved *serviceaccount.ServiceAccount) error {
	return refuseStrandingMove(ctx, d, "service-account", moved.Meta.ID, moved.Meta.Name, moved.Meta.Owner, func(referenceItem) bool { return false })
}

// guardRateLimitMove refuses an update that moves a rate limit away from the
// policies using it.
func guardRateLimitMove(d Deps) mutationGuard[ratelimit.RateLimit] {
	return func(ctx context.Context, action string, existing, incoming *ratelimit.RateLimit) error {
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		return refuseRateLimitMove(ctx, d, incoming)
	}
}
