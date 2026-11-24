// POST /pseudo-host-keys makes a provider key with the account it belongs to, the account's tier and its shares in one request (app/pseudo.ExpandHostKey), once the key's host has been asked about the value.

package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

type pseudoHostKeyInput struct {
	DryRun bool `query:"dryRun" doc:"Ask the host about the value and run every check, the plan check included, and answer what would be made, without writing it or storing the value."`
	Body   pseudo.HostKeyForm
}

// PseudoHostKeyCreated is what POST /pseudo-host-keys made, or would make on a dry run.
type PseudoHostKeyCreated struct {
	Created   []CreatedRow          `json:"created"             doc:"Every row made, in write order."`
	Account   RowRef                `json:"account"             doc:"The account the key belongs to: the one accountId names, or the one made."`
	HostKey   *RowRef               `json:"hostKey,omitempty"   doc:"The host key; absent for a host that takes no key."`
	Shares    []PseudoHostKeyShare  `json:"shares"              doc:"The account's share with each shareWith receiver, in order."`
	Detection string                `json:"detection,omitempty" enum:"detected,unsupported,unreachable" doc:"What the host did when asked which organization spec.value belongs to: named it (detected), names none (unsupported), or gave no answer or another status (unreachable). Absent when no value was checked: valueFrom, or a host that takes no key."`
	Detected  *DetectedOrganization `json:"detected,omitempty"  doc:"The organization the host named. A new account takes it as its orgId."`
	Warnings  []PseudoIssue         `json:"warnings"            doc:"Plan warnings the rows written would raise that are not there now, on those rows (as the row carrying the warning, or the row it names). They refuse nothing; [] when none. A row the caller may not see keeps its code and kind only."`
}

// PseudoHostKeyShare is the account's share with one shareWith receiver.
type PseudoHostKeyShare struct {
	Receiver account.Receiver `json:"receiver"           doc:"The receiver, as shareWith names it."`
	ID       string           `json:"id"                 doc:"Id of the share; for a receiver that reaches the account by ownership, the account's own."`
	Name     string           `json:"name"               doc:"Slug of that account."`
	Existing bool             `json:"existing,omitempty" doc:"True when nothing was made for the receiver: it holds this share already, or reaches the account by ownership."`
}

// DetectedOrganization is the provider organization a host named for a key.
type DetectedOrganization struct {
	OrgID   string `json:"orgId"             doc:"The organization's id."`
	OrgName string `json:"orgName,omitempty" doc:"The organization's display name, when the host sends one."`
}

type pseudoHostKeyOutput struct {
	Status int
	Body   PseudoHostKeyCreated
}

// Codes a pseudo host key is refused with besides its rows' own.
const (
	codeProviderRefusedKey   = "provider_refused_key"
	codeOrganizationTaken    = "organization_taken"
	codeOrganizationMismatch = "organization_mismatch"
)

// pseudoOrganizationRefusal is a 409 of a pseudo host key: the pseudo refusal, with the owner's account that holds the key's organization.
type pseudoOrganizationRefusal struct {
	httpapi.APIError
	Pointer string  `json:"pointer,omitempty" doc:"JSON pointer of the part of the body refused, such as /spec/accountId."`
	Account *RowRef `json:"account,omitempty" doc:"With organization_taken: the owner's account at the host that holds the key's organization; send its id as accountId."`
}

