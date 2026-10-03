package apply

import (
	"context"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
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
	// admin relaxes the rules an operator may cross: only re-parenting a row
	// onto a different owner.
	admin bool
	// lic gates the features a manifest may declare. Nil is no gate — see
	// Options.License.
	lic license.Checker

	// declared holds the rows this run writes, by id, so a later kind's
	// reference checks see what an earlier kind in the same bundle declared.
	declared map[string]any

	entries []Entry
	// deletes are collected per kind in the same order the upserts run and
	// flushed in reverse, so a pruned parent (Team) goes after its children
	// (Projects) and PG's foreign keys stay satisfied.
	deletes [][]Entry
}

func (b *builder) run(ctx context.Context, docs []manifest.Document) error {
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
		if d.Setting != nil || d.Foreign != "" {
			return &UnsupportedKindError{Kind: d.Kind()}
		}
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

	if err := b.checkRoleDocs(roleDocs); err != nil {
		return err
	}

	// Tenancy first: every kind below may be owned by a project.
	if err := b.planTenancy(ctx, teamDocs, projDocs); err != nil {
		return err
	}
	if err := b.planCatalog(ctx, provDocs, hostDocs, rlDocs, hkDocs, mDocs, prDocs, bndDocs, polDocs); err != nil {
		return err
	}
	if err := b.planGroupsAndRoles(ctx, grpDocs, roleDocs); err != nil {
		return err
	}
	if err := b.planServiceAccountsAndKeys(ctx, saDocs, keyDocs); err != nil {
		return err
	}
	// Bindings last: they name roles, tenancy rows, policies, and the
	// principals every kind above just wrote.
	if err := b.planBindings(ctx, rbDocs, pbDocs); err != nil {
		return err
	}
	if err := b.planOverlays(ovDocs); err != nil {
		return err
	}
	if err := b.checkPrunedTenancy(); err != nil {
		return err
	}

	for i := len(b.deletes) - 1; i >= 0; i-- {
		b.entries = append(b.entries, b.deletes[i]...)
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
