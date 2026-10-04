package control

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// registerCRUD wires the eight kinds onto api. metaOf closures + slug
// resolvers are supplied per kind.
func registerCRUD(api huma.API, d Deps, protect huma.Middlewares) {
	pmeta := func(p *provider.Provider) *meta.Metadata { return &p.Meta }
	hmeta := func(h *host.Host) *meta.Metadata { return &h.Meta }
	mmeta := func(m *model.Model) *meta.Metadata { return &m.Meta }
	kmeta := func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta }
	rlmeta := func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }
	polmeta := func(p *policy.Policy) *meta.Metadata { return &p.Meta }
	prmeta := func(p *pricing.Pricing) *meta.Metadata { return &p.Meta }
	bmeta := func(b *binding.Binding) *meta.Metadata { return &b.Meta }
	rkmeta := func(k *key.Key) *meta.Metadata { return &k.Meta }
	tmeta := func(t *team.Team) *meta.Metadata { return &t.Meta }
	projmeta := func(p *project.Project) *meta.Metadata { return &p.Meta }
	sameta := func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta }
	gmeta := func(g *group.Group) *meta.Metadata { return &g.Meta }
	rolemeta := func(r *role.Role) *meta.Metadata { return &r.Meta }
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	pbmeta := func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta }

	registerKind[team.Team](
		api, "teams", "team", d.Stores.Team, d.Authz, tmeta,
		func(t *team.Team) error { return t.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Team, tmeta),
		guardTeamDelete(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&teamFilter,
	)

	registerKind[project.Project](
		api, "projects", "project", d.Stores.Project, d.Authz, projmeta,
		func(p *project.Project) error { return p.Validate() },
		meta.OwnerTeam,
		listScanResolver(d.Stores.Project, projmeta),
		guardProject(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&projectFilter,
	)

	registerKind[serviceaccount.ServiceAccount](
		api, "service-accounts", "service-account", d.Stores.ServiceAccount, d.Authz, sameta,
		func(sa *serviceaccount.ServiceAccount) error { return sa.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.ServiceAccount, sameta),
		guardServiceAccount(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&serviceAccountFilter,
	)

	registerKind[group.Group](
		api, "groups", "group", d.Stores.Group, d.Authz, gmeta,
		func(g *group.Group) error { return g.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Group, gmeta),
		guardGroupMembers(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&groupFilter,
	)

	registerKind[role.Role](
		api, "roles", "role", d.Stores.Role, d.Authz, rolemeta,
		func(r *role.Role) error { return r.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Role, rolemeta),
		guardRole(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&roleFilter,
	)

	registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", d.Stores.RoleBinding, d.Authz, rbmeta,
		// Owner mirrors the scope exactly, so it is re-derived before the
		// row is checked rather than in the guard that runs after.
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver(d.Stores.RoleBinding, rbmeta),
		guardRoleBinding(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&roleBindingFilter,
	)

	registerKind[policybinding.PolicyBinding](
		api, "policy-bindings", "policy-binding", d.Stores.PolicyBinding, d.Authz, pbmeta,
		func(b *policybinding.PolicyBinding) error { return b.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.PolicyBinding, pbmeta),
		guardPolicyBinding(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&policyBindingFilter,
	)

	registerKind[provider.Provider](
		api, "providers", "provider", d.Stores.Provider, d.Authz, pmeta,
		func(p *provider.Provider) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Provider, pmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&providerFilter,
	)

	registerKind[host.Host](
		api, "hosts", "host", d.Stores.Host, d.Authz, hmeta,
		func(h *host.Host) error { return h.Validate() },
		"",
		listScanResolver(d.Stores.Host, hmeta),
		nil,
		enrichHostStatus(d),
		enrichHostStatusAll(d),
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&hostFilter,
	)

	registerKind[model.Model](
		api, "models", "model", d.Stores.Model, d.Authz, mmeta,
		func(m *model.Model) error { return m.Validate() },
		"",
		listScanResolver(d.Stores.Model, mmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&modelFilter,
	)

	registerKind[hostkey.HostKey](
		api, "host-keys", "host-key", d.Stores.HostKey, d.Authz, kmeta,
		func(k *hostkey.HostKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.HostKey, kmeta),
		guardHostKey(d),
		enrichHostKeyPolicies(d),
		enrichHostKeyPoliciesAll(d),
		cascadeHostKeyDetach(d),
		mergeHostKeyPreserveValue,
		d.Catalog,
		false,
		protect,
		&hostKeyFilter,
	)

	registerKind[ratelimit.RateLimit](
		api, "rate-limits", "rate-limit", d.Stores.RateLimit, d.Authz, rlmeta,
		func(r *ratelimit.RateLimit) error { return r.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RateLimit, rlmeta),
		nil,
		nil,
		nil,
		cascadeRateLimitDetach(d),
		nil,
		d.Catalog,
		false,
		protect,
		&rateLimitFilter,
	)

	registerKind[policy.Policy](
		api, "policies", "policy", d.Stores.Policy, d.Authz, polmeta,
		func(p *policy.Policy) error { return p.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Policy, polmeta),
		guardPolicyModels(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&policyFilter,
	)

	registerKind[pricing.Pricing](
		api, "pricings", "pricing", d.Stores.Pricing, d.Authz, prmeta,
		func(p *pricing.Pricing) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Pricing, prmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&pricingFilter,
	)

	registerKind[binding.Binding](
		api, "host-bindings", "host-binding", d.Stores.Binding, d.Authz, bmeta,
		func(b *binding.Binding) error { return b.Validate() },
		"",
		listScanResolver(d.Stores.Binding, bmeta),
		guardHostBinding(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&bindingFilter,
	)

	// keys uses a custom POST handler (registerKeyCreate) that
	// generates the bearer plaintext server-side and returns it once. The
	// generic CRUD POST is therefore skipped here.
	registerKind[key.Key](
		api, "keys", "key", d.Stores.Key, d.Authz, rkmeta,
		func(k *key.Key) error { return k.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.Key, rkmeta),
		guardKeyPolicy(d),
		nil,
		nil,
		nil,
		// Credential material is server-managed: PUT can neither wipe nor
		// overwrite it. Rotation goes through POST /keys/by-id/{id}/rotate.
		func(existing, incoming *key.Key) {
			incoming.Spec.KeyHash = existing.Spec.KeyHash
			incoming.Spec.Prefix = existing.Spec.Prefix
			incoming.Spec.PreviousKeyHash = existing.Spec.PreviousKeyHash
			incoming.Spec.GraceUntil = existing.Spec.GraceUntil
			incoming.Spec.Principal = existing.Spec.Principal
		},
		d.Catalog,
		true, // skipCreate
		protect,
		&keyFilter,
	)
	registerKeyCreate(api, d, protect)
}
