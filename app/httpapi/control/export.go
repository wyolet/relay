// export.go serves GET /export: the tenant-owned rows of the deployment as
// a manifest bundle POST /apply accepts back unchanged. Catalog template
// rows (system-, provider-, host-owned) are never exported — they come from
// the catalog, not from anyone's git repo — but the overlays layered on them
// are.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/ids"
)

type exportInput struct {
	Kinds  string `query:"kinds"  doc:"Comma-separated API plurals to include. Default: every exportable kind."`
	Scope  string `query:"scope"  doc:"Restrict to a subtree: team:<id> or project:<id>."`
	Format string `query:"format" doc:"yaml (default) or json."`
}

type exportOutput struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

// exportOrder is the emit order: tenancy, then the rows that reference it,
// then the bindings that reference those, then the catalog overlays. A
// bundle replayed top to bottom never forward-references.
var exportOrder = []string{
	"teams", "projects", "groups", "roles", "rate-limits", "policies",
	"host-keys", "service-accounts", "keys", "role-bindings",
	"policy-bindings", "overlays",
}

func registerExport(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "export",
		Method:      http.MethodGet,
		Path:        "/export",
		Summary:     "Export the tenant-owned rows as a manifest bundle",
		Description: "Secrets never leave: host-key values are omitted, and a Key exports " +
			"only its hash, prefix, and expiry.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500},
	}, func(ctx context.Context, in *exportInput) (*exportOutput, error) {
		// No endpoint gate: every row is filtered by Visible, so a scoped
		// caller exports their own subtree and nothing else.
		if d.Stores == nil {
			return nil, huma.Error500InternalServerError("stores not wired")
		}
		e, err := newExporter(ctx, d, in)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		docs, err := e.documents(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &exportOutput{}
		if in.Format == "json" {
			payloads := make([]any, 0, len(docs))
			for _, doc := range docs {
				payloads = append(payloads, doc.Payload())
			}
			body, err := json.Marshal(struct {
				Documents []any `json:"documents"`
			}{payloads})
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			out.ContentType, out.Body = "application/json", body
			return out, nil
		}
		body, err := manifest.Render(docs, d.PublicURL)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out.ContentType, out.Body = "application/yaml", body
		return out, nil
	})
}

type exporter struct {
	d     Deps
	rows  *apply.Rows
	rev   manifest.MapReverseResolver
	kinds map[string]bool

	// scoped narrows the export to one subtree; the id sets are the teams
	// and projects that subtree covers.
	scoped   bool
	teamIDs  map[string]bool
	projects map[string]bool
}

func newExporter(ctx context.Context, d Deps, in *exportInput) (*exporter, error) {
	if in.Format != "" && in.Format != "yaml" && in.Format != "json" {
		return nil, fmt.Errorf("format must be yaml or json")
	}
	rows, err := apply.Load(ctx, applyStores(d))
	if err != nil {
		return nil, err
	}
	e := &exporter{d: d, rows: rows, rev: reverseResolver(rows)}

	if in.Kinds != "" {
		e.kinds = map[string]bool{}
		for _, k := range strings.Split(in.Kinds, ",") {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if !sliceHas(exportOrder, k) {
				return nil, fmt.Errorf("kind %q is not exportable", k)
			}
			e.kinds[k] = true
		}
	}

	if in.Scope != "" {
		kind, id, ok := strings.Cut(in.Scope, ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("scope must be team:<id> or project:<id>")
		}
		e.scoped = true
		e.teamIDs, e.projects = map[string]bool{}, map[string]bool{}
		switch kind {
		case "team":
			e.teamIDs[id] = true
			for _, p := range rows.Projects {
				if p.Spec.TeamID == id {
					e.projects[p.Meta.ID] = true
				}
			}
		case "project":
			e.projects[id] = true
		default:
			return nil, fmt.Errorf("scope must be team:<id> or project:<id>")
		}
	}
	return e, nil
}

