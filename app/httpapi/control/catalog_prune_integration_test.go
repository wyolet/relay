//go:build integration

// catalog_prune_integration_test.go runs the automatic prune after a
// versioned reseed against Postgres, with the reference scans the delete
// handler uses: what holds a row is decided by the real stores, including
// the accounts and pools a host delete would cascade to.

package control

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalogprune"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
)

// pruneBase is what both releases ship.
const pruneBase = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Provider
metadata: {name: acme}
spec: {homepageURL: "https://acme.test"}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata: {name: acme}
spec: {baseURL: "https://api.acme.test"}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Model
metadata: {name: acme-chat, owner: {kind: provider, name: acme}}
spec: {snapshots: [{name: acme-chat}], pointer: acme-chat}
`

// pruneDropped is what the second release drops: two tier rate limits, an
// idle model with its binding and pricing, a busy model with its binding, a
// whole provider with its model, and a host.
const pruneDropped = `---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RateLimit
metadata: {name: acme-tier-a-rl, owner: {kind: host, name: acme}}
spec:
  enabled: true
  rules: [{meter: requests, amount: 500, window: 1m, strategy: sliding-window}]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RateLimit
metadata: {name: acme-tier-b-rl, owner: {kind: host, name: acme}}
spec:
  enabled: true
  rules: [{meter: requests, amount: 900, window: 1m, strategy: sliding-window}]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Model
