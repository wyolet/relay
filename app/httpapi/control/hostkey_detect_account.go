package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/catalogkind/host"
)

// maxSiblingProbes bounds the upstream calls one detect makes for the other keys of the account before filling its orgId; an account with more keys is left for the operator to fill.
const maxSiblingProbes = 8

type detectAccountInput struct {
	ID string `path:"id" doc:"HostKey id (UUIDv7)."`
}

type detectedAccount struct {
	OrgID             string `json:"orgId" doc:"The organization id the host named for the key."`
	OrgName           string `json:"orgName,omitempty" doc:"The organization's display name, when the host sends one."`
	CurrentAccountID  string `json:"currentAccountId" doc:"The account the key is in now."`
	MatchingAccountID string `json:"matchingAccountId,omitempty" doc:"An account of the key's owner at the same host whose orgId is the detected one: the current account when the key is already there. Absent when there is none; the key is never moved by this call."`
}

type detectAccountResponse struct {
	Body detectedAccount
}

// POST /host-keys/by-id/{id}/detect-account asks the key's host which organization the key belongs to and names the owner's account for it. It moves nothing: the client moves the key with a normal update. The one write is filling orgId on the key's current account when that account has none and none of the owner's accounts at the host claims the organization.
func registerHostKeyDetectAccount(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "detect_host_key_account",
		Method:      http.MethodPost,
		Path:        "/host-keys/by-id/{id}/detect-account",
		Summary:     "Detect which provider organization a HostKey belongs to",
		Description: "Makes one cheap read to the key's host with the key and returns the organization id from the response headers, the key's current account, and the account of the key's owner at the host with that orgId. It never moves the key; set spec.accountId with a normal update to move it. When the current account has no orgId and no other account claims the organization, the current account's orgId is filled in, provided every other key of that account names the same organization. 422 when the host does not name organizations or the key has no value; 502 when the host refuses the call.",
		Tags:        []string{"host-keys"},
		Middlewares: protect,
		Errors:      []int{401, 403, 404, 422, 500, 502},
	}, func(ctx context.Context, in *detectAccountInput) (*detectAccountResponse, error) {
		k, err := d.Stores.HostKey.Get(ctx, in.ID)
		if err != nil || k == nil || !visibleTo(ctx, d.Authz, "host-key", k.Meta.ID, k.Meta.Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("host-key %q not found", in.ID))
		}
		if err := d.Authz.Authorize(ctx, "host-keys.update", authz.Resource{Kind: "host-key", ID: in.ID, Owner: &k.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		h, spec, reason := probeTarget(d.Catalog.Current(), d.Adapters, k.Spec.HostID)
		if spec == nil {
			return nil, huma.Error422UnprocessableEntity(reason)
		}
		if k.Resolved == "" {
			return nil, huma.Error422UnprocessableEntity("the key's value cannot be resolved")
		}
		check := probeValue(ctx, spec, h, k.Resolved, k.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth)
		if check.Outcome != keyDetected {
			return nil, check.detectError()
		}
		org := check.Org

		accounts, err := ownerAccountsAt(ctx, d, k)
		if err != nil {
			return nil, huma.Error500InternalServerError("accounts: " + err.Error())
		}
		var current *account.Account
		for _, a := range accounts {
			if a.Meta.ID == k.Spec.AccountID {
				current = a
			}
		}
		matching := accountWithOrg(accounts, current, org.ID)
		if matching == nil && current != nil && current.Spec.OrgID == "" && d.siblingsAgree(ctx, spec, h, k, current.Meta.ID, org.ID) && d.fillOrgID(ctx, current, org.ID) {
			matching = current
		}
		return &detectAccountResponse{Body: detectedAccount{
			OrgID: org.ID, OrgName: org.Name,
			CurrentAccountID: k.Spec.AccountID, MatchingAccountID: idOf(matching),
		}}, nil
	})
}

// ownerAccountsAt lists the accounts of k's owner at k's host that the caller can see.
func ownerAccountsAt(ctx context.Context, d Deps, k *hostkey.HostKey) ([]*account.Account, error) {
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*account.Account
	for _, a := range all {
		if a.Meta.Owner == k.Meta.Owner && a.Spec.HostID == k.Spec.HostID && visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			out = append(out, a)
		}
	}
	return out, nil
}

// accountWithOrg is the account holding orgID, the current one first.
func accountWithOrg(accounts []*account.Account, current *account.Account, orgID string) *account.Account {
	if current != nil && current.Spec.OrgID == orgID {
		return current
	}
	for _, a := range accounts {
		if a.Spec.OrgID == orgID {
			return a
		}
	}
	return nil
}

// siblingsAgree reports whether every other key of the account names orgID when asked; a key without a value, or more keys than maxSiblingProbes, cannot be confirmed.
func (d Deps) siblingsAgree(ctx context.Context, spec *adapter.Spec, h *host.Host, k *hostkey.HostKey, accountID, orgID string) bool {
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return false
	}
	probes := 0
	for _, o := range keys {
		if o.Meta.ID == k.Meta.ID || o.Spec.AccountID != accountID {
			continue
		}
		if probes++; probes > maxSiblingProbes || o.Resolved == "" {
			return false
		}
		org, err := spec.ProbeOrg(ctx, h.Spec.BaseURL, o.Resolved, o.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth)
		if err != nil || org.ID != orgID {
			return false
		}
	}
	return true
}

// fillOrgID writes orgID onto a when the caller may edit it, conditional on the version read. It reports whether the write landed.
func (d Deps) fillOrgID(ctx context.Context, a *account.Account, orgID string) bool {
	if d.Authz.Authorize(ctx, "accounts.update", authz.Resource{Kind: "account", ID: a.Meta.ID, Owner: &a.Meta.Owner}) != nil {
		return false
	}
	if settings.Governs(d.Catalog, settings.OpEdit, "account", string(a.Meta.GovernedOwnerKind()), authz.IsAdmin(ctx)) != nil {
		return false
	}
	filled := *a
	filled.Spec.OrgID = orgID
	if err := d.Stores.Account.Upsert(ctx, &filled); err != nil {
		return false
	}
	audit.Changed(ctx, []string{"account.spec.orgId"})
	return true
}

func idOf(a *account.Account) string {
	if a == nil {
		return ""
	}
	return a.Meta.ID
}