func registerPseudoHostKey(api huma.API, d Deps, protect huma.Middlewares) {
	responses := pseudoResponses(api, "")
	responses["409"] = jsonResponse(api, "organization_taken: the host named an organization another account of the owner at the host holds; account names it. organization_mismatch: the account named holds another organization. conflict: another change touched the same rows at the same time, retry; share_exists or reshare_refused from a share; duplicate_binding from the tier binding. pointer names the part of the body.", pseudoOrganizationRefusal{})
	huma.Register(api, huma.Operation{
		OperationID: "create_pseudo_host_key",
		Method:      http.MethodPost,
		Path:        "/pseudo-host-keys",
		Summary:     "Create a provider key with its account, the account's tier and its shares",
		Description: "First asks the create grants the host key and a new account will ask for the owner, with the owner rules of POST /host-keys: a refusal answers as that create would, before the host is asked anything or a value is stored. Then runs the whole write rolled back, without the value: every row's checks and the plan check refuse here, before the host is asked. Then asks the host which organization spec.value belongs to, with the one read POST /host-keys/by-id/{id}/detect-account makes, to the host's own base URL: a 401 or 403 refuses the request with 422 provider_refused_key and nothing is stored; a host that names no organization, or does not answer, refuses nothing and is answered in detection. A detected organization another account of the owner at the host holds answers 409 organization_taken with that account. Then, in one transaction, makes the account (spec.account, taking the detected orgId) or uses spec.accountId, binds it alone to spec.tierPolicyId (one of the host's tier presets, else 400) as PUT /accounts/{ref}/policy does, makes the host key in it, and shares the account with each spec.shareWith receiver that neither holds a share of it nor reaches it by ownership; those are answered with existing: true. A host that takes no key takes no value and gets no host key. spec.value is sealed under the owner's scope before the transaction and removed if the transaction does not commit. " +
			"Each row passes exactly what its own create call passes (POST /accounts, PUT /accounts/{ref}/policy, POST /host-keys, POST /accounts/{ref}/shares), with the same answers; a refusal names the row by pointer, the JSON pointer of the part of the body it came from, and nothing is written. Once every row passed, the rows are checked against the plans as a whole: errors they would raise that the plans do not have now refuse the request with 422 plan_issue. " +
			"dryRun=true asks the host the same once and does everything else but storing the value and the commit, and answers 200 with the same body, ids included. Each row made is audited as its own create; a dry run that asked the host is audited as one pseudo-host-keys.dry-run row on the host, whatever it answered. The value never appears in an answer or audit row.",
		Tags:          []string{"host-keys"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
		Responses:     responses,
	}, func(ctx context.Context, in *pseudoHostKeyInput) (*pseudoHostKeyOutput, error) {
		body := in.Body
		keyID := ids.New()
		var reads pseudo.HostKeyReads
		expandWith := func(check *keyCheck) pseudo.ExpandFunc {
			return func(ctx context.Context, tx *appcatalog.Stores) ([]pseudo.Row, error) {
				var err error
				if reads, err = d.boundTo(tx).hostKeyReads(ctx, body, check); err != nil {
					return nil, err
				}
				reads.HostKeyID = keyID
				return pseudo.ExpandHostKey(body, reads)
			}
		}
		preflight := func(ctx context.Context) error { return d.preflightPseudo(ctx, expandWith(nil)) }
		organization := func(c *keyCheck) error {
			if c.Outcome != keyDetected {
				return nil
			}
			detected := reads
			detected.Detected = &pseudo.Organization{ID: c.Org.ID, Name: c.Org.Name}
			_, err := pseudo.ExpandHostKey(body, detected)
			return err
		}
		check, err := d.checkHostKeyForm(ctx, &body, preflight, organization)
		if in.DryRun && check != nil {
			defer func() { audit.RecordOnly(ctx, []audit.Row{probedDryRunRow(ctx, body.Spec.HostID, err)}) }()
		}
		if err != nil {
			err = d.refusePseudo(ctx, err, in.DryRun)
			return nil, err
		}
		var outside []pseudo.Outside
		if v := body.Spec.Value; v != "" {
			outside = append(outside, d.sealHostKeyValue(keyID, body.Metadata.Owner, v))
		}
		w, rows, err := d.runPseudo(ctx, expandWith(check), in.DryRun, outside...)
		if err != nil {
			return nil, err
		}
		out := &pseudoHostKeyOutput{Status: http.StatusCreated, Body: hostKeyCreated(rows, body.Spec, reads, check)}
		out.Body.Warnings = w.warnings
		if in.DryRun {
			out.Status = http.StatusOK
		}
		return out, nil
	})
	documentPlanIssue(api, "/pseudo-host-keys")
	if item := api.OpenAPI().Paths["/pseudo-host-keys"]; item != nil && item.Post != nil {
		r := item.Post.Responses["422"]
		r.Description = "provider_refused_key: the host answered 401 or 403 to spec.value; nothing was written. " + r.Description
	}
}

// checkHostKeyForm runs what is checked before the value leaves relay or is stored: the body's own rules, its owner settled as POST /host-keys settles it, the host, the value against the host, the create grants the rows will ask (mayCreateHostKeyForm), the secret backend, then preflight, the whole write rolled back. Then it asks the provider about the value, whose refusal of the key refuses the request, and gives what it said to organization. It returns what the host said of the value, refused or not, nil when the provider was not asked.
func (d Deps) checkHostKeyForm(ctx context.Context, body *pseudo.HostKeyForm, preflight func(context.Context) error, organization func(*keyCheck) error) (*keyCheck, error) {
	if err := pseudo.CheckHostKey(*body); err != nil {
		return nil, err
	}
	if err := settleHostKeyOwner(ctx, &body.Metadata.Owner); err != nil {
		return nil, refusal(huma.Error400BadRequest(err.Error()), "/metadata/owner")
	}
	h, err := d.pseudoHost(ctx, body.Spec.HostID)
	if err != nil {
		return nil, err
	}
	if err := pseudo.CheckHostKeyValue(body.Spec, h.Meta.Name, h.Spec.NoAuth); err != nil {
		return nil, err
	}
	if err := d.mayCreateHostKeyForm(ctx, *body); err != nil {
		return nil, err
	}
	v := body.Spec.Value
	if v != "" && d.Stores.Stored == nil {
		return nil, refusal(mapWriteErr("host-key", "", hostkey.ErrNoSecretBackend), "/spec/value")
	}
	if err := preflight(ctx); err != nil {
		return nil, err
	}
	if v == "" {
		return nil, nil
	}
	c := checkKeyValue(ctx, d.Catalog.Current(), d.Adapters, h.Meta.ID, v)
	if c.Outcome == keyRefused {
		return &c, refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeProviderRefusedKey, Message: c.Reason + " to the key: the provider does not accept it; nothing was written"},
			HTTPStatus: http.StatusUnprocessableEntity,
		}, "/spec/value")
	}
	return &c, organization(&c)
}