metadata: {name: acme-old, owner: {kind: provider, name: acme}}
spec: {snapshots: [{name: acme-old}], pointer: acme-old}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostBinding
metadata: {name: acme-old-on-acme}
spec: {model: acme-old, host: acme, adapter: openai, enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Pricing
metadata: {name: acme-acme-old, owner: {kind: host, name: acme}}
spec:
  currency: USD
  targetModels: [acme-old]
  rates: [{meter: tokens.input, unit: per_million, amount: 1}]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Model
metadata: {name: acme-busy, owner: {kind: provider, name: acme}}
spec: {snapshots: [{name: acme-busy}], pointer: acme-busy}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostBinding
metadata: {name: acme-busy-on-acme}
spec: {model: acme-busy, host: acme, adapter: openai, enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Provider
metadata: {name: old-vendor}
spec: {homepageURL: "https://old-vendor.test"}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Model
metadata: {name: old-vendor-x, owner: {kind: provider, name: old-vendor}}
spec: {snapshots: [{name: old-vendor-x}], pointer: old-vendor-x}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostBinding
metadata: {name: old-vendor-x-on-acme}
spec: {model: old-vendor-x, host: acme, adapter: openai, enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata: {name: gone}
spec: {baseURL: "https://api.gone.test"}
`

// busyUsage answers that one model served a request an hour ago.
type busyUsage struct{ modelID string }

func (u *busyUsage) Events(_ context.Context, q usage.EventQuery) ([]usage.Event, error) {
	if slices.Contains(q.ModelID, u.modelID) {
		return []usage.Event{{ModelID: u.modelID, Timestamp: time.Now().Add(-time.Hour)}}, nil
	}
	return nil, nil
}

func writeRelease(t *testing.T, version, docs string) string {
	t.Helper()
	dir := t.TempDir()
	must(t, "write tree", os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(docs), 0o644))
	must(t, "write version", os.WriteFile(filepath.Join(dir, ".version"), []byte(version+"\n"), 0o644))
	return dir
}

func pruneBootOptions(pg *pgxpool.Pool, dir, version string, u catalogprune.UsageReader) appcatalog.BootstrapOptions {
	return appcatalog.BootstrapOptions{
		Pool: pg, AutoSeedDir: dir, CatalogVersion: version,
		CatalogURL: "http://127.0.0.1:1/{version}.tar.gz",
		SystemRateLimits: []*ratelimit.RateLimit{{
			Meta: meta.Metadata{Name: "control-api", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: "requests", Amount: 10, Window: ratelimit.Window(time.Second), Strategy: "sliding-window",
			}}},
		}},
		Prune: appcatalog.PruneOptions{Auto: true, Idle: 30 * 24 * time.Hour, References: CatalogReferences, Usage: u},
	}
}

func bootRelease(t *testing.T, ctx context.Context, opts appcatalog.BootstrapOptions) *appcatalog.Stores {
	t.Helper()
	cat, stores, err := appcatalog.BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate %s: %v", opts.CatalogVersion, err)
	}
	return stores
}

// catalogRows lists every row of the pruned kinds as "kind/name", detached
// ones marked with a trailing "*".
func catalogRows(t *testing.T, ctx context.Context, s *appcatalog.Stores) []string {
	t.Helper()
	var out []string
	add := func(kind string, m meta.Metadata) {
		if m.Owner.Kind == meta.OwnerSystem && kind == "rate-limit" {
			return // the relay's own rate limits
		}
		name := kind + "/" + m.Name
		if m.CatalogDetachedAt != nil {
			name += "*"
		}
		out = append(out, name)
	}
	provs, err := s.Provider.List(ctx)
	must(t, "list providers", err)
	for _, x := range provs {
		add("provider", x.Meta)
	}
	hosts, err := s.Host.List(ctx)
	must(t, "list hosts", err)
	for _, x := range hosts {
		add("host", x.Meta)
	}
	models, err := s.Model.List(ctx)
	must(t, "list models", err)
	for _, x := range models {
		add("model", x.Meta)
	}
	bindings, err := s.Binding.List(ctx)
	must(t, "list bindings", err)
	for _, x := range bindings {
		add("host-binding", x.Meta)
	}
	pricings, err := s.Pricing.List(ctx)
	must(t, "list pricings", err)
	for _, x := range pricings {
		add("pricing", x.Meta)
	}
	rls, err := s.RateLimit.List(ctx)
	must(t, "list rate limits", err)
	for _, x := range rls {
		add("rate-limit", x.Meta)
	}
	slices.Sort(out)
	return out
}

func lookupID[T any](t *testing.T, ctx context.Context, list func(context.Context) ([]*T, error), metaOf func(*T) meta.Metadata, name string) string {
	t.Helper()
	rows, err := list(ctx)
	must(t, "list", err)
	for _, r := range rows {
		if m := metaOf(r); m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("no row named %q", name)
	return ""
}

// A release drops a batch of rows. The prune after the reseed deletes the
// ones nothing holds, children before parents, and keeps the rate limit a
// team's policy links, the model and binding with recent traffic, and the
// host an account and a pool sit on, which a host delete would cascade to.
// Pruning again changes nothing.
func TestIntegration_CatalogPruneAfterReseed(t *testing.T) {
	pg, ctx := setupPolicyRefDB(t)
	u := &busyUsage{}
	stores := bootRelease(t, ctx, pruneBootOptions(pg, writeRelease(t, "v1.0.0-test", pruneBase+pruneDropped), "v1.0.0-test", u))
	u.modelID = lookupID(t, ctx, stores.Model.List, func(m *model.Model) meta.Metadata { return m.Meta }, "acme-busy")

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert team", stores.Team.Upsert(ctx, tm))
	rlA := lookupID(t, ctx, stores.RateLimit.List, func(r *ratelimit.RateLimit) meta.Metadata { return r.Meta }, "acme-tier-a-rl")
	must(t, "upsert team policy", stores.Policy.Upsert(ctx, &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "team-default", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rlA}}},
	}))
	gone := lookupID(t, ctx, stores.Host.List, func(h *host.Host) meta.Metadata { return h.Meta }, "gone")
	must(t, "upsert account", stores.Account.Upsert(ctx, &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gone-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: gone}}))
	must(t, "upsert pool", stores.Pool.Upsert(ctx, &pool.Pool{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gone-pool", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: pool.Spec{HostID: gone}}))

	v2 := pruneBootOptions(pg, writeRelease(t, "v2.0.0-test", pruneBase), "v2.0.0-test", u)
	stores = bootRelease(t, ctx, v2)

	want := []string{
		"host-binding/acme-busy-on-acme*", "host/acme", "host/gone*",
		"model/acme-busy*", "model/acme-chat", "provider/acme", "rate-limit/acme-tier-a-rl*",
	}
	if diff := cmp.Diff(want, catalogRows(t, ctx, stores)); diff != "" {
		t.Fatalf("rows after the prune (-want +got):\n%s", diff)
	}
	row, err := stores.Settings.Get(ctx, settings.SectionCatalogSource)
	must(t, "read catalog-source", err)
	src, _ := row.Value.(*settings.CatalogSource)
	if src == nil || src.Version != "v2.0.0-test" || src.Prune == nil || *src.Prune != (settings.CatalogPrune{Pruned: 7, KeptDetached: 4}) {
		t.Fatalf("catalog-source = %+v, want v2.0.0-test with 7 pruned and 4 kept", src)
	}
	acct, err := stores.Account.List(ctx)
	must(t, "list accounts", err)
	if !slices.ContainsFunc(acct, func(a *account.Account) bool { return a.Meta.Name == "gone-org" }) {
		t.Fatal("the account on the kept host is gone")
	}

	rep, err := appcatalog.PruneDetached(ctx, stores, v2, 0, true)
	must(t, "prune again", err)
	got := map[string]string{}
	for _, d := range rep.Rows {
		got[d.Kind+"/"+d.Name] = d.Decision + ": " + d.Reason
	}
	traffic := func(r string) bool { return strings.HasPrefix(r, "keep: traffic since ") }
	if rep.Pruned != 0 || rep.Kept != 4 || !traffic(got["host-binding/acme-busy-on-acme"]) || !traffic(got["model/acme-busy"]) ||
		got["rate-limit/acme-tier-a-rl"] != "keep: referenced by policy team-default" ||
		got["host/gone"] != "keep: referenced by account gone-org, pool gone-pool" {
		t.Fatalf("second prune = %+v\n%v", rep, got)
	}
	if diff := cmp.Diff(want, catalogRows(t, ctx, stores)); diff != "" {
		t.Fatalf("rows after the second prune (-want +got):\n%s", diff)
	}
}
