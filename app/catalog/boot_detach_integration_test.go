//go:build integration

package catalog

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/pkg/ids"
)

// detachBase is what every release of the test catalog ships: the provider,
// the host, one model and a dirty-to-be tier the operator edits.
const detachBase = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata: {name: acme}
spec: {homepageURL: "https://acme.test"}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata: {name: acme}
spec: {baseURL: "https://api.acme.test"}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata: {name: acme-chat, owner: {kind: provider, name: acme}}
spec: {snapshots: [{name: acme-chat}], pointer: acme-chat}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: acme-tier-4, owner: {kind: host, name: acme}}
spec: {}
`

// detachDroppedDocs is the part of the first release later releases drop: a tier
// policy with its rate limit, a second and a third tier, and a model with its
// binding and pricing.
const detachDroppedDocs = `---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata: {name: acme-tier-1-rl, owner: {kind: host, name: acme}}
spec:
  enabled: true
  rules: [{meter: requests, amount: 500, window: 1m, strategy: sliding-window}]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: acme-tier-1, owner: {kind: host, name: acme}}
spec: {rateLimit: acme-tier-1-rl}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata: {name: acme-old, owner: {kind: provider, name: acme}}
spec: {snapshots: [{name: acme-old}], pointer: acme-old}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: HostBinding
metadata: {name: acme-old-on-acme}
spec: {model: acme-old, host: acme, adapter: openai, enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Pricing
metadata: {name: acme-acme-old, owner: {kind: host, name: acme}}
spec:
  currency: USD
  targetModels: [acme-old]
  rates: [{meter: tokens.input, unit: per_million, amount: 1}]
`

func tierDoc(name, displayName string) string {
	return `---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: ` + name + `, displayName: "` + displayName + `", owner: {kind: host, name: acme}}
spec: {}
`
}

// operatorDocs are rows the operator seeds themselves (relay seed): a host
// key on the first tier, and a team's policy.
func operatorDocs(env string) string {
	return `apiVersion: relay.wyolet.dev/v1alpha2
kind: HostKey
metadata: {name: acme-key}
spec:
  hostId: acme
  policyId: acme-tier-1
  valueFrom: {kind: env, env: ` + env + `}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata: {name: platform, owner: {kind: user}}
