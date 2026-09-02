//go:build integration

// models_policyless_integration_test.go covers the policy-less listing against
// a real Postgres. The gate is settings.Inference.AllowMissingPolicy, and the
// settings cache is only reachable through Hydrate — there is no in-memory
// seam for it, so this is the only place the "switched on" half of the flag
// can be exercised.
// Run with: make test-integration.
package inference

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
	"github.com/wyolet/relay/pkg/slug"
)

func suffix() string {
	id := meta.NewID()
	return id[len(id)-8:]
}

// policylessCatalog seeds a catalog whose models differ in exactly the ways
// the policy-less pool cares about, then hydrates a Catalog over it with
// AllowMissingPolicy set to allow.
// policylessCatalog returns the hydrated catalog and the per-run name suffix
// every row it seeded carries. The database is shared and never reset, so the
// suffix is how a test tells its own rows from a previous run's.
func policylessCatalog(t *testing.T, allow bool) (*appcatalog.Catalog, string) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)

	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}

	sfx := suffix()
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-" + sfx, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Provider.Upsert(ctx, prov); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	// Four hosts, one per reason a model is or is not reachable without a
	// policy: a shared key, a key that belongs to a project, no key needed
	// at all, and a shared key whose tier grants a different model.
	mkHost := func(name string, noAuth bool) *host.Host {
		h := &host.Host{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-" + sfx, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: "https://" + name + ".example.com", NoAuth: noAuth},
		}
		if err := stores.Host.Upsert(ctx, h); err != nil {
			t.Fatalf("upsert host %s: %v", name, err)
		}
		return h
	}
	shared, owned := mkHost("shared", false), mkHost("owned", false)
	open, gated := mkHost("open", true), mkHost("gated", false)

	mkTier := func(h *host.Host, grants ...string) *policy.Policy {
		p := &policy.Policy{
			Meta: meta.Metadata{ID: meta.NewID(), Name: h.Meta.Name + "-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}},
			Spec: policy.Spec{Models: grants},
		}
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert tier: %v", err)
		}
		return p
	}
	sharedTier, ownedTier := mkTier(shared), mkTier(owned)
	// A tier naming a model this host does not serve: the key is shared and
	// healthy, and the gate is still what keeps its model off the listing.
	gatedTier := mkTier(gated, prov.Meta.Name+"/nothing-"+sfx)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "team-" + sfx, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "proj-" + sfx}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	proj.StampOwner()
	if err := stores.Project.Upsert(ctx, proj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	mkKey := func(name string, h *host.Host, tier *policy.Policy, owner meta.Owner) {
		k := &hostkey.HostKey{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-" + sfx, Owner: owner},
			Spec: hostkey.Spec{HostID: h.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "POLICYLESS_TEST_KEY"}},
		}
		if err := stores.HostKey.Upsert(ctx, k); err != nil {
			t.Fatalf("upsert host key %s: %v", name, err)
		}
	}
	t.Setenv("POLICYLESS_TEST_KEY", "sk-policyless")
	mkKey("shared-key", shared, sharedTier, meta.Owner{Kind: meta.OwnerSystem})
	mkKey("owned-key", owned, ownedTier, meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID})
	mkKey("gated-key", gated, gatedTier, meta.Owner{Kind: meta.OwnerSystem})

	mkModel := func(name string, h *host.Host) {
		md := &model.Model{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-" + sfx, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From(name + "-" + sfx)}}, Pointer: slug.From(name + "-" + sfx)},
		}
		if err := stores.Model.Upsert(ctx, md); err != nil {
			t.Fatalf("upsert model %s: %v", name, err)
		}
		b := &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-on-" + h.Meta.Name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: md.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI},
		}
		if err := stores.Binding.Upsert(ctx, b); err != nil {
			t.Fatalf("upsert binding %s: %v", name, err)
		}
	}
	mkModel("on-shared", shared)
	mkModel("on-owned", owned)
	mkModel("on-gated", gated)
	mkModel("on-open", open)

	raw, err := json.Marshal(settings.Inference{AllowMissingPolicy: allow})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if _, err := stores.Settings.Upsert(ctx, settings.SectionInference, raw); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, appcatalog.BootstrapOptions{Pool: pool}); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	return cat, sfx
}

// TestIntegration_ListModelsPolicylessMatchesResolve is the D73 agreement at
// the endpoint: with policy-less traffic allowed, /v1/models lists exactly the
// models the policy-less flow would serve — shared keys only, tier gate
// applied, a keyless host included.
func TestIntegration_ListModelsPolicylessMatchesResolve(t *testing.T) {
	cat, sfx := policylessCatalog(t, true)
	d := Deps{Catalog: cat, Resolver: routing.New(cat)}
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, &Principal{
		CredentialKind: CredentialKey, CredentialID: meta.NewID(), UserID: meta.NewID(),
	})

	out, err := listModels(ctx, d, "")
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	listed := make([]string, 0, len(out.Body.Data))
	for _, row := range out.Body.Data {
		if strings.HasSuffix(row.ID, sfx) {
			listed = append(listed, row.ID)
		}
	}
	sort.Strings(listed)

	// The flow is the reference: ask it for every model in the catalog.
	snap := cat.Current()
	served := []string{}
	for _, m := range snap.AllModels() {
		name := m.Spec.Snapshots[0].Name
		if !strings.HasSuffix(name, sfx) {
			continue
		}
		if _, err := d.Resolver.Resolve(routing.Request{ModelName: name, Snapshot: snap}); err == nil {
			served = append(served, name)
		}
	}
	sort.Strings(served)

	if len(listed) != 2 {
		t.Fatalf("listed %v, want the shared-key model and the keyless host's model", listed)
	}
	if len(listed) != len(served) {
		t.Fatalf("listed %v, but the policy-less flow serves %v", listed, served)
	}
	for i := range listed {
		if listed[i] != served[i] {
			t.Fatalf("listed %v, but the policy-less flow serves %v", listed, served)
		}
	}
	// Concretely: the shared-key model and the keyless host are in, the
	// project-owned key's model is not.
	joined := ""
	for _, id := range listed {
		joined += id + " "
	}
	for _, want := range []string{"on-shared", "on-open"} {
		if !containsPrefix(listed, want) {
			t.Errorf("%q is missing from the policy-less listing (%s)", want, joined)
		}
	}
	if containsPrefix(listed, "on-owned") {
		t.Errorf("a project-owned key's model is advertised to a policy-less caller (%s)", joined)
	}
	if containsPrefix(listed, "on-gated") {
		t.Errorf("a model the shared key's tier does not grant is advertised (%s)", joined)
	}
}

// With the flag off the endpoint refuses rather than listing a reduced set,
// matching the flow, which answers policyless_disabled.
func TestIntegration_ListModelsPolicylessRefusedWhenDisabled(t *testing.T) {
	cat, sfx := policylessCatalog(t, false)
	d := Deps{Catalog: cat, Resolver: routing.New(cat)}
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, &Principal{
		CredentialKind: CredentialKey, CredentialID: meta.NewID(), UserID: meta.NewID(),
	})
	if _, err := listModels(ctx, d, ""); err == nil {
		t.Fatal("listModels served a policy-less caller while the flag is off")
	}
	snap := cat.Current()
	for _, m := range snap.AllModels() {
		name := m.Spec.Snapshots[0].Name
		if !strings.HasSuffix(name, sfx) {
			continue
		}
		if _, err := d.Resolver.Resolve(routing.Request{ModelName: name, Snapshot: snap}); err == nil {
			t.Fatalf("model %q resolved policy-less while the flag is off", m.Meta.Name)
		}
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
