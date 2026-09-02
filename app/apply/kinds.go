package apply

import (
	"context"
	"fmt"
	"sort"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
)

// KindRoutes maps a manifest kind to the API plural and singular the RBAC
// verb and the export query use. Kinds absent from a manifest (Setting) are
// not listed.
var KindRoutes = map[string]struct{ Plural, Singular string }{
	"Team":           {"teams", "team"},
	"Project":        {"projects", "project"},
	"Provider":       {"providers", "provider"},
	"Host":           {"hosts", "host"},
	"RateLimit":      {"rate-limits", "rate-limit"},
	"HostKey":        {"host-keys", "host-key"},
	"Model":          {"models", "model"},
	"Pricing":        {"pricings", "pricing"},
	"HostBinding":    {"host-bindings", "host-binding"},
	"Policy":         {"policies", "policy"},
	"Group":          {"groups", "group"},
	"Role":           {"roles", "role"},
	"ServiceAccount": {"service-accounts", "service-account"},
	"Key":            {"keys", "key"},
	"RoleBinding":    {"role-bindings", "role-binding"},
	"PolicyBinding":  {"policy-bindings", "policy-binding"},
	"Overlay":        {"models.overlay", "model"},
}

func singularOf(plural string) string {
	for _, r := range KindRoutes {
		if r.Plural == plural {
			return r.Singular
		}
	}
	return plural
}

// builder accumulates plan entries in dependency order.
type builder struct {
	opts     Options
	rows     *Rows
	idx      *index
	selector labelSelector
	// admin relaxes the rules an operator may cross: today only re-parenting
	// a row onto a different owner.
	admin bool

	entries []Entry
	// deletes are collected per kind in the same order the upserts run and
	// flushed in reverse, so a pruned parent (Team) goes after its children
	// (Projects) and PG's foreign keys stay satisfied.
	deletes [][]Entry
}

