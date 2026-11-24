// POST /pseudo-agents makes a service account with its lane account, what it may use and its first key in one request (app/pseudo.ExpandAgent).

package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/ids"
)

type pseudoAgentInput struct {
	DryRun bool `query:"dryRun" doc:"Run every check, the plan check included, and answer what would be made, without writing it."`
	Body   pseudo.Agent
}

// PseudoAgentCreated is what POST /pseudo-agents made, or would make on a dry run.
type PseudoAgentCreated struct {
	Created        []CreatedRow    `json:"created"          doc:"Every row made, in write order."`
	ServiceAccount RowRef          `json:"serviceAccount"   doc:"The service account."`
	Account        RowRef          `json:"account"          doc:"The service account's account, its keys' default lane."`
	Policy         *RowRef         `json:"policy,omitempty" doc:"The policy the account is bound to: the one policyId names, or the one made from access. Absent when access made none."`
	Pools          []RowRef        `json:"pools"            doc:"The pools made, one per host of access.accountIds."`
	Shares         []RowRef        `json:"shares"           doc:"The shares made of accounts the owner did not reach."`
	Key            *PseudoAgentKey `json:"key,omitempty"    doc:"The key; absent when spec.key was omitted."`
	Warnings       []PseudoIssue   `json:"warnings"         doc:"Plan warnings the rows made would raise that are not there now, on those rows (as the row carrying the warning, or the row it names), such as model_ref_blocked for a model a policy above excludes. They refuse nothing; [] when none. A row the caller may not see keeps its code and kind only."`
}

// PseudoAgentKey is the service account's first key.
type PseudoAgentKey struct {
	ID        string `json:"id"                  doc:"Id of the key."`
	Name      string `json:"name"                doc:"Slug of the key."`
	Plaintext string `json:"plaintext,omitempty" doc:"The bearer token to send as Authorization: Bearer. Returned only here; store it now. Absent on a dry run."`
}

type pseudoAgentOutput struct {
	Status int
	Body   PseudoAgentCreated
}

func registerPseudoAgent(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_pseudo_agent",
		Method:      http.MethodPost,
		Path:        "/pseudo-agents",
		Summary:     "Create a service account with what it may use and its first key",
		Description: "In one transaction, makes the service account, its account (its keys' default lane, answered in account), and either a policy binding of that account to policyId, or from access: a share with the owner of each listed provider account the owner neither reaches nor holds a share of, one pool per host of accountIds (its accounts in the order given), a policy linking poolIds in the order given and then the pools made in the order each host first appears in accountIds, with models, and its binding; then the key when spec.key is given. With everyCredential and no models nothing more is made: the account's default reach is the access. An account at a host that takes no key answers 400: name that host's pool in poolIds. The pools and policy made are owned by the service account's owner and labelled agent-id=<service account id>. " +
			"Each row passes exactly what its own create call passes (POST /service-accounts, /accounts/{ref}/shares, /pools, /policies, PUT /accounts/{ref}/policy, POST /keys), with the same answers; a refusal names the row by pointer, the JSON pointer of the part of the body it came from, and nothing is written. Once every row passed, the rows are checked against the plans as a whole: errors they would raise that the plans do not have now refuse the request with 422 plan_issue. " +
			"dryRun=true does everything but the commit and answers 200 with the same body, ids included and the key's plaintext left out. Each row made is audited as its own create. A repeated request makes a second set, its names suffixed.",
		Tags:          []string{"service-accounts"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
		Responses:     pseudoResponses(api, "conflict: another change touched the same rows at the same time, retry; share_exists, already_reachable or reshare_refused from a share; duplicate_binding from the binding; managed_by_source. pointer names the row."),
	}, func(ctx context.Context, in *pseudoAgentInput) (*pseudoAgentOutput, error) {
		var reads pseudo.AgentReads
		expand := func(ctx context.Context, tx *appcatalog.Stores) ([]pseudo.Row, error) {
			if err := pseudo.CheckAgent(in.Body); err != nil {
				return nil, err
			}
			var err error
			if reads, err = d.boundTo(tx).agentReads(ctx, in.Body); err != nil {
				return nil, err
			}
			return pseudo.ExpandAgent(in.Body, reads)
		}
		w, rows, err := d.runPseudo(ctx, expand, in.DryRun)
		if err != nil {
			return nil, err
		}
		out := &pseudoAgentOutput{Status: http.StatusCreated, Body: agentCreated(rows, in.Body.Spec.PolicyID, reads.PolicyName)}
		out.Body.Warnings = w.warnings
		if in.DryRun {
			out.Status = http.StatusOK
			return out, nil
		}
		if k := out.Body.Key; k != nil {
			k.Plaintext = w.plaintexts[k.ID]
			if d.Plans != nil {
				hash := rowValue[key.Key](rows, k.ID).Spec.KeyHash
				awaitServed(ctx, "key", k.ID, func(ctx context.Context) bool { return d.Plans.AwaitKey(ctx, hash) })
			}
		}
		return out, nil
	})
	documentPlanIssue(api, "/pseudo-agents")
}