// preflightPseudo runs expand and each step's checks and writes in a transaction it rolls back, with the plan check, as a dry run does, and records no audit row of its own: what a request would be refused for that needs no outside write is refused here.
func (d Deps) preflightPseudo(ctx context.Context, expand pseudo.ExpandFunc) error {
	w := &pseudoWrite{d: d, plaintexts: map[string]string{}}
	_, err := pseudo.Writer{Stores: d.Stores, Check: d.PlanCheck, View: d.PlanView}.Run(ctx, expand, w.write, true)
	return err
}

// refusePseudo answers and audits a pseudo request refused before its write, as runPseudo answers and audits one refused during it.
func (d Deps) refusePseudo(ctx context.Context, err error, dryRun bool) error {
	err = d.pseudoError(ctx, err)
	if dryRun {
		audit.Discard(ctx)
	} else {
		audit.RecordEach(ctx, []audit.Row{refusedRow(ctx, err)})
	}
	return err
}

// settleHostKeyOwner settles an owner as POST /host-keys does, before anything is written under it: the value is sealed under the scope of the owner the row ends up with.
func settleHostKeyOwner(ctx context.Context, o *meta.Owner) error {
	readSystemAsGlobal("host-key", o, meta.Owner{})
	if o.Kind == "" {
		o.Kind = meta.OwnerUser
	}
	return stampOwnerID(ctx, o)
}

// pseudoHost reads the host spec.hostId names, refusing one that does not exist (400) or the caller may not see (404), as a row naming it is refused.
func (d Deps) pseudoHost(ctx context.Context, id string) (*host.Host, error) {
	h, err := d.Stores.Host.Get(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case h == nil:
		return nil, refusal(huma.Error400BadRequest(fmt.Sprintf("host %q does not exist", id)), "/spec/hostId")
	case !visibleTo(ctx, d.Authz, "host", h.Meta.ID, h.Meta.Owner):
		return nil, refusal(huma.Error404NotFound(fmt.Sprintf("host %q not found", id)), "/spec/hostId")
	}
	return h, nil
}

