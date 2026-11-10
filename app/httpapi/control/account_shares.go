// Sharing a provider account: POST /accounts/{ref}/shares makes an account owned by the receiver that reaches the real account's keys, and GET lists them with what each drew. The owner of the real account (its user alone, or an admin of its team, project or the system) shares; a shared account is revoked through the normal account delete (account_revoke.go), by that owner or by an admin of the receiving scope.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// defaultShareUsageWindow is how far back a share list counts usage when since is omitted.
const defaultShareUsageWindow = "30d"

type createShareInput struct {
	Ref  string `path:"ref" doc:"Slug or id of the provider account to share."`
	Body struct {
		Receiver account.Receiver `json:"receiver" doc:"Who the account is shared with."`
	}
}

type listSharesInput struct {
	Ref   string `path:"ref" doc:"Slug or id of the provider account whose shares to list."`
	Since string `query:"since" doc:"Relative usage window (e.g. \"24h\", \"7d\", \"30d\"), the same grammar as the usage endpoints. Default \"30d\"."`
}

// AccountShare is one account a provider account is shared with, and what it drew.
type AccountShare struct {
	SharedAccount ShareAccountRef    `json:"sharedAccount" doc:"The shared account, owned by the receiver."`
	Receiver      account.ShareParty `json:"receiver"      doc:"Who the account is shared with."`
	Usage         *ShareUsage        `json:"usage,omitempty" doc:"What requests through the shared account drew on the real account over the window; absent when this deployment reads no usage."`
}

// ShareAccountRef names a shared account.
type ShareAccountRef struct {
	ID   string `json:"id"   doc:"Id of the shared account."`
	Name string `json:"name" doc:"Slug of the shared account."`
}

// ShareUsage is the traffic one shared account sent through the real account.
type ShareUsage struct {
	Requests  int64 `json:"requests"  doc:"Requests that reached an upstream through the shared account."`
	CostNanos int64 `json:"costNanos" doc:"Cost of the priced ones, in nano-USD."`
	Unpriced  int64 `json:"unpriced"  doc:"Requests with no cost, left out of costNanos."`
}

// AccountShares lists a provider account's shares.
type AccountShares struct {
	Items []AccountShare `json:"items" doc:"The accounts the provider account is shared with, by name."`
	From  time.Time      `json:"from"  doc:"Start of the usage window."`
	To    time.Time      `json:"to"    doc:"End of the usage window."`
}

type listSharesOutput struct {
	Body AccountShares
}

func registerAccountShares(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID:   "share_account",
		Method:        http.MethodPost,
		Path:          "/accounts/{ref}/shares",
		Summary:       "Share a provider account with a user, team, project or service account",
		Description:   "Creates an account owned by the receiver (a service account's by its project, held by it so only its lane draws on it) that reaches the real account's keys: current ones, keys added later, and their rotations. The shared account holds no keys and never shows a secret; its requests count on the real account's tier, and a policy bound to it only narrows them. It changes no routing by itself: the receiving scope uses it by listing it in one of its pools. Only the real account's owner shares it: its user alone for a personal account (no admin), an admin of its team, project or the system otherwise (403). A receiver that does not exist, or that the caller may not see, answers 404. One share per receiver: a second answers 409 share_exists. A shared account is not shared on: 409 reshare_refused. A receiver that already reaches the account through ownership (a project of the team that owns it, any scope for a system account) answers 409 already_reachable. A user whose personal accounts are turned off answers 403 personal_account_off: share with their team or project instead.",
		Tags:          []string{"accounts"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
		Responses:     map[string]*huma.Response{"409": conflictResponse(api, "share_exists: the account is already shared with this receiver; reshare_refused: the account is itself a share; already_reachable: the receiver reaches the account through ownership already.")},
	}, func(ctx context.Context, in *createShareInput) (*itemResponse[account.Account], error) {
		return d.shareAccount(ctx, in.Ref, in.Body.Receiver)
	})

	huma.Register(api, huma.Operation{
		OperationID: "list_account_shares",
		Method:      http.MethodGet,
		Path:        "/accounts/{ref}/shares",
		Summary:     "List the accounts a provider account is shared with",
		Description: "The real account's shares, each with its receiver and the requests and cost it sent through the real account over the window: usage rows of the real account grouped by via_account_id. For the real account's owner, as sharing is (403 otherwise). Revoke a share with DELETE /accounts/by-id/{id} on the shared account: it always goes through and answers what it changed (policy bindings that drop it or go, relay keys moved to their principal's own account, pools that lose it).",
		Tags:        []string{"accounts"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *listSharesInput) (*listSharesOutput, error) {
		return d.listShares(ctx, in.Ref, in.Since)
	})
}