// agentReads reads what expanding body needs, refusing a listed pool a policy of the owner may not link, or an account the caller may not see, with its pointer: the listed accounts, their hosts' names and which take no key, the owner's shares, the owner's name, the bound policy's name, and the names taken. It records for the plan check what the agent will draw on: those accounts and hosts, the listed or bound policy's pools, and their accounts and host keys.
func (d Deps) agentReads(ctx context.Context, body pseudo.Agent) (pseudo.AgentReads, error) {
	owner := body.Metadata.Owner.Owner()
	c := refs(d)
	in := pseudo.AgentReads{
		Accounts:     map[string]*account.Account{},
		HostNames:    map[string]string{},
		KeylessHosts: map[string]bool{},
		Reaches:      func(a *account.Account) bool { return c.Reaches(ctx, a.Meta.Owner, owner) },
		OwnerName:    d.ownerName(ctx, owner),
		Names:        d.pseudoNamer(),
		NewID:        ids.New,
	}
	var upstreamAccounts, upstreamKeys, pools []string
	if a := body.Spec.Access; a != nil {
		for i, id := range a.PoolIDs {
			if err := c.PoolRefs(ctx, []string{id}, owner); err != nil {
				return in, refusal(refErr(err), fmt.Sprintf("/spec/access/poolIds/%d", i))
			}
		}
		pools = a.PoolIDs
	}
	if a := body.Spec.Access; a != nil && len(a.AccountIDs) > 0 {
		ar, err := d.readAccessAccounts(ctx, a.AccountIDs, owner)
		if err != nil {
			return in, err
		}
		in.Accounts, in.HostNames, in.KeylessHosts, in.Shares = ar.accounts, ar.hostNames, ar.keyless, ar.shares
		upstreamAccounts = append(upstreamAccounts, ar.upstream...)
	}
	if id := body.Spec.PolicyID; id != "" {
		in.PolicyName = "policy"
		if p, err := d.Stores.Policy.Get(ctx, id); err == nil && p != nil {
			pseudo.Record(ctx, id, p)
			in.PolicyName = p.Meta.Name
			pools = p.Spec.PoolIDs
		}
	}
	for _, id := range pools {
		p, err := d.Stores.Pool.Get(ctx, id)
		if err != nil {
			return in, err
		}
		if p != nil {
			pseudo.Record(ctx, id, p)
			upstreamAccounts = append(upstreamAccounts, p.Spec.AccountIDs...)
			upstreamKeys = append(upstreamKeys, p.Spec.CredentialIDs...)
		}
	}
	return in, d.recordUpstream(ctx, upstreamAccounts, upstreamKeys)
}

// agentCreated answers what the rows are, by role.
func agentCreated(rows []pseudo.Row, policyID, policyName string) PseudoAgentCreated {
	out := PseudoAgentCreated{Created: createdRows(rows), Pools: []RowRef{}, Shares: []RowRef{}}
	if policyID != "" {
		out.Policy = &RowRef{ID: policyID, Name: policyName}
	}
	for _, r := range rows {
		ref := rowRef(r.Meta)
		switch {
		case r.Kind == "service-account":
			out.ServiceAccount = ref
		case r.Kind == "account" && r.Pointer == "/metadata":
			out.Account = ref
		case r.Kind == "account":
			out.Shares = append(out.Shares, ref)
		case r.Kind == "pool":
			out.Pools = append(out.Pools, ref)
		case r.Kind == "policy":
			out.Policy = &ref
		case r.Kind == "key":
			out.Key = &PseudoAgentKey{ID: ref.ID, Name: ref.Name}
		}
	}
	return out
}

// rowValue is the row of type T with id among rows; nil when there is none.
func rowValue[T any](rows []pseudo.Row, id string) *T {
	for _, r := range rows {
		if v, ok := r.Value.(*T); ok && r.Meta.ID == id {
			return v
		}
	}
	return nil
}