// wanted reports whether a row belongs in the bundle: an exportable
// provenance, inside the requested scope, and visible to the caller.
func (e *exporter) wanted(ctx context.Context, plural, singular string, m *meta.Metadata) bool {
	if e.kinds != nil && !e.kinds[plural] {
		return false
	}
	// A role binding's owner mirrors its scope, so a global binding is
	// system-owned. It is still authored by a tenant, not shipped by the
	// catalog, so provenance does not decide its membership here — the
	// scope filter below still does, and a global binding (no team, no
	// project) only survives an unscoped export.
	if singular != "role-binding" {
		switch m.Owner.Kind {
		case meta.OwnerUser, meta.OwnerTeam, meta.OwnerProject:
		default:
			return false
		}
	}
	if e.scoped {
		switch {
		case singular == "team":
			if !e.teamIDs[m.ID] {
				return false
			}
		case singular == "project":
			if !e.projects[m.ID] {
				return false
			}
		case m.Owner.Kind == meta.OwnerTeam && e.teamIDs[m.Owner.ID]:
		case m.Owner.Kind == meta.OwnerProject && e.projects[m.Owner.ID]:
		default:
			return false
		}
	}
	return visibleTo(ctx, e.d.Authz, singular, m.ID, m.Owner)
}

func (e *exporter) documents(ctx context.Context) ([]manifest.Document, error) {
	var docs []manifest.Document
	for _, plural := range exportOrder {
		if e.kinds != nil && !e.kinds[plural] {
			continue
		}
		switch plural {
		case "teams":
			docs = appendKind(ctx, e, docs, plural, "team", e.rows.Teams,
				func(t *team.Team) *meta.Metadata { return &t.Meta },
				func(t *team.Team) manifest.Document {
					dto := manifest.FromTeam(t, e.rev)
					return manifest.Document{Team: &dto}
				})
		case "projects":
			docs = appendKind(ctx, e, docs, plural, "project", e.rows.Projects,
				func(p *project.Project) *meta.Metadata { return &p.Meta },
				func(p *project.Project) manifest.Document {
					dto := manifest.FromProject(p, e.rev)
					return manifest.Document{Project: &dto}
				})
		case "groups":
			docs = appendKind(ctx, e, docs, plural, "group", e.rows.Groups,
				func(g *group.Group) *meta.Metadata { return &g.Meta },
				func(g *group.Group) manifest.Document {
					dto := manifest.FromGroup(g, e.rev)
					return manifest.Document{Group: &dto}
				})
		case "roles":
			docs = appendKind(ctx, e, docs, plural, "role", e.rows.Roles,
				func(r *role.Role) *meta.Metadata { return &r.Meta },
				func(r *role.Role) manifest.Document {
					dto := manifest.FromRole(r, e.rev)
					return manifest.Document{Role: &dto}
				})
		case "rate-limits":
			docs = appendKind(ctx, e, docs, plural, "rate-limit", e.rows.RateLimits,
				func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta },
				func(r *ratelimit.RateLimit) manifest.Document {
					dto := manifest.FromRateLimit(r, e.rev)
					return manifest.Document{RateLimit: &dto}
				})
		case "policies":
			docs = appendKind(ctx, e, docs, plural, "policy", e.rows.Policies,
				func(p *policy.Policy) *meta.Metadata { return &p.Meta },
				func(p *policy.Policy) manifest.Document {
					dto := manifest.FromPolicy(p, e.rev)
					return manifest.Document{Policy: &dto}
				})
		case "host-keys":
			docs = appendKind(ctx, e, docs, plural, "host-key", e.rows.HostKeys,
				func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta },
				func(k *hostkey.HostKey) manifest.Document {
					dto := manifest.FromHostKey(k, e.rev)
					return manifest.Document{HostKey: &dto}
				})
		case "service-accounts":
			docs = appendKind(ctx, e, docs, plural, "service-account", e.rows.ServiceAccounts,
				func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta },
				func(sa *serviceaccount.ServiceAccount) manifest.Document {
					dto := manifest.FromServiceAccount(sa, e.rev)
					return manifest.Document{ServiceAccount: &dto}
				})
		case "keys":
			docs = appendKind(ctx, e, docs, plural, "key", e.rows.Keys,
				func(k *key.Key) *meta.Metadata { return &k.Meta },
				func(k *key.Key) manifest.Document {
					dto := manifest.FromKey(k, e.rev)
					return manifest.Document{Key: &dto}
				})
		case "role-bindings":
			docs = appendKind(ctx, e, docs, plural, "role-binding", e.rows.RoleBindings,
				func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta },
				func(b *rolebinding.RoleBinding) manifest.Document {
					dto := manifest.FromRoleBinding(b, e.rev)
					return manifest.Document{RoleBinding: &dto}
				})
		case "policy-bindings":
			docs = appendKind(ctx, e, docs, plural, "policy-binding", e.rows.PolicyBindings,
				func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta },
				func(b *policybinding.PolicyBinding) manifest.Document {
					dto := manifest.FromPolicyBinding(b, e.rev)
					return manifest.Document{PolicyBinding: &dto}
				})
		case "overlays":
			// Overlays are keyed by their target row, not by a tenant, so a
			// scoped export leaves them out rather than guessing.
			if e.scoped {
				continue
			}
			var ovs []manifest.Document
			for _, o := range e.rows.Overlays {
				dto, err := manifest.FromOverlay(o, e.rev)
				if err != nil {
					return nil, err
				}
				ovs = append(ovs, manifest.Document{Overlay: &dto})
			}
			sort.Slice(ovs, func(i, j int) bool {
				return ovs[i].Overlay.Metadata.Name < ovs[j].Overlay.Metadata.Name
			})
			docs = append(docs, ovs...)
		}
	}
	return docs, nil
}