// findAccount reads the account ref names (id or slug), answering 404 for one the caller may not see.
func (d Deps) findAccount(ctx context.Context, ref string) (*account.Account, error) {
	notFound := huma.Error404NotFound(fmt.Sprintf("account %q not found", ref))
	id := ref
	if !ids.Valid(id) {
		resolved, err := listScanResolver(d.Stores.Account, func(a *account.Account) *meta.Metadata { return &a.Meta })(ref)
		if err != nil {
			return nil, notFound
		}
		id = resolved
	}
	a, err := d.Stores.Account.Get(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if a == nil || !visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
		return nil, notFound
	}
	return a, nil
}

func (d Deps) shareAccount(ctx context.Context, ref string, r account.Receiver) (*itemResponse[account.Account], error) {
	src, err := d.findAccount(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !src.IsProvider() {
		return nil, huma.Error400BadRequest(fmt.Sprintf("account %q is a %s account; only a provider account is shared", src.Meta.Name, src.Spec.Kind))
	}
	c := refs(d)
	if err := c.MayShare(ctx, src); err != nil {
		return nil, refErr(err)
	}
	owner, name, err := d.receiverScope(ctx, r)
	if err != nil {
		return nil, err
	}
	share := account.Share(src, r, owner, name)
	if err := share.Validate(); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if err := c.Share(ctx, src, share); err != nil {
		return nil, refErr(err)
	}
	if err := d.Stores.Account.CreateShare(ctx, share); err != nil {
		if errors.Is(err, account.ErrShareTaken) {
			return nil, refErr(c.ShareExists(ctx, src, r))
		}
		return nil, mapWriteErr("account", share.Meta.ID, err)
	}
	audit.Record(ctx, "accounts.create", d.auditResource("account", share.Meta.ID, share.Meta.Name, share.Meta.Owner), audit.StatusAllowed)
	created, err := d.Stores.Account.Get(ctx, share.Meta.ID)
	if err != nil || created == nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("shared but could not read back: %v", err))
	}
	d.enrichAccount(ctx, created)
	return &itemResponse[account.Account]{Body: created}, nil
}

// receiverScope is the owner a share with r takes and r's name: r itself, or a service account's project.
func (d Deps) receiverScope(ctx context.Context, r account.Receiver) (meta.Owner, string, error) {
	missing := huma.Error404NotFound(fmt.Sprintf("%s %q not found", r.Kind, r.ID))
	switch r.Kind {
	case account.ReceiverUser:
		if d.Users == nil {
			return meta.Owner{}, "", huma.Error400BadRequest("this relay keeps no users to share with")
		}
		u, err := d.Users.Get(ctx, r.ID)
		if err != nil || u == nil {
			return meta.Owner{}, "", missing
		}
		return meta.Owner{Kind: meta.OwnerUser, ID: u.ID}, u.Username, nil
	case account.ReceiverTeam:
		t, err := d.Stores.Team.Get(ctx, r.ID)
		if err != nil || t == nil {
			return meta.Owner{}, "", missing
		}
		return meta.Owner{Kind: meta.OwnerTeam, ID: t.Meta.ID}, t.Meta.Name, nil
	case account.ReceiverProject:
		p, err := d.Stores.Project.Get(ctx, r.ID)
		if err != nil || p == nil {
			return meta.Owner{}, "", missing
		}
		return meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}, p.Meta.Name, nil
	case account.ReceiverServiceAccount:
		sa, err := d.Stores.ServiceAccount.Get(ctx, r.ID)
		if err != nil || sa == nil {
			return meta.Owner{}, "", missing
		}
		return meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}, sa.Meta.Name, nil
	}
	return meta.Owner{}, "", huma.Error400BadRequest("receiver.kind must be user, team, project or serviceaccount")
}