func (b *builder) run(docs []manifest.Document) error {
	var (
		teamDocs []*manifest.TeamDTO
		projDocs []*manifest.ProjectDTO
		provDocs []*manifest.ProviderDTO
		hostDocs []*manifest.HostDTO
		rlDocs   []*manifest.RateLimitDTO
		hkDocs   []*manifest.HostKeyDTO
		mDocs    []*manifest.ModelDTO
		prDocs   []*manifest.PricingDTO
		bndDocs  []*manifest.HostBindingDTO
		polDocs  []*manifest.PolicyDTO
		saDocs   []*manifest.ServiceAccountDTO
		grpDocs  []*manifest.GroupDTO
		roleDocs []*manifest.RoleDTO
		rbDocs   []*manifest.RoleBindingDTO
		pbDocs   []*manifest.PolicyBindingDTO
		keyDocs  []*manifest.KeyDTO
		ovDocs   []*manifest.OverlayDTO
	)
	for _, d := range docs {
		switch {
		case d.Team != nil:
			teamDocs = append(teamDocs, d.Team)
		case d.Project != nil:
			projDocs = append(projDocs, d.Project)
		case d.Provider != nil:
			provDocs = append(provDocs, d.Provider)
		case d.Host != nil:
			hostDocs = append(hostDocs, d.Host)
		case d.RateLimit != nil:
			rlDocs = append(rlDocs, d.RateLimit)
		case d.HostKey != nil:
			hkDocs = append(hkDocs, d.HostKey)
		case d.Model != nil:
			mDocs = append(mDocs, d.Model)
		case d.Pricing != nil:
			prDocs = append(prDocs, d.Pricing)
		case d.HostBinding != nil:
			bndDocs = append(bndDocs, d.HostBinding)
		case d.Policy != nil:
			polDocs = append(polDocs, d.Policy)
		case d.ServiceAccount != nil:
			saDocs = append(saDocs, d.ServiceAccount)
		case d.Group != nil:
			grpDocs = append(grpDocs, d.Group)
		case d.Role != nil:
			roleDocs = append(roleDocs, d.Role)
		case d.RoleBinding != nil:
			rbDocs = append(rbDocs, d.RoleBinding)
		case d.PolicyBinding != nil:
			pbDocs = append(pbDocs, d.PolicyBinding)
		case d.Key != nil:
			keyDocs = append(keyDocs, d.Key)
		case d.Overlay != nil:
			ovDocs = append(ovDocs, d.Overlay)
		}
	}

	// Mint ids before translate so cross-refs resolve against names this
	// same apply introduces.
	mintIDs(b.idx.Teams, teamDocs, docName[manifest.TeamDTO])
	mintIDs(b.idx.Projects, projDocs, docName[manifest.ProjectDTO])
	mintIDs(b.idx.Providers, provDocs, docName[manifest.ProviderDTO])
	mintIDs(b.idx.Hosts, hostDocs, docName[manifest.HostDTO])
	mintIDs(b.idx.RateLimits, rlDocs, docName[manifest.RateLimitDTO])
	mintIDs(b.idx.HostKeys, hkDocs, docName[manifest.HostKeyDTO])
	mintIDs(b.idx.Models, mDocs, docName[manifest.ModelDTO])
	mintIDs(b.idx.Pricings, prDocs, docName[manifest.PricingDTO])
	mintIDs(b.idx.Bindings, bndDocs, docName[manifest.HostBindingDTO])
	mintIDs(b.idx.Policies, polDocs, docName[manifest.PolicyDTO])
	mintIDs(b.idx.ServiceAccounts, saDocs, docName[manifest.ServiceAccountDTO])
	mintIDs(b.idx.Groups, grpDocs, docName[manifest.GroupDTO])
	mintIDs(b.idx.Roles, roleDocs, docName[manifest.RoleDTO])
	mintIDs(b.idx.RoleBindings, rbDocs, docName[manifest.RoleBindingDTO])
	mintIDs(b.idx.PolicyBindings, pbDocs, docName[manifest.PolicyBindingDTO])
	mintIDs(b.idx.Keys, keyDocs, docName[manifest.KeyDTO])

	s := b.opts.Stores

	// Tenancy first: every kind below may be owned by a project.
	if err := planKind(b, kindWiring[manifest.TeamDTO, team.Team]{
		Kind: "Team", Docs: teamDocs, Names: b.idx.Teams, Rows: b.rows.Teams,
		To: manifest.ToTeam, Meta: func(t *team.Team) *meta.Metadata { return &t.Meta },
		Upsert: s.Team.Upsert, Delete: s.Team.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.ProjectDTO, project.Project]{
		Kind: "Project", Docs: projDocs, Names: b.idx.Projects, Rows: b.rows.Projects,
		To: manifest.ToProject, Meta: func(p *project.Project) *meta.Metadata { return &p.Meta },
		Upsert: s.Project.Upsert, Delete: s.Project.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.ProviderDTO, provider.Provider]{
		Kind: "Provider", Docs: provDocs, Names: b.idx.Providers, Rows: b.rows.Providers,
		To: manifest.ToProvider, Meta: func(p *provider.Provider) *meta.Metadata { return &p.Meta },
		Upsert: s.Provider.Upsert, Delete: s.Provider.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.HostDTO, host.Host]{
		Kind: "Host", Docs: hostDocs, Names: b.idx.Hosts, Rows: b.rows.Hosts,
		To: manifest.ToHost, Meta: func(h *host.Host) *meta.Metadata { return &h.Meta },
		Upsert: s.Host.Upsert, Delete: s.Host.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
		Kind: "RateLimit", Docs: rlDocs, Names: b.idx.RateLimits, Rows: b.rows.RateLimits,
		To: manifest.ToRateLimit, Meta: func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta },
		Upsert: s.RateLimit.Upsert, Delete: s.RateLimit.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.HostKeyDTO, hostkey.HostKey]{
		Kind: "HostKey", Docs: hkDocs, Names: b.idx.HostKeys, Rows: b.rows.HostKeys,
		To: manifest.ToHostKey, Meta: func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta },
		Upsert: s.HostKey.Upsert, Delete: s.HostKey.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.ModelDTO, model.Model]{
		Kind: "Model", Docs: mDocs, Names: b.idx.Models, Rows: b.rows.Models,
		To: manifest.ToModel, Meta: func(m *model.Model) *meta.Metadata { return &m.Meta },
		Upsert: s.Model.Upsert, Delete: s.Model.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.PricingDTO, pricing.Pricing]{
		Kind: "Pricing", Docs: prDocs, Names: b.idx.Pricings, Rows: b.rows.Pricings,
		To: manifest.ToPricing, Meta: func(p *pricing.Pricing) *meta.Metadata { return &p.Meta },
		Upsert: s.Pricing.Upsert, Delete: s.Pricing.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.HostBindingDTO, binding.Binding]{
		Kind: "HostBinding", Docs: bndDocs, Names: b.idx.Bindings, Rows: b.rows.Bindings,
		To: manifest.ToHostBinding, Meta: func(x *binding.Binding) *meta.Metadata { return &x.Meta },
		Upsert: s.HostBinding.Upsert, Delete: s.HostBinding.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.PolicyDTO, policy.Policy]{
		Kind: "Policy", Docs: polDocs, Names: b.idx.Policies, Rows: b.rows.Policies,
		To: manifest.ToPolicy, Meta: func(p *policy.Policy) *meta.Metadata { return &p.Meta },
		Upsert: s.Policy.Upsert, Delete: s.Policy.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.GroupDTO, group.Group]{
		Kind: "Group", Docs: grpDocs, Names: b.idx.Groups, Rows: b.rows.Groups,
		To: manifest.ToGroup, Meta: func(g *group.Group) *meta.Metadata { return &g.Meta },
		Upsert: s.Group.Upsert, Delete: s.Group.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.RoleDTO, role.Role]{
		Kind: "Role", Docs: roleDocs, Names: b.idx.Roles, Rows: b.rows.Roles,
		To: manifest.ToRole, Meta: func(r *role.Role) *meta.Metadata { return &r.Meta },
		Upsert: s.Role.Upsert, Delete: s.Role.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.ServiceAccountDTO, serviceaccount.ServiceAccount]{
		Kind: "ServiceAccount", Docs: saDocs, Names: b.idx.ServiceAccounts, Rows: b.rows.ServiceAccounts,
		To: manifest.ToServiceAccount, Meta: func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta },
		Upsert: s.ServiceAccount.Upsert, Delete: s.ServiceAccount.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.KeyDTO, key.Key]{
		Kind: "Key", Docs: keyDocs, Names: b.idx.Keys, Rows: b.rows.Keys,
		To: manifest.ToKey, Meta: func(k *key.Key) *meta.Metadata { return &k.Meta },
		Upsert: s.Key.Upsert, Delete: s.Key.Delete,
	}); err != nil {
		return err
	}
	// Bindings last: they name roles, tenancy rows, policies, and the
	// principals every kind above just wrote.
	if err := planKind(b, kindWiring[manifest.RoleBindingDTO, rolebinding.RoleBinding]{
		Kind: "RoleBinding", Docs: rbDocs, Names: b.idx.RoleBindings, Rows: b.rows.RoleBindings,
		To: manifest.ToRoleBinding, Meta: func(x *rolebinding.RoleBinding) *meta.Metadata { return &x.Meta },
		Upsert: s.RoleBinding.Upsert, Delete: s.RoleBinding.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(b, kindWiring[manifest.PolicyBindingDTO, policybinding.PolicyBinding]{
		Kind: "PolicyBinding", Docs: pbDocs, Names: b.idx.PolicyBindings, Rows: b.rows.PolicyBindings,
		To: manifest.ToPolicyBinding, Meta: func(x *policybinding.PolicyBinding) *meta.Metadata { return &x.Meta },
		Upsert: s.PolicyBinding.Upsert, Delete: s.PolicyBinding.Delete,
	}); err != nil {
		return err
	}
	if err := b.planOverlays(ovDocs); err != nil {
		return err
	}

	for i := len(b.deletes) - 1; i >= 0; i-- {
		b.entries = append(b.entries, b.deletes[i]...)
	}
	return nil
}

// kindWiring is the per-kind glue planKind needs: the documents, the name→id
// map they mint into, the stored rows, and the translate/store calls.
type kindWiring[D any, T any] struct {
	Kind   string
	Docs   []*D
	Names  map[string]string
	Rows   []*T
	To     func(D, manifest.Resolver) (*T, error)
	Meta   func(*T) *meta.Metadata
	Upsert func(context.Context, *T) error
	Delete func(context.Context, string) error
}

func planKind[D any, T any](b *builder, k kindWiring[D, T]) error {
	route := KindRoutes[k.Kind]
	existing := make(map[string]*T, len(k.Rows))
	for _, row := range k.Rows {
		existing[k.Meta(row).Name] = row
	}

	declared := make(map[string]bool, len(k.Docs))
	for _, d := range k.Docs {
		wm := docMeta(d)
		name := wm.Name
		if declared[name] {
			return &DuplicateError{Kind: k.Kind, Name: name}
		}
		declared[name] = true

		obj, err := k.To(*d, b.idx)
		if err != nil {
			return fmt.Errorf("apply: %s %q: %w", k.Kind, name, err)
		}
		m := k.Meta(obj)
		m.ID = k.Names[name]
		// A declared row is no longer hand-edited: apply owns it now.
		m.Dirty = false

		e := Entry{Kind: k.Kind, Name: name, ID: m.ID, plural: route.Plural, owner: m.Owner}
		if wm.ID != "" && wm.ID != m.ID {
			e.IDMismatch = wm.ID
		}

		prev, found := existing[name]
		switch {
		case !found:
			e.Action = ActionCreate
		case k.Meta(prev).Dirty && !b.opts.Force:
			e.Action = ActionSkipDirty
		default:
			// The stored owner wins: a document must not move a row into
			// another tenant's scope. An operator may still re-parent.
			if !b.admin {
				m.Owner = k.Meta(prev).Owner
			}
			e.owner = m.Owner
			fields := changedFields(viewOf(prev, k.Meta(prev)), viewOf(obj, m))
			if len(fields) == 0 {
				e.Action = ActionUnchanged
			} else {
				e.Action = ActionUpdate
				e.ChangedFields = fields
			}
		}
		if e.Action == ActionCreate || e.Action == ActionUpdate {
			if err := validateRow(obj); err != nil {
				return &InvalidError{Kind: k.Kind, Name: name, Err: err}
			}
			if err := b.governs(settings.OpEdit, route.Singular, e.owner); err != nil {
				return &GovernanceError{Kind: k.Kind, Name: name, Err: err}
			}
			row := obj
			e.write = func(ctx context.Context) error { return k.Upsert(ctx, row) }
		}
		b.entries = append(b.entries, e)
	}

	var pruned []Entry
	if b.opts.Prune {
		for _, row := range k.Rows {
			m := k.Meta(row)
			if declared[m.Name] || !prunable(k.Kind, m.Owner) || !b.selector.matches(m.Labels) {
				continue
			}
			if err := b.governs(settings.OpDelete, route.Singular, m.Owner); err != nil {
				return &GovernanceError{Kind: k.Kind, Name: m.Name, Err: err}
			}
			id := m.ID
			pruned = append(pruned, Entry{
				Kind: k.Kind, Name: m.Name, ID: id, Action: ActionDelete,
				plural: route.Plural, owner: m.Owner,
				write: func(ctx context.Context) error { return k.Delete(ctx, id) },
			})
		}
		sort.Slice(pruned, func(i, j int) bool { return pruned[i].Name < pruned[j].Name })
	}
	b.deletes = append(b.deletes, pruned)
	return nil
}

// validateRow runs the kind's own Validate when it has one. Every catalog
// kind does; the assertion keeps planKind free of a per-kind wiring field.
func validateRow(row any) error {
	v, ok := row.(interface{ Validate() error })
	if !ok {
		return nil
	}
	return v.Validate()
}

// governs applies the governance:<kind> settings to a planned mutation. The
// system tier of settings.Governs is a rule about generic CRUD; apply is the
// declarative loader that owns system rows (providers, hosts, built-in
// roles), so system-owned rows are exempt here.
func (b *builder) governs(op settings.Op, kind string, owner meta.Owner) error {
	if b.opts.Gov == nil || owner.Kind == meta.OwnerSystem {
		return nil
	}
	return settings.Governs(b.opts.Gov, op, kind, string(owner.Kind))
}

// planOverlays diffs the model overlays. Overlays carry no metadata of their
// own — they are keyed by (kind, target row) and owned by the user who wrote
// them — so they have no dirty flag, are never pruned, and authorize under
// the model verbs the overlay endpoints already use.
func (b *builder) planOverlays(docs []*manifest.OverlayDTO) error {
	if len(docs) == 0 {
		return nil
	}
	if b.opts.Stores.Overlay == nil {
		return fmt.Errorf("apply: overlay store not wired")
	}
	existing := make(map[string]*overlay.Overlay, len(b.rows.Overlays))
	for _, o := range b.rows.Overlays {
		existing[o.Key()] = o
	}
	route := KindRoutes["Overlay"]
	for _, d := range docs {
		o, err := manifest.ToOverlay(*d, b.idx)
		if err != nil {
			return fmt.Errorf("apply: Overlay %q: %w", d.Metadata.Name, err)
		}
		e := Entry{
			Kind: "Overlay", Name: d.Metadata.Name, ID: o.ResourceID,
			plural: route.Plural, owner: meta.Owner{Kind: meta.OwnerUser},
		}
		prev, found := existing[o.Key()]
		switch {
		case !found:
			e.Action = ActionCreate
		case string(prev.Patch) == string(o.Patch):
			e.Action = ActionUnchanged
		default:
			e.Action = ActionUpdate
			e.ChangedFields = []string{"spec.patch"}
		}
		if e.Action == ActionCreate || e.Action == ActionUpdate {
			row := o
			e.write = func(ctx context.Context) error { return b.opts.Stores.Overlay.Upsert(ctx, row) }
		}
		b.entries = append(b.entries, e)
	}
	return nil
}

// docName and docMeta read the shared metadata block off any wire DTO. The
// DTOs are plain structs with no common interface, so reflection-free access
// goes through a tiny type switch.
func docName[D any](d *D) string { return docMeta(d).Name }

func docMeta[D any](d *D) manifest.WireMeta {
	switch v := any(d).(type) {
	case *manifest.TeamDTO:
		return v.Metadata
	case *manifest.ProjectDTO:
		return v.Metadata
	case *manifest.ProviderDTO:
		return v.Metadata
	case *manifest.HostDTO:
		return v.Metadata
	case *manifest.RateLimitDTO:
		return v.Metadata
	case *manifest.HostKeyDTO:
		return v.Metadata
	case *manifest.ModelDTO:
		return v.Metadata
	case *manifest.PricingDTO:
		return v.Metadata
	case *manifest.HostBindingDTO:
		return v.Metadata
	case *manifest.PolicyDTO:
		return v.Metadata
	case *manifest.GroupDTO:
		return v.Metadata
	case *manifest.RoleDTO:
		return v.Metadata
	case *manifest.ServiceAccountDTO:
		return v.Metadata
	case *manifest.KeyDTO:
		return v.Metadata
	case *manifest.RoleBindingDTO:
		return v.Metadata
	case *manifest.PolicyBindingDTO:
		return v.Metadata
	case *manifest.OverlayDTO:
		return v.Metadata
	}
	return manifest.WireMeta{}
}