spec: {enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata: {name: search}
spec: {team: platform}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: search-default, owner: {kind: project, name: search}}
spec: {}
`
}

// writeTree writes docs as a catalog tree stamped with version.
func writeTree(t *testing.T, version, docs string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(docs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".version"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hydrateVersion boots the catalog pinned to version, seeded from dir with no
// network.
func hydrateVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dir, version string) (*Catalog, *Stores) {
	t.Helper()
	opts := BootstrapOptions{
		Pool: pool, AutoSeedDir: dir, CatalogVersion: version,
		CatalogURL: "http://127.0.0.1:1/{version}.tar.gz",
		SystemRateLimits: []*ratelimit.RateLimit{{
			Meta: meta.Metadata{Name: "control-api", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: "requests", Amount: 10, Window: ratelimit.Window(time.Second), Strategy: "sliding-window",
			}}},
		}},
	}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate %s: %v", version, err)
	}
	return cat, stores
}

// detachedRows lists every catalog-kind row carrying the marker as
// "Kind/name", with the mark by key.
func detachedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]time.Time {
	t.Helper()
	rows, err := apply.Load(ctx, apply.NewStores(pool, nil))
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	out := map[string]time.Time{}
	add := func(kind string, m meta.Metadata) {
		if m.CatalogDetachedAt != nil {
			out[kind+"/"+m.Name] = *m.CatalogDetachedAt
		}
	}
	for _, x := range rows.Providers {
		add("Provider", x.Meta)
	}
	for _, x := range rows.Hosts {
		add("Host", x.Meta)
	}
	for _, x := range rows.RateLimits {
		add("RateLimit", x.Meta)
	}
	for _, x := range rows.HostKeys {
		add("HostKey", x.Meta)
	}
	for _, x := range rows.Models {
		add("Model", x.Meta)
	}
	for _, x := range rows.Pricings {
		add("Pricing", x.Meta)
	}
	for _, x := range rows.Bindings {
		add("HostBinding", x.Meta)
	}
	for _, x := range rows.Policies {
		add("Policy", x.Meta)
	}
	return out
}

func keysOf(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func policyNamed(t *testing.T, ctx context.Context, stores *Stores, name string) *policy.Policy {
	t.Helper()
	all, err := stores.Policy.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Meta.Name == name {
			return p
		}
	}
	t.Fatalf("policy %q not found", name)
	return nil
}

func markDirty(t *testing.T, ctx context.Context, stores *Stores, name string) {
	t.Helper()
	p := policyNamed(t, ctx, stores, name)
	p.Meta.Dirty = true
	p.Meta.DisplayName = "edited by the operator"
	if err := stores.Policy.Upsert(ctx, p); err != nil {
		t.Fatalf("mark %s dirty: %v", name, err)
	}
}

func TestIntegration_SeedVersioned_DetachesWhatAReleaseDrops(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	env := "DETACH_TEST_KEY_" + strings.ToUpper(strings.ReplaceAll(ids.New(), "-", ""))[:16]
	t.Setenv(env, "sk-detach-test")

	v1 := detachBase + detachDroppedDocs + tierDoc("acme-tier-2", "Tier 2") + tierDoc("acme-tier-3", "Tier 3")
	_, stores := hydrateVersion(t, ctx, pool, writeTree(t, "v1.0.0-test", v1), "v1.0.0-test")
	if got := detachedRows(t, ctx, pool); len(got) != 0 {
		t.Fatalf("first boot detached %v", keysOf(got))
	}

	// The operator's own rows: a host key on tier 1, a team's policy, and a
	// host created through the API with its own tier.
	opDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(opDir, "operator.yaml"), []byte(operatorDocs(env)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: opDir}); err != nil {
		t.Fatalf("operator seed: %v", err)
	}
	byo := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "byo"}, Spec: host.Spec{BaseURL: "https://byo.test"}}
	if err := stores.Host.Upsert(ctx, byo); err != nil {
		t.Fatal(err)
	}
	byoTier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "byo-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: byo.Meta.ID}}}
	if err := stores.Policy.Upsert(ctx, byoTier); err != nil {
		t.Fatal(err)
	}
	markDirty(t, ctx, stores, "acme-tier-3")
	markDirty(t, ctx, stores, "acme-tier-4")

	// The second release drops tier 1 with its rate limit, tiers 2 and 3,
	// and the old model.
	v2Dir := writeTree(t, "v2.0.0-test", detachBase)
	cat, stores := hydrateVersion(t, ctx, pool, v2Dir, "v2.0.0-test")
	want := []string{
		"HostBinding/acme-old-on-acme", "Model/acme-old", "Policy/acme-tier-1", "Policy/acme-tier-2",
		"Policy/acme-tier-3", "Pricing/acme-acme-old", "RateLimit/acme-tier-1-rl",
	}
	detached := detachedRows(t, ctx, pool)
	if got := keysOf(detached); !slices.Equal(got, want) {
		t.Fatalf("detached = %v\nwant       %v", got, want)
	}
	if got := catalogSourceVersion(t, stores, ctx); got != "v2.0.0-test" {
		t.Fatalf("marker = %q, want v2.0.0-test", got)
	}
	if p := policyNamed(t, ctx, stores, "acme-tier-3"); !p.Meta.Dirty {
		t.Fatal("detaching cleared the dirty flag on tier 3")
	}

	// The host key keeps its tier: the detached policy and its rate limit
	// still serve.
	keys, err := stores.HostKey.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("host keys: %v (%d)", err, len(keys))
	}
	snap := cat.Current()
	hk, ok := snap.HostKey(keys[0].Meta.ID)
	if !ok {
		t.Fatal("the host key dropped out of the snapshot")
	}
	tier, ok := snap.Policy(hk.Spec.PolicyID)
	if !ok || tier.Meta.Name != "acme-tier-1" || tier.Meta.CatalogDetachedAt == nil {
		t.Fatalf("host key tier = %+v (found %v), want the detached acme-tier-1", tier, ok)
	}
	if rl := snap.RateLimitOfPolicy(tier.Meta.ID); rl == nil || rl.Meta.Name != "acme-tier-1-rl" {
		t.Fatalf("tier rate limit = %+v, want acme-tier-1-rl", rl)
	}

	// Reseeding the same tree changes nothing.
	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: v2Dir, CatalogKindsOnly: true, DetachDropped: true})
	if err != nil {
		t.Fatalf("second reseed: %v", err)
	}
	if res.Detached != 0 || res.Reattached != 0 {
		t.Fatalf("second reseed detached %d, reattached %d; want 0", res.Detached, res.Reattached)
	}
	again := detachedRows(t, ctx, pool)
	for k, at := range detached {
		if !again[k].Equal(at) {
			t.Errorf("%s: marker moved from %v to %v", k, at, again[k])
		}
	}

	// The third release brings tiers 2 and 3 back. Tier 2 is clean: it is
	// re-attached and updated. Tier 3 is dirty: it stays the operator's.
	v3 := detachBase + tierDoc("acme-tier-2", "Tier 2 (new)") + tierDoc("acme-tier-3", "Tier 3 (new)")
	_, stores = hydrateVersion(t, ctx, pool, writeTree(t, "v3.0.0-test", v3), "v3.0.0-test")
	want = []string{
		"HostBinding/acme-old-on-acme", "Model/acme-old", "Policy/acme-tier-1",
		"Policy/acme-tier-3", "Pricing/acme-acme-old", "RateLimit/acme-tier-1-rl",
	}
	if got := keysOf(detachedRows(t, ctx, pool)); !slices.Equal(got, want) {
		t.Fatalf("after v3 detached = %v\nwant               %v", got, want)
	}
	if p := policyNamed(t, ctx, stores, "acme-tier-2"); p.Meta.DisplayName != "Tier 2 (new)" {
		t.Fatalf("re-attached tier 2 display name = %q, want the release's", p.Meta.DisplayName)
	}
	if p := policyNamed(t, ctx, stores, "acme-tier-3"); !p.Meta.Dirty || p.Meta.DisplayName != "edited by the operator" {
		t.Fatalf("dirty tier 3 = %+v, want the operator's copy left alone", p.Meta)
	}
}

// A release that cannot be fetched, parsed or planned detaches nothing and
// leaves the marker on the old version. Each tree below declares none of the
// seeded rows, so a detach step that ran would mark every one of them.
func TestIntegration_SeedVersioned_FailedReleaseDetachesNothing(t *testing.T) {
	v1Dir := writeTree(t, "v1.0.0-test", detachBase+detachDroppedDocs)
	parseError := archiveServer(t, map[string]string{
		"providers/broken/provider.yaml": "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata: [unclosed\n",
	})
	planError := archiveServer(t, map[string]string{
		"hosts/acme/policies/tier-9.yaml": `apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: acme-tier-9, owner: {kind: host, name: acme}}
spec: {rateLimit: no-such-rate-limit}
`,
	})
	cases := []struct{ name, url string }{
		{"fetch fails", "http://127.0.0.1:1/{version}.tar.gz"},
		{"parse fails", parseError.URL + "/{version}.tar.gz"},
		{"plan fails", planError.URL + "/{version}.tar.gz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx, cancel := setupDB(t)
			defer cancel()
			hydrateVersion(t, ctx, pool, v1Dir, "v1.0.0-test")

			opts := BootstrapOptions{Pool: pool, AutoSeedDir: v1Dir, CatalogVersion: "v2.0.0-test", CatalogURL: c.url}
			cat, stores, err := BootstrapStores(ctx, opts)
			if err != nil {
				t.Fatalf("BootstrapStores: %v", err)
			}
			if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
			if got := detachedRows(t, ctx, pool); len(got) != 0 {
				t.Fatalf("detached %v", keysOf(got))
			}
			if got := catalogSourceVersion(t, stores, ctx); got != "v1.0.0-test" {
				t.Fatalf("marker = %q, want v1.0.0-test", got)
			}
		})
	}
}
