package control

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

// registerCRUD wires the CRUD kinds onto api and returns how each finds its rows. metaOf closures + slug resolvers are supplied per kind.
func registerCRUD(api huma.API, d Deps, protect huma.Middlewares) kindIndex {
	rows := kindIndex{}
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
	dk := newDirectoryKinds(d, newDirectoryOps(d))
	c := refs(d)

	rows.add(registerKind[team.Team](
		api, "teams", "team", dk.teams, d.Authz, tmeta,
		func(t *team.Team) error { return t.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Team, tmeta),
		nil,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&teamFilter,
		nil,
	))

	rows.add(registerKind[project.Project](
		api, "projects", "project", dk.projects, d.Authz, projmeta,
		func(p *project.Project) error { return p.Validate() },
		meta.OwnerTeam,
		listScanResolver(d.Stores.Project, projmeta),
		guardProject(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&projectFilter,
		nil,
	))

	rows.add(registerKind[serviceaccount.ServiceAccount](
		api, "service-accounts", "service-account", dk.serviceAccounts, d.Authz, sameta,
		func(sa *serviceaccount.ServiceAccount) error { return sa.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.ServiceAccount, sameta),
		guardServiceAccount(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&serviceAccountFilter,
		nil,
	))

	rows.add(registerKind[group.Group](
		api, "groups", "group", dk.groups, d.Authz, gmeta,
		func(g *group.Group) error { return g.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Group, gmeta),
		guardGroup(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&groupFilter,
		nil,
	))

	rows.add(registerKind[role.Role](
		api, "roles", "role", dk.roles, d.Authz, rolemeta,
		func(r *role.Role) error { return r.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Role, rolemeta),
		guardRole(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&roleFilter,
		nil,
	))

	rows.add(registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", dk.roleBindings, d.Authz, rbmeta,
		// Owner mirrors the scope exactly, so it is re-derived before the
		// row is checked rather than in the guard that runs after.
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver(d.Stores.RoleBinding, rbmeta),
		guardRoleBinding(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&roleBindingFilter,
		nil,
	))

	pbc := policyBindingChecks(d)
	rows.add(registerKind[policybinding.PolicyBinding](
		api, pbc.plural, pbc.singular, d.Stores.PolicyBinding, d.Authz, pbmeta,
		pbc.validate,
		// As for policies: an omitted owner is the caller's own, or system for the admin token.
		meta.OwnerUser,
		listScanResolver(d.Stores.PolicyBinding, pbmeta),
		pbc.guard,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&policyBindingFilter,
		nil,
	))

	rows.add(registerKind[provider.Provider](
		api, "providers", "provider", d.Stores.Provider, d.Authz, pmeta,
		func(p *provider.Provider) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Provider, pmeta),
		nil,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&providerFilter,
		nil,
	))

	rows.add(registerKind[host.Host](
		api, "hosts", "host", d.Stores.Host, d.Authz, hmeta,
		func(h *host.Host) error { return h.Validate() },
		"",
		listScanResolver(d.Stores.Host, hmeta),
		nil,
		enrichHostStatus(d),
		enrichHostStatusAll(d),
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&hostFilter,
		nil,
	))

	rows.add(registerKind[model.Model](
		api, "models", "model", d.Stores.Model, d.Authz, mmeta,
		func(m *model.Model) error { return m.Validate() },
		"",
		listScanResolver(d.Stores.Model, mmeta),
		nil,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&modelFilter,
		nil,
	))

	rows.add(registerKind[hostkey.HostKey](
		api, "host-keys", "host-key", d.Stores.HostKey, d.Authz, kmeta,
		func(k *hostkey.HostKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.HostKey, kmeta),
		guardHostKey(d),
		enrichHostKeyPolicies(d),
		enrichHostKeyPoliciesAll(d),
		d.refuseInUse,
		mergeHostKeyPreserveValue,
		d.Catalog,
		false,
		protect,
		&hostKeyFilter,
		usableFor(c, "pool", refcheck.Checker.UsableHostKey),
	))

	rows.add(registerKind[ratelimit.RateLimit](
		api, "rate-limits", "rate-limit", d.Stores.RateLimit, d.Authz, rlmeta,
		func(r *ratelimit.RateLimit) error { return r.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RateLimit, rlmeta),
		guardRateLimitMove(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&rateLimitFilter,
		usableFor(c, "policy", refcheck.Checker.UsableRateLimit),
	))

	rows.add(registerKind[policy.Policy](
		api, "policies", "policy", d.Stores.Policy, d.Authz, polmeta,
		func(p *policy.Policy) error { return p.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Policy, polmeta),
		guardPolicyModels(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&policyFilter,
		usableFor(c, "policy-binding", refcheck.Checker.UsablePolicy),
	))

	rows.add(registerKind[pricing.Pricing](
		api, "pricings", "pricing", d.Stores.Pricing, d.Authz, prmeta,
		func(p *pricing.Pricing) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Pricing, prmeta),
		nil,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&pricingFilter,
		nil,
	))

	rows.add(registerKind[binding.Binding](
		api, "host-bindings", "host-binding", d.Stores.Binding, d.Authz, bmeta,
		func(b *binding.Binding) error { return b.Validate() },
		"",
		listScanResolver(d.Stores.Binding, bmeta),
		nil,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&bindingFilter,
		nil,
	))

	// keys uses a custom POST handler (registerKeyCreate) that
	// generates the bearer plaintext server-side and returns it once. The
	// generic CRUD POST is therefore skipped here.
	rows.add(registerKind[key.Key](
		api, "keys", "key", d.Stores.Key, d.Authz, rkmeta,
		func(k *key.Key) error { return k.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.Key, rkmeta),
		guardKeyAccount(d),
		enrichKey(d),
		enrichKeys(d),
		d.refuseInUse,
		// Credential material is server-managed: PUT can neither wipe nor
		// overwrite it. Rotation goes through POST /keys/by-id/{id}/rotate.
		// An omitted accountId keeps the key's lane.
		func(existing, incoming *key.Key) {
			incoming.Spec.KeyHash = existing.Spec.KeyHash
			incoming.Spec.Prefix = existing.Spec.Prefix
			incoming.Spec.PreviousKeyHash = existing.Spec.PreviousKeyHash
			incoming.Spec.GraceUntil = existing.Spec.GraceUntil
			incoming.Spec.Principal = existing.Spec.Principal
			if incoming.Spec.AccountID == "" {
				incoming.Spec.AccountID = existing.Spec.AccountID
			}
		},
		d.Catalog,
		true, // skipCreate
		protect,
		&keyFilter,
		nil,
	))
	registerKeyCreate(api, d, protect)
	registerUpstreamCRUD(api, d, protect, rows)
	return rows
}
