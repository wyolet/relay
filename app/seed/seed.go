// Package seed loads a directory of manifest YAMLs into Postgres at boot.
//
// It is app/apply driven from a directory: the same parse → mint → translate
// → diff → upsert path the control plane's POST /apply runs, with no
// authorizer (boot has no actor) and no prune (a catalog tree is additive).
// ClearDirty maps to apply's Force. Everything else — bucket order, id
// minting, the dirty-row skip, the change vocabulary — lives in app/apply.
package seed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/manifest"
)

// Options configures a seed run.
type Options struct {
	Pool      *pgxpool.Pool
	YAMLDir   string
	MasterKey []byte // for stored-mode HostKey rows; nil disables them

	// ClearDirty re-seeds (overwrites) rows an operator has edited, resetting
	// them to the catalog version and clearing their dirty flag. Default false:
	// dirty rows are skipped so re-seeding never clobbers operator changes.
	ClearDirty bool

	// CatalogKindsOnly refuses the tenancy kinds. The boot paths set it: a
	// catalog tree is fetched by tag from a public repository and must not be
	// able to mint a Key, a ServiceAccount or a RoleBinding on the way in.
	// Those kinds reach Postgres through `relay seed --apply`, `relay apply`
	// or the control API, all of which have an actor to authorize.
	CatalogKindsOnly bool
}

// tenancyKinds are the kinds a catalog tree may not carry: they name
// principals, credentials or grants rather than shared catalog templates.
var tenancyKinds = map[string]bool{
	"Team": true, "Project": true, "Group": true, "ServiceAccount": true,
	"Key": true, "Role": true, "RoleBinding": true, "PolicyBinding": true,
}

// Result summarises a seed run. Per-kind counts are the rows the run
// reconciled — created, updated, or already matching the manifest.
type Result struct {
	Teams           int
	Projects        int
	Providers       int
	Hosts           int
	RateLimits      int
	HostKeys        int
	Models          int
	Pricings        int
	HostBindings    int
	Policies        int
	ServiceAccounts int
	Groups          int
	Roles           int
	RoleBindings    int
	PolicyBindings  int
	Keys            int
	Overlays        int
	Skipped         int // dirty rows preserved (operator-edited; not re-seeded)
}

// Run executes the seed pipeline end-to-end.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("seed: Pool is required")
	}
	if opts.YAMLDir == "" {
		return nil, fmt.Errorf("seed: YAMLDir is required")
	}

	docs, err := manifest.LoadDir(opts.YAMLDir)
	if err != nil {
		return nil, fmt.Errorf("seed: load yaml: %w", err)
	}
	docs, settingDocs, refused := splitSeedDocs(docs, opts.CatalogKindsOnly)
	// A Setting in a seeded tree is not applied here and would otherwise
	// vanish without a trace, so each one is named.
	for _, name := range settingDocs {
		slog.Warn("seed: Setting document not applied; load it with RELAY_SETTINGS_DIR",
			"dir", opts.YAMLDir, "document", name)
	}
	if len(refused) > 0 {
		slog.Warn("seed: refusing tenancy documents from a catalog tree",
			"dir", opts.YAMLDir, "count", len(refused), "documents", refused)
	}

	plan, err := apply.Plan(ctx, docs, apply.Options{
		Stores: apply.NewStores(opts.Pool, opts.MasterKey),
		Force:  opts.ClearDirty,
	})
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	if _, err := apply.Execute(ctx, plan, nil); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	// A conflict is a row someone wrote between plan and write. The seed
	// leaves it alone, which is silent unless it is said out loud.
	for _, e := range plan.Entries {
		if e.Action == apply.ActionConflict {
			slog.Warn("seed: row changed during the run and was left alone",
				"kind", e.Kind, "name", e.Name)
		}
	}
	return tally(plan), nil
}

// splitSeedDocs separates what apply takes from what this path leaves out:
// Settings, which have their own loader (settings.SeedDir), and — for a
// catalog tree — the tenancy kinds. Both are returned as "Kind/name" for the
// caller to log.
func splitSeedDocs(docs []manifest.Document, catalogKindsOnly bool) (keep []manifest.Document, settings, tenancy []string) {
	keep = docs[:0]
	for _, d := range docs {
		switch {
		case d.Setting != nil:
			// A Setting carries no Payload, so its name isn't in docName.
			settings = append(settings, d.Kind()+"/"+d.Setting.Metadata.Name)
		case catalogKindsOnly && tenancyKinds[d.Kind()]:
			tenancy = append(tenancy, d.Kind()+"/"+docName(d))
		default:
			keep = append(keep, d)
		}
	}
	return keep, settings, tenancy
}

// docName is the metadata name of a document, for logs.
func docName(d manifest.Document) string {
	if m := d.Meta(); m != nil {
		return m.Name
	}
	return ""
}

func tally(p *apply.Result) *Result {
	res := &Result{}
	counters := map[string]*int{
		"Team": &res.Teams, "Project": &res.Projects, "Provider": &res.Providers,
		"Host": &res.Hosts, "RateLimit": &res.RateLimits, "HostKey": &res.HostKeys,
		"Model": &res.Models, "Pricing": &res.Pricings, "HostBinding": &res.HostBindings,
		"Policy": &res.Policies, "ServiceAccount": &res.ServiceAccounts,
		"Group": &res.Groups, "Role": &res.Roles, "RoleBinding": &res.RoleBindings,
		"PolicyBinding": &res.PolicyBindings, "Key": &res.Keys, "Overlay": &res.Overlays,
	}
	for _, e := range p.Entries {
		if e.Action == apply.ActionSkipDirty {
			res.Skipped++
			continue
		}
		if c := counters[e.Kind]; c != nil {
			*c++
		}
	}
	return res
}