func (d Deps) listShares(ctx context.Context, ref, since string) (*listSharesOutput, error) {
	if since == "" {
		since = defaultShareUsageWindow
	}
	window, err := parseSince(since)
	if err != nil {
		return nil, err
	}
	src, err := d.findAccount(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := refs(d).MayShare(ctx, src); err != nil {
		return nil, refErr(err)
	}
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	to := time.Now()
	out := &listSharesOutput{Body: AccountShares{Items: []AccountShare{}, From: to.Add(-window), To: to}}
	for _, a := range all {
		if a.Spec.SourceAccountID != src.Meta.ID {
			continue
		}
		out.Body.Items = append(out.Body.Items, AccountShare{
			SharedAccount: ShareAccountRef{ID: a.Meta.ID, Name: a.Meta.Name},
			Receiver:      d.receiverParty(ctx, a.Receiver()),
		})
	}
	sort.Slice(out.Body.Items, func(i, j int) bool {
		return out.Body.Items[i].SharedAccount.Name < out.Body.Items[j].SharedAccount.Name
	})
	if d.UsageReader == nil || len(out.Body.Items) == 0 {
		return out, nil
	}
	// The owner reads the drain on their own account; the usage read scope, which is about callers, does not apply.
	q := usagelog.SummaryQuery{EventQuery: usagelog.EventQuery{From: out.Body.From, To: to, AccountID: []string{src.Meta.ID}}, GroupBy: "via_account_id"}
	res, err := d.UsageReader.Summary(ctx, q)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	byShare := make(map[string]usagelog.SummaryRow, len(res.Rows))
	for _, row := range res.Rows {
		byShare[row.Group["via_account_id"]] = row
	}
	for i := range out.Body.Items {
		row := byShare[out.Body.Items[i].SharedAccount.ID]
		out.Body.Items[i].Usage = &ShareUsage{Requests: row.Requests, CostNanos: row.CostNanos, Unpriced: row.Unpriced}
	}
	return out, nil
}

// receiverParty names r for reads.
func (d Deps) receiverParty(ctx context.Context, r account.Receiver) account.ShareParty {
	p := account.ShareParty{Kind: string(r.Kind), ID: r.ID}
	switch r.Kind {
	case account.ReceiverServiceAccount:
		if sa, err := d.Stores.ServiceAccount.Get(ctx, r.ID); err == nil && sa != nil {
			p.Name = sa.Meta.Name
		}
	default:
		p.Name = d.ownerName(ctx, meta.Owner{Kind: meta.OwnerKind(r.Kind), ID: r.ID})
	}
	return p
}

// ownerName is the username, or the team's or project's slug, that o names; empty for the system or a row that is gone.
func (d Deps) ownerName(ctx context.Context, o meta.Owner) string {
	switch o.Kind {
	case meta.OwnerUser:
		if d.Users != nil {
			if u, err := d.Users.Get(ctx, o.ID); err == nil && u != nil {
				return u.Username
			}
		}
	case meta.OwnerTeam:
		if t, err := d.Stores.Team.Get(ctx, o.ID); err == nil && t != nil {
			return t.Meta.Name
		}
	case meta.OwnerProject:
		if p, err := d.Stores.Project.Get(ctx, o.ID); err == nil && p != nil {
			return p.Meta.Name
		}
	}
	return ""
}

// enrichAccount fills a shared account's sharedBy and the holder's name.
func (d Deps) enrichAccount(ctx context.Context, a *account.Account) {
	d.enrichSharedBy(ctx, a)
	newPrincipalNamer(ctx, d, false).nameHolder(ctx, sharerAuthz{Authorizer: d.Authz, d: d}, a)
}

// enrichAccounts is enrichAccount over a list, reading users and service accounts once.
func (d Deps) enrichAccounts(ctx context.Context, items []*account.Account) {
	n := newPrincipalNamer(ctx, d, len(items) > 1)
	az := sharerAuthz{Authorizer: d.Authz, d: d}
	for _, a := range items {
		d.enrichSharedBy(ctx, a)
		n.nameHolder(ctx, az, a)
	}
}

// enrichSharedBy fills a shared account's sharedBy from its real account's owner.
func (d Deps) enrichSharedBy(ctx context.Context, a *account.Account) {
	if !a.IsShared() || d.Stores == nil || d.Stores.Account == nil {
		return
	}
	src, err := d.Stores.Account.Get(ctx, a.Spec.SourceAccountID)
	if err != nil || src == nil {
		return
	}
	o := src.Meta.Owner
	a.Spec.SharedBy = &account.ShareParty{Kind: string(o.Kind), ID: o.ID, Name: d.ownerName(ctx, o)}
}

// sharerAuthz lets whoever may share a real account read and delete the accounts shared from it, which live in the receivers' scopes.
type sharerAuthz struct {
	authz.Authorizer
	d Deps
}

// Authorize asks the sharer rule first, so a read it allows records no denial; a delete it allows is recorded as the delete it is.
func (a sharerAuthz) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if r.Kind == "account" && r.ID != "" && (action == "accounts.get" || action == "accounts.delete") && a.d.sharesOf(ctx, r.ID) {
		if action == "accounts.delete" && r.Owner != nil {
			audit.Record(ctx, action, a.d.auditResource("account", r.ID, r.Name, *r.Owner), audit.StatusAllowed)
		}
		return nil
	}
	return a.Authorizer.Authorize(ctx, action, r)
}