// sealHostKeyValue writes value as the stored secret of host key id under owner's scope, as the host key store seals one, before the rows' transaction. Its undo removes the value unless the host key's row exists, its transaction having committed after all.
func (d Deps) sealHostKeyValue(id string, owner meta.Owner, value string) pseudo.Outside {
	return pseudo.Outside{
		What: fmt.Sprintf("the sealed value of host key %s", id),
		Write: func(ctx context.Context) error {
			if d.Stores.Stored == nil {
				return hostkey.ErrNoSecretBackend
			}
			_, err := d.Stores.Stored.CreateScoped(ctx, hostkey.SecretScope(owner), id, []byte(value))
			return err
		},
		Undo: func(ctx context.Context) error {
			k, err := d.Stores.HostKey.Get(ctx, id)
			if err != nil || k != nil {
				return err
			}
			return d.Stores.Stored.Delete(ctx, id)
		},
	}
}

// hostKeyReads reads what expanding body needs: the host, the owner's accounts there, the account accountId names (404 for one the caller may not see) and the binding holding it, the tier policy, and each receiver's scope and share of the account (404 for one that does not exist). It records them for the plan check, with the account's host keys.
func (d Deps) hostKeyReads(ctx context.Context, body pseudo.HostKeyForm, check *keyCheck) (pseudo.HostKeyReads, error) {
	s, owner := body.Spec, body.Metadata.Owner
	in := pseudo.HostKeyReads{Names: d.pseudoNamer(), NewID: ids.New}
	h, err := d.pseudoHost(ctx, s.HostID)
	if err != nil {
		return in, err
	}
	pseudo.Record(ctx, h.Meta.ID, h)
	in.HostName, in.Keyless = h.Meta.Name, h.Spec.NoAuth
	if check != nil && check.Outcome == keyDetected {
		in.Detected = &pseudo.Organization{ID: check.Org.ID, Name: check.Org.Name}
	}
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return in, err
	}
	for _, a := range all {
		sameOwner := a.Meta.Owner == owner || (a.Meta.Owner.DeploymentWide() && owner.DeploymentWide())
		if a.IsProvider() && !a.IsShared() && a.Spec.HostID == h.Meta.ID && sameOwner && visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			in.OwnerAccounts = append(in.OwnerAccounts, a)
		}
	}
	shares := map[account.Receiver]*account.Account{}
	if s.AccountID != "" {
		acc, err := d.findAccount(ctx, s.AccountID)
		if err != nil {
			return in, refusal(err, "/spec/accountId")
		}
		in.Account = acc
		if in.Holding, err = d.Stores.PolicyBinding.Holding(ctx, acc.Meta.ID); err != nil {
			return in, err
		}
		if held := in.Holding; held != nil {
			if s.TierPolicyID != "" && !visibleTo(ctx, d.Authz, "policy-binding", held.Meta.ID, held.Meta.Owner) {
				return in, refusal(huma.Error403Forbidden(fmt.Sprintf("account %q is held by a policy binding you may not see", acc.Meta.Name)), "/spec/tierPolicyId")
			}
			pseudo.Record(ctx, held.Meta.ID, held)
		}
		if err := d.recordUpstream(ctx, []string{acc.Meta.ID}, nil); err != nil {
			return in, err
		}
		sharer := sharerAuthz{Authorizer: d.Authz, d: d}
		for _, a := range all {
			if a.Spec.SourceAccountID == acc.Meta.ID && visibleTo(ctx, sharer, "account", a.Meta.ID, a.Meta.Owner) {
				shares[a.Receiver()] = a
			}
		}
	}
	if id := s.TierPolicyID; id != "" {
		p, err := d.Stores.Policy.Get(ctx, id)
		if err != nil {
			return in, err
		}
		if p != nil && visibleTo(ctx, d.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
			pseudo.Record(ctx, id, p)
			in.Tier = p
		}
	}
	c := refs(d)
	for i, r := range s.ShareWith {
		o, name, err := d.receiverScope(ctx, r)
		if err != nil {
			return in, refusal(err, fmt.Sprintf("/spec/shareWith/%d", i))
		}
		rc := pseudo.ShareReceiver{Owner: o, Name: name, Share: shares[r]}
		rc.Reaches = rc.Share == nil && c.ReachesByOwnership(ctx, owner, o)
		in.Receivers = append(in.Receivers, rc)
	}
	return in, nil
}

