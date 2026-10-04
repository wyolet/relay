// GET /{kind}/by-id/{id}/references — generic reverse-ref lookup.
//
// Returns every PG row that references the target entity. Used by the
// admin UI for blast-radius confirmation dialogs ("deleting this rate
// limit will affect N policies") and for inline "in use by" lists.
//
// Walks the relevant stores per kind; no SLO. Referencing rows the caller
// may not see are dropped, so the endpoint needs no gate of its own.
// Indices come later if scan time matters. Reading PG (not the catalog snapshot) so
// disabled / soft-dropped refs are still visible — they exist in PG even
// when the data plane has filtered them out.
package control

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/settings"
)

type referenceItem struct {
	Kind string `json:"kind" doc:"Kind of the referencing row: an API singular (policy, host-key, role-binding, …), or overlay, settings, user."`
	ID   string `json:"id"   doc:"Resource id."`
	Name string `json:"name" doc:"Resource slug."`
	Via  string `json:"via"  doc:"Field path on the referencing row that points at the target."`

	// owner is the referencing row's provenance, carried for scoping only
	// (never serialized).
	owner meta.Owner
	// detachable marks an optional field or a list entry: the referencing row
	// stays valid without the target, so it can let go instead of blocking.
	detachable bool
}

type referencesOutput struct {
	Body struct {
		Items []referenceItem `json:"items"`
	}
}

type referencesInput struct {
	ID string `path:"id" doc:"Target resource id."`
}

// referenceScans maps a kind (API singular) to the scan listing the rows
// that reference one of its rows. Keys, host bindings, role bindings and
// policy bindings have no scan: no row points at one.
var referenceScans = map[string]func(ctx context.Context, d Deps, id string) ([]referenceItem, error){
	"provider":        scanProviderRefs,
	"host":            scanHostRefs,
	"model":           scanModelRefs,
	"pricing":         scanPricingRefs,
	"policy":          scanPolicyRefs,
	"host-key":        scanHostKeyRefs,
	"rate-limit":      scanRateLimitRefs,
	"team":            scanTeamRefs,
	"project":         scanProjectRefs,
	"service-account": scanServiceAccountRefs,
	"group":           scanGroupRefs,
	"role":            scanRoleRefs,
}

// registerReferences installs the per-kind references endpoints.
func registerReferences(api huma.API, d Deps, protect huma.Middlewares) {
	for _, plural := range []string{
		"providers", "hosts", "models", "pricings", "policies", "host-keys",
		"rate-limits", "teams", "projects", "service-accounts", "groups", "roles",
	} {
		singular := authz.Singular(plural)
		scan := referenceScans[singular]
		huma.Register(api, huma.Operation{
			OperationID: "list_" + singular + "_references",
			Method:      http.MethodGet,
			Path:        "/" + plural + "/by-id/{id}/references",
			Summary:     "List rows that reference this " + singular,
			Tags:        []string{plural},
			Middlewares: protect,
			Errors:      []int{401, 500},
		}, func(ctx context.Context, in *referencesInput) (*referencesOutput, error) {
			items, err := scan(ctx, d, in.ID)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			// Drop referencing rows the caller may not see — "in use by" must
			// not leak other users' rows.
			if s, ok := d.Authz.(authz.Scoper); ok {
				visible := items[:0:0]
				for _, it := range items {
					if s.Visible(ctx, it.Kind, it.ID, it.owner) {
						visible = append(visible, it)
					}
				}
				items = visible
			}
			sortReferences(items)
			out := &referencesOutput{}
			out.Body.Items = items
			return out, nil
		})
	}
}

func sortReferences(items []referenceItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
}

func scanProviderRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	models, err := d.Stores.Model.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	for _, m := range models {
		if m.Meta.Owner.ID == id {
			out = append(out, referenceItem{Kind: "model", ID: m.Meta.ID, Name: m.Meta.Name, Via: "metadata.owner.id", owner: m.Meta.Owner})
		}
	}
	owned, err := scanRateLimitsOwnedBy(ctx, d, meta.OwnerProvider, id)
	if err != nil {
		return nil, err
	}
	return append(out, owned...), nil
}

func scanHostRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.HostID == id {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.hostId", owner: k.Meta.Owner})
		}
	}
	bindings, err := d.Stores.Binding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range bindings {
		if b.Spec.HostID == id {
			out = append(out, referenceItem{Kind: "host-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.hostId", owner: b.Meta.Owner})
		}
	}
	pricings, err := d.Stores.Pricing.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pricings: %w", err)
	}
	for _, p := range pricings {
		if p.Meta.Owner.ID == id {
			out = append(out, referenceItem{Kind: "pricing", ID: p.Meta.ID, Name: p.Meta.Name, Via: "metadata.owner.id", owner: p.Meta.Owner})
		}
	}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		if ownedBy(p.Meta.Owner, meta.OwnerHost, id) {
			out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "metadata.owner.id", owner: p.Meta.Owner})
		}
	}
	owned, err := scanRateLimitsOwnedBy(ctx, d, meta.OwnerHost, id)
	if err != nil {
		return nil, err
	}
	return append(out, owned...), nil
}

// scanRateLimitsOwnedBy returns the rate limits a provider or host publishes
// as its own upstream tiers.
func scanRateLimitsOwnedBy(ctx context.Context, d Deps, kind meta.OwnerKind, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	rls, err := d.Stores.RateLimit.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list rate-limits: %w", err)
	}
	for _, r := range rls {
		if ownedBy(r.Meta.Owner, kind, id) {
			out = append(out, referenceItem{Kind: "rate-limit", ID: r.Meta.ID, Name: r.Meta.Name, Via: "metadata.owner.id", owner: r.Meta.Owner})
		}
	}
	return out, nil
}

func scanModelRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		for _, mid := range p.Spec.ModelIDs {
			if mid == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.modelIds", owner: p.Meta.Owner, detachable: true})
				break
			}
		}
	}
	pricings, err := d.Stores.Pricing.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pricings: %w", err)
	}
	for _, p := range pricings {
		for _, mid := range p.Spec.TargetModelIDs {
			if mid == id {
				// targetModels needs at least one entry: a pricing can let go
				// of this model only while it prices another.
				out = append(out, referenceItem{Kind: "pricing", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.targetModels", owner: p.Meta.Owner,
					detachable: len(p.Spec.TargetModelIDs) > 1})
				break
			}
		}
	}
	bindings, err := d.Stores.Binding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range bindings {
		if b.Spec.ModelID == id {
			out = append(out, referenceItem{Kind: "host-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.modelId", owner: b.Meta.Owner})
		}
	}
	o, err := d.Stores.Overlay.Get(ctx, overlay.KindModel, id)
	if err != nil {
		return nil, fmt.Errorf("get overlay: %w", err)
	}
	if o == nil {
		return out, nil
	}
	m, err := d.Stores.Model.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get model: %w", err)
	}
	if m != nil {
		// An overlay has no id or name of its own; it goes by its model's.
		out = append(out, referenceItem{Kind: "overlay", ID: m.Meta.ID, Name: m.Meta.Name, Via: "resourceId", owner: m.Meta.Owner})
	}
	return out, nil
}

func scanPricingRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	bindings, err := d.Stores.Binding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range bindings {
		if b.Spec.PricingID == id {
			out = append(out, referenceItem{Kind: "host-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.pricingId", owner: b.Meta.Owner, detachable: true})
		}
	}
	return out, nil
}

func scanPolicyRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	rks, err := d.Stores.Key.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, k := range rks {
		if k.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.policyId", owner: k.Meta.Owner, detachable: true})
		}
	}
	sas, err := d.Stores.ServiceAccount.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list service-accounts: %w", err)
	}
	for _, sa := range sas {
		if sa.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "service-account", ID: sa.Meta.ID, Name: sa.Meta.Name, Via: "spec.policyId", owner: sa.Meta.Owner, detachable: true})
		}
	}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.policyId", owner: k.Meta.Owner})
		}
	}
	pbs, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policy-bindings: %w", err)
	}
	for _, b := range pbs {
		if b.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "policy-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.policyId", owner: b.Meta.Owner})
		}
	}
	hosts, err := d.Stores.Host.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	for _, h := range hosts {
		matched := false
		for _, pid := range h.Spec.Policies {
			if pid == id {
				out = append(out, referenceItem{Kind: "host", ID: h.Meta.ID, Name: h.Meta.Name, Via: "spec.policies", owner: h.Meta.Owner, detachable: true})
				matched = true
				break
			}
		}
		if !matched && h.Spec.DefaultPolicy == id {
			out = append(out, referenceItem{Kind: "host", ID: h.Meta.ID, Name: h.Meta.Name, Via: "spec.defaultPolicy", owner: h.Meta.Owner, detachable: true})
		}
	}
	return out, nil
}

func scanHostKeyRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		for _, kid := range p.Spec.HostKeyIDs {
			if kid == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.hostKeyIds", owner: p.Meta.Owner, detachable: true})
				break
			}
		}
	}
	// A host key's delete removes the stored secret kept under its id, so a
	// settings section naming that secret is a reference too. Settings are
	// reconfigured by the operator, never detached.
	rows, err := d.Stores.Settings.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	for _, r := range rows {
		for field, ref := range settings.SecretRefs(r.Value) {
			if ref.ID == id {
				out = append(out, referenceItem{Kind: "settings", ID: r.Section, Name: r.Section, Via: field, owner: meta.Owner{Kind: meta.OwnerSystem}})
			}
		}
	}
	return out, nil
}

func scanRateLimitRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		if p.Spec.RateLimitID == id {
			out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.rateLimitId", owner: p.Meta.Owner, detachable: true})
			continue
		}
		for _, b := range p.Spec.RLBindings {
			if b.RateLimitID == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.rlBindings[].rateLimitId", owner: p.Meta.Owner, detachable: true})
				break
			}
		}
	}
	return out, nil
}

func scanTeamRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	projects, err := d.Stores.Project.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if p.Spec.TeamID == id {
			out = append(out, referenceItem{Kind: "project", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.teamId", owner: p.Meta.Owner})
		}
	}
	scoped, err := scanRoleBindingsScopedTo(ctx, d, meta.OwnerTeam, id)
	if err != nil {
		return nil, err
	}
	return append(out, scoped...), nil
}

func scanProjectRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		if ownedBy(p.Meta.Owner, meta.OwnerProject, id) {
			out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "metadata.owner.id", owner: p.Meta.Owner})
		}
	}
	rks, err := d.Stores.Key.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, k := range rks {
		if ownedBy(k.Meta.Owner, meta.OwnerProject, id) {
			out = append(out, referenceItem{Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "metadata.owner.id", owner: k.Meta.Owner})
		}
	}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if ownedBy(k.Meta.Owner, meta.OwnerProject, id) {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "metadata.owner.id", owner: k.Meta.Owner})
		}
	}
	rls, err := d.Stores.RateLimit.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list rate-limits: %w", err)
	}
	for _, r := range rls {
		if ownedBy(r.Meta.Owner, meta.OwnerProject, id) {
			out = append(out, referenceItem{Kind: "rate-limit", ID: r.Meta.ID, Name: r.Meta.Name, Via: "metadata.owner.id", owner: r.Meta.Owner})
		}
	}
	sas, err := d.Stores.ServiceAccount.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list service-accounts: %w", err)
	}
	for _, sa := range sas {
		if sa.Spec.ProjectID == id {
			out = append(out, referenceItem{Kind: "service-account", ID: sa.Meta.ID, Name: sa.Meta.Name, Via: "spec.projectId", owner: sa.Meta.Owner})
		}
	}
	pbs, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policy-bindings: %w", err)
	}
	for _, b := range pbs {
		if b.Spec.ProjectID == id {
			out = append(out, referenceItem{Kind: "policy-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.projectId", owner: b.Meta.Owner})
		}
	}
	scoped, err := scanRoleBindingsScopedTo(ctx, d, meta.OwnerProject, id)
	if err != nil {
		return nil, err
	}
	return append(out, scoped...), nil
}

func scanServiceAccountRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	keys, err := d.Stores.Key.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.Principal.Kind == key.PrincipalServiceAccount && k.Spec.Principal.ID == id {
			out = append(out, referenceItem{Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.principal.id", owner: k.Meta.Owner})
		}
	}
	bound, err := scanBindingSubjects(ctx, d, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id})
	if err != nil {
		return nil, err
	}
	return append(out, bound...), nil
}

// scanGroupRefs finds bindings naming the group. Group subjects carry the
// name, so the scan reads the group first.
func scanGroupRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	g, err := d.Stores.Group.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}
	if g == nil {
		return []referenceItem{}, nil
	}
	return scanBindingSubjects(ctx, d, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: g.Meta.Name})
}

// scanBindingSubjects returns the role and policy bindings naming subject.
// Subjects needs at least one entry, so a binding can let go only while it
// names someone else.
func scanBindingSubjects(ctx context.Context, d Deps, subject rolebinding.Subject) ([]referenceItem, error) {
	out := []referenceItem{}
	want := subject.Key()
	rbs, err := d.Stores.RoleBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role-bindings: %w", err)
	}
	for _, b := range rbs {
		if namesSubject(b.Spec.Subjects, want) {
			out = append(out, referenceItem{Kind: "role-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.subjects", owner: b.Meta.Owner,
				detachable: len(b.Spec.Subjects) > 1})
		}
	}
	pbs, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policy-bindings: %w", err)
	}
	for _, b := range pbs {
		if namesSubject(b.Spec.Subjects, want) {
			out = append(out, referenceItem{Kind: "policy-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.subjects", owner: b.Meta.Owner,
				detachable: len(b.Spec.Subjects) > 1})
		}
	}
	return out, nil
}

// scanRoleBindingsScopedTo returns the role bindings whose scope names
// (kind, id) — the tenancy row's blast radius when it is deleted.
func scanRoleBindingsScopedTo(ctx context.Context, d Deps, kind meta.OwnerKind, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	rbs, err := d.Stores.RoleBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role-bindings: %w", err)
	}
	for _, b := range rbs {
		if b.Spec.Scope.Kind == kind && b.Spec.Scope.ID == id {
			out = append(out, referenceItem{Kind: "role-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.scope", owner: b.Meta.Owner})
		}
	}
	return out, nil
}

func scanRoleRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	rbs, err := d.Stores.RoleBinding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role-bindings: %w", err)
	}
	for _, b := range rbs {
		if b.Spec.RoleID == id {
			out = append(out, referenceItem{Kind: "role-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.roleId", owner: b.Meta.Owner})
		}
	}
	// Accounts name their roles by name, not id; an account holding the
	// admin role is what keeps that row from being deleted.
	r, err := d.Stores.Role.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get role: %w", err)
	}
	if r == nil || d.Users == nil {
		return out, nil
	}
	users, err := d.Users.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if u.HasRole(r.Meta.Name) {
			out = append(out, referenceItem{Kind: "user", ID: u.ID, Name: u.Username, Via: "roles", owner: meta.Owner{Kind: meta.OwnerSystem}, detachable: true})
		}
	}
	return out, nil
}

// namesSubject reports whether subjects hold the subject whose Key is want.
func namesSubject(subjects []rolebinding.Subject, want string) bool {
	for i := range subjects {
		if subjects[i].Key() == want {
			return true
		}
	}
	return false
}

func ownedBy(o meta.Owner, kind meta.OwnerKind, id string) bool {
	return o.Kind == kind && o.ID == id
}