func (a sharerAuthz) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	if refcheck.Visible(ctx, a.Authorizer, kind, id, owner) {
		return true
	}
	return kind == "account" && id != "" && a.d.sharesOf(ctx, id)
}

// sharesOf reports whether id is a shared account whose real account the caller may share.
func (d Deps) sharesOf(ctx context.Context, id string) bool {
	a := d.accountRow(ctx, id)
	if a == nil || !a.IsShared() {
		return false
	}
	src := d.accountRow(ctx, a.Spec.SourceAccountID)
	if src == nil || actor.From(ctx) == nil {
		return false
	}
	c := refs(d)
	c.Authz = unaudited(c.Authz)
	return c.MayShare(ctx, src) == nil
}

// userVisible reports whether the caller may see the user, as a share's receiver: themselves, a caller who may read users, or one holding an account in a team or project the user holds one in.
func (d Deps) userVisible(ctx context.Context, id string) bool {
	a := actor.From(ctx)
	if a == nil {
		return false
	}
	if a.UserID == id || unaudited(d.Authz).Authorize(ctx, "users.get", userResource(id)) == nil {
		return true
	}
	if a.UserID == "" || d.Stores == nil || d.Stores.Account == nil {
		return false
	}
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return false
	}
	scopes := map[meta.Owner]int{}
	for _, acc := range all {
		h := acc.Spec.Holder
		if acc.DerivedKind() != account.KindMember || acc.Spec.Closed || h == nil {
			continue
		}
		switch h.ID {
		case a.UserID:
			scopes[acc.Meta.Owner] |= 1
		case id:
			scopes[acc.Meta.Owner] |= 2
		}
	}
	for _, both := range scopes {
		if both == 3 {
			return true
		}
	}
	return false
}

// keySharedWithCaller reports whether the caller sees an account shared from accountID, the account a key belongs to: the share's receivers see that key read-only.
func (d Deps) keySharedWithCaller(ctx context.Context, accountID string) bool {
	if accountID == "" || d.Stores == nil || d.Stores.Account == nil {
		return false
	}
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return false
	}
	for _, a := range all {
		if a.Spec.SourceAccountID == accountID && visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			return true
		}
	}
	return false
}

// accountRow reads an account from the published plans when there are any, else from the store; nil when absent.
func (d Deps) accountRow(ctx context.Context, id string) *account.Account {
	if d.PlanView != nil {
		if v := d.PlanView(); v != nil {
			return engine.Get[account.Account](v, id)
		}
	}
	if d.Stores == nil || d.Stores.Account == nil {
		return nil
	}
	a, _ := d.Stores.Account.Get(ctx, id)
	return a
}