func appendKind[T any](
	ctx context.Context,
	e *exporter,
	docs []manifest.Document,
	plural, singular string,
	rows []*T,
	metaOf func(*T) *meta.Metadata,
	doc func(*T) manifest.Document,
) []manifest.Document {
	kept := make([]*T, 0, len(rows))
	for _, r := range rows {
		if e.wanted(ctx, plural, singular, metaOf(r)) {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return metaOf(kept[i]).Name < metaOf(kept[j]).Name })
	for _, r := range kept {
		d := doc(r)
		e.nameOwner(d.Meta())
		docs = append(docs, d)
	}
	return docs
}

// nameOwner rewrites an owner reference from the stored id to the slug the
// manifest names it by, so a bundle in git reads as slugs, not UUIDs.
func (e *exporter) nameOwner(m *manifest.WireMeta) {
	if m == nil || !ids.Valid(m.Owner.Name) {
		return
	}
	var (
		name string
		ok   bool
	)
	switch m.Owner.Kind {
	case meta.OwnerTeam:
		name, ok = e.rev.TeamName(m.Owner.Name)
	case meta.OwnerProject:
		name, ok = e.rev.ProjectName(m.Owner.Name)
	case meta.OwnerUser:
		name, ok = e.rev.Username(m.Owner.Name)
	}
	if ok {
		m.Owner.Name = name
	}
}

// reverseResolver builds the id→name index the From* renderers need.
func reverseResolver(r *apply.Rows) manifest.MapReverseResolver {
	rev := manifest.MapReverseResolver{
		Providers: map[string]string{}, Hosts: map[string]string{},
		Policies: map[string]string{}, Models: map[string]string{},
		HostKeys: map[string]string{}, RateLimits: map[string]string{},
		Pricings: map[string]string{}, Bindings: map[string]string{},
		Teams: map[string]string{}, Projects: map[string]string{},
		ServiceAccounts: map[string]string{}, Groups: map[string]string{},
		Roles: map[string]string{}, Users: map[string]string{},
		ModelProviders: map[string]string{},
	}
	for _, x := range r.Providers {
		rev.Providers[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Hosts {
		rev.Hosts[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Policies {
		rev.Policies[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Models {
		rev.Models[x.Meta.ID] = x.Meta.Name
		rev.ModelProviders[x.Meta.ID] = x.Meta.Owner.ID
	}
	for _, x := range r.HostKeys {
		rev.HostKeys[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.RateLimits {
		rev.RateLimits[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Pricings {
		rev.Pricings[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Bindings {
		rev.Bindings[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Teams {
		rev.Teams[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Projects {
		rev.Projects[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.ServiceAccounts {
		rev.ServiceAccounts[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Groups {
		rev.Groups[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Roles {
		rev.Roles[x.Meta.ID] = x.Meta.Name
	}
	for _, u := range r.Users {
		rev.Users[u.ID] = u.Username
	}
	return rev
}

func sliceHas(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
