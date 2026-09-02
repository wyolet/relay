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
	// Settings share the config tree but have their own loader
	// (settings.SeedDir); apply refuses them rather than dropping them.
	catalogDocs := docs[:0]
	for _, d := range docs {
		if d.Setting == nil {
			catalogDocs = append(catalogDocs, d)
		}
	}
	docs = catalogDocs

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
	return tally(plan), nil
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
