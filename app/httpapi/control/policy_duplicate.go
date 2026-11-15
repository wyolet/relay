// POST /policies/{ref}/duplicate copies a policy into a new row bound nowhere: the same spec, linking the same pools, rate limits and budgets, under a new id and name and the owner the caller picks. It is how an editor changes the limits of the accounts they manage when the policy is also applied to accounts whose policy they may not set. The copy is authorized and checked as POST /policies of the same row would be. Out of scope: copying bindings, labels or annotations.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/slug"
)

type duplicatePolicyBody struct {
	DisplayName string      `json:"displayName,omitempty" doc:"Display name of the copy; its slug derives from it. Omitted: the source's display name (or slug) followed by \" copy\"."`
	Owner       *meta.Owner `json:"owner,omitempty"       doc:"Owner of the copy, as on POST /policies. Omitted: the caller's own user, or the system for the admin token."`
}

type duplicatePolicyInput struct {
	Ref  string               `path:"ref" doc:"Slug or id of the policy to copy."`
	Body *duplicatePolicyBody `json:"body,omitempty" required:"false"`
}

func registerPolicyDuplicate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID:   "duplicate_policy",
		Method:        http.MethodPost,
		Path:          "/policies/{ref}/duplicate",
		Summary:       "Copy a policy",
		Description:   "Creates a policy with the source's spec, linking the same pools, rate limits and budgets (not copies of them), under a new id and a slug derived from displayName. The copy is bound to no account and carries no labels. It needs read access to the source and is authorized and checked as POST /policies of the copy with that owner: a link the owner's scope may not use answers 400. Use it when an update answers 403 policy_in_use: bind the copy to the accounts you manage, then edit it.",
		Tags:          []string{"policies"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *duplicatePolicyInput) (*itemResponse[policy.Policy], error) {
		var body duplicatePolicyBody
		if in.Body != nil {
			body = *in.Body
		}
		return d.duplicatePolicy(ctx, in.Ref, body)
	})
}

func (d Deps) duplicatePolicy(ctx context.Context, ref string, in duplicatePolicyBody) (*itemResponse[policy.Policy], error) {
	src, err := d.readPolicy(ctx, ref)
	if err != nil {
		return nil, err
	}
	polmeta := func(p *policy.Policy) *meta.Metadata { return &p.Meta }
	cp := &policy.Policy{Spec: src.Spec}
	m := &cp.Meta
	m.ID = ids.New()
	m.DisplayName = in.DisplayName
	if m.DisplayName == "" {
		m.DisplayName = policyDisplay(src.Meta) + " copy"
	}
	base := slug.From(m.DisplayName)
	if base == "" {
		base = "policy"
	}
	m.Name = slug.Unique(base, slugTakenFn(d.Stores.Policy, polmeta))
	if in.Owner != nil {
		m.Owner = *in.Owner
	}
	// The owner rules of POST /policies, whose default owner kind is user.
	if m.Owner.Kind == meta.OwnerSystem {
		return nil, huma.Error400BadRequest("owner.kind=system is reserved for seed; omit owner.kind on create")
	}
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerUser
	}
	if err := stampOwnerID(ctx, &m.Owner); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	checks := policyChecks(d)
	if err := checks.guarded(ctx, "create", nil, cp); err != nil {
		return nil, err
	}
	if err := checks.mayCreate(ctx, &m.Owner); err != nil {
		return nil, err
	}
	if err := checks.valid(cp); err != nil {
		return nil, err
	}
	audit.Changed(ctx, []string{audit.AnyField})
	if err := d.Stores.Policy.Upsert(ctx, cp); err != nil {
		return nil, mapWriteErr("policy", m.ID, err)
	}
	created, err := d.Stores.Policy.Get(ctx, m.ID)
	if err != nil || created == nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("created policy %s but could not read it back: %v", m.ID, err))
	}
	enrichPolicyEditable(d)(ctx, created)
	return &itemResponse[policy.Policy]{Body: created}, nil
}

// readPolicy finds a policy by slug or id as GET /policies/{ref} does: one the caller may not read answers 404.
func (d Deps) readPolicy(ctx context.Context, ref string) (*policy.Policy, error) {
	notFound := huma.Error404NotFound(fmt.Sprintf("policy %q not found", ref))
	id := ref
	if !ids.Valid(ref) {
		resolved, err := listScanResolver(d.Stores.Policy, func(p *policy.Policy) *meta.Metadata { return &p.Meta })(ref)
		if err != nil {
			return nil, notFound
		}
		id = resolved
	}
	p, err := d.Stores.Policy.Get(ctx, id)
	if err != nil || p == nil {
		return nil, notFound
	}
	if err := d.Authz.Authorize(ctx, "policies.get", authz.Resource{Kind: "policy", ID: p.Meta.ID, Name: ref, Owner: &p.Meta.Owner}); err != nil {
		if errors.Is(err, authz.ErrUnauthenticated) {
			return nil, mapAuthzErr(err)
		}
		return nil, notFound
	}
	return p, nil
}

// policyDisplay is a policy's display name, or its slug when it has none.
func policyDisplay(m meta.Metadata) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}