// createPseudoProviderAccount runs the checks POST /accounts runs on a provider account, which has its id and name, and writes it.
func (d Deps) createPseudoProviderAccount(ctx context.Context, a *account.Account) error {
	if err := checkCreate(ctx, providerAccountChecks(d), meta.OwnerUser, &a.Meta, a); err != nil {
		return err
	}
	return insertRow(ctx, accountRows{Store: d.Stores.Account, d: d}, a)
}

// hostKeyChecks are the checks POST /host-keys runs.
func hostKeyChecks(d Deps) kindChecks[hostkey.HostKey] {
	return kindChecks[hostkey.HostKey]{
		plural: "host-keys", singular: "host-key", authz: d.Authz, gov: d.Catalog,
		validate: func(k *hostkey.HostKey) error { return k.Validate() },
		guard:    guardHostKey(d),
	}
}

// createPseudoHostKey runs the checks POST /host-keys runs on k, which has its id and name, and writes its row. A stored value was sealed at k's id before the transaction, so the row keeps it; an env value is read back as the store resolves it, for the plan check.
func (d Deps) createPseudoHostKey(ctx context.Context, k *hostkey.HostKey) error {
	if err := checkCreate(ctx, hostKeyChecks(d), meta.OwnerUser, &k.Meta, k); err != nil {
		return err
	}
	if k.Spec.ValueFrom.Kind == hostkey.ValueKindStored {
		k.KeyHash = hostkey.KeyHashOf(k.Spec.Value)
		return d.Stores.HostKey.UpsertKeepingValue(ctx, k)
	}
	if err := insertRow(ctx, d.Stores.HostKey, k); err != nil {
		return err
	}
	stored, err := d.Stores.HostKey.Get(ctx, k.Meta.ID)
	if err != nil || stored == nil {
		return fmt.Errorf("created host key %s but could not read it back: %v", k.Meta.ID, err)
	}
	k.KeyHash, k.Status = stored.KeyHash, stored.Status
	return nil
}

// organizationRefusal answers a key whose organization does not fit its account.
func organizationRefusal(e *pseudo.OrganizationError) error {
	code := codeOrganizationMismatch
	var holder *RowRef
	if e.Holder != nil {
		code = codeOrganizationTaken
		ref := rowRef(&e.Holder.Meta)
		holder = &ref
	}
	return &pseudoOrganizationRefusal{
		APIError: httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: code, Message: e.Message}, HTTPStatus: http.StatusConflict},
		Pointer:  e.Pointer, Account: holder,
	}
}

// hostKeyCreated answers what the rows are, by role, with each receiver's share and what the host said of the value.
func hostKeyCreated(rows []pseudo.Row, s pseudo.HostKeyFormSpec, in pseudo.HostKeyReads, check *keyCheck) PseudoHostKeyCreated {
	out := PseudoHostKeyCreated{Created: createdRows(rows), Shares: []PseudoHostKeyShare{}}
	if a := in.Account; a != nil {
		out.Account = rowRef(&a.Meta)
	}
	made := map[string]RowRef{}
	for _, r := range rows {
		switch {
		case r.Kind == "account" && r.Pointer == "/spec/account":
			out.Account = rowRef(r.Meta)
		case r.Kind == "account":
			made[r.Pointer] = rowRef(r.Meta)
		case r.Kind == "host-key":
			ref := rowRef(r.Meta)
			out.HostKey = &ref
		}
	}
	for i, recv := range s.ShareWith {
		sh := PseudoHostKeyShare{Receiver: recv}
		switch rc := in.Receivers[i]; {
		case rc.Share != nil:
			sh.ID, sh.Name, sh.Existing = rc.Share.Meta.ID, rc.Share.Meta.Name, true
		case rc.Reaches:
			sh.ID, sh.Name, sh.Existing = out.Account.ID, out.Account.Name, true
		default:
			ref := made[fmt.Sprintf("/spec/shareWith/%d", i)]
			sh.ID, sh.Name = ref.ID, ref.Name
		}
		out.Shares = append(out.Shares, sh)
	}
	if check != nil {
		out.Detection = string(check.Outcome)
		if check.Outcome == keyDetected {
			out.Detected = &DetectedOrganization{OrgID: check.Org.ID, OrgName: check.Org.Name}
		}
	}
	return out
}
