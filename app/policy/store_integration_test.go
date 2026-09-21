//go:build integration

// store_integration_test.go covers the Policy store against a real Postgres:
// Upsert fans a single row out across `policies`, `policy_models` and
// `policy_host_keys` in one transaction, and the read path reassembles it.
// There is no fake seam for that — the relational split is the behaviour.
// Run with: make test-integration.
package policy_test

import (
	"context"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/internal/storage/gen"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

// storeFixture is the row graph a policy needs before it can reference
// anything: a provider and host, two models, a host key, and a rate limit.
type storeFixture struct {
	store          *policy.Store
	models         []string
	hostKeyID      string
	rateLimitID    string
	otherRateLimit string
}

func setupPolicyStore(t *testing.T) (storeFixture, context.Context) {
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

	q := gen.New(pool)
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "store-prov-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := provider.NewStore(q).Upsert(ctx, prov); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "store-host-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	f := storeFixture{store: policy.NewStore(pool)}
	ms := model.NewStore(q)
	for i := 0; i < 2; i++ {
		md := &model.Model{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "store-model-" + short(), Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "s" + short()}}},
		}
		if err := ms.Upsert(ctx, md); err != nil {
			t.Fatalf("upsert model %d: %v", i, err)
		}
		f.models = append(f.models, md.Meta.ID)
	}
	rls := ratelimit.NewStore(q)
	for i, id := range []*string{&f.rateLimitID, &f.otherRateLimit} {
		rl := &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "store-rl-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: ratelimit.MeterRequests, Amount: int64(10 * (i + 1)),
				Window: ratelimit.Window(60_000_000_000), Strategy: ratelimit.StrategyFixedWindow,
			}}},
		}
		if err := rls.Upsert(ctx, rl); err != nil {
			t.Fatalf("upsert rate limit %d: %v", i, err)
		}
		*id = rl.Meta.ID
	}
	// A host key needs a host-owned tier policy of its own.
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "store-tier-" + short(), Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	if err := f.store.Upsert(ctx, tier); err != nil {
		t.Fatalf("upsert tier policy: %v", err)
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "store-key-" + short(), Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "STORE_TEST_KEY"}},
	}
	if err := hostkey.NewStore(q, nil, nil).Upsert(ctx, hk); err != nil {
		t.Fatalf("upsert host key: %v", err)
	}
	f.hostKeyID = hk.Meta.ID
	return f, ctx
}

// short is a per-row name suffix. It reads the tail of a UUIDv7, not the
// head: the leading bytes are a millisecond timestamp, so rows minted in one
// tick would share a prefix and collide on the unique name index.
func short() string {
	id := meta.NewID()
	return id[len(id)-8:]
}

// A Policy's membership lists live in junction tables and its rate limit in
// a column, so a round trip through the store has to reassemble all three —
// and a rewrite has to replace them rather than accumulate.
func TestIntegration_PolicyStoreRoundTrip(t *testing.T) {
	f, ctx := setupPolicyStore(t)

	p := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "store-policy-" + short(), DisplayName: "Store Policy", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{
			Models:                []string{"acme/m", "acme"},
			ModelIDs:              f.models,
			HostKeyIDs:            []string{f.hostKeyID},
			RateLimitID:           f.rateLimitID,
			KeySelection:          policy.KeySelectionRoundRobin,
			PayloadLoggingEnabled: true,
		},
	}
	if err := f.store.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Meta.DisplayName != "Store Policy" || got.Meta.Owner.Kind != meta.OwnerUser {
		t.Errorf("identity round trip = %+v", got.Meta)
	}
	if len(got.Spec.ModelIDs) != len(f.models) {
		t.Errorf("modelIds = %v, want %v", got.Spec.ModelIDs, f.models)
	}
	if len(got.Spec.HostKeyIDs) != 1 || got.Spec.HostKeyIDs[0] != f.hostKeyID {
		t.Errorf("hostKeyIds = %v, want [%s]", got.Spec.HostKeyIDs, f.hostKeyID)
	}
	if got.Spec.RateLimitID != f.rateLimitID {
		t.Errorf("rateLimitId = %q, want %q", got.Spec.RateLimitID, f.rateLimitID)
	}
	if len(got.Spec.Models) != 2 {
		t.Errorf("models = %v, want the two grants", got.Spec.Models)
	}
	if got.Spec.KeySelection != policy.KeySelectionRoundRobin || !got.Spec.PayloadLoggingEnabled {
		t.Errorf("spec knobs = %+v", got.Spec)
	}
	// The stored row must satisfy its own Validate, or the next PUT of it
	// fails on a value the store itself produced.
	if err := got.Validate(); err != nil {
		t.Errorf("the row read back does not validate: %v", err)
	}

	// List sees the same row, hydrated the same way.
	rows, err := f.store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listed *policy.Policy
	for _, r := range rows {
		if r.Meta.ID == p.Meta.ID {
			listed = r
		}
	}
	if listed == nil {
		t.Fatal("the policy is missing from List")
	}
	if len(listed.Spec.ModelIDs) != len(got.Spec.ModelIDs) || len(listed.Spec.HostKeyIDs) != len(got.Spec.HostKeyIDs) {
		t.Errorf("List hydration = %+v, want the same as Get %+v", listed.Spec, got.Spec)
	}
	if listed.Spec.RateLimitID != f.rateLimitID {
		t.Errorf("List rateLimitId = %q, want %q", listed.Spec.RateLimitID, f.rateLimitID)
	}

	// Rewriting with narrower membership replaces the junction rows.
	p.Spec.ModelIDs = f.models[:1]
	p.Spec.HostKeyIDs = nil
	p.Spec.RateLimitID = ""
	p.Spec.RLBindings = []policy.RLBinding{{Models: []string{"acme/m"}, RateLimitID: f.otherRateLimit}}
	if err := f.store.Upsert(ctx, p); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got, err = f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after rewrite: %v", err)
	}
	if len(got.Spec.ModelIDs) != 1 || got.Spec.ModelIDs[0] != f.models[0] {
		t.Errorf("modelIds = %v, want only the first model", got.Spec.ModelIDs)
	}
	if len(got.Spec.HostKeyIDs) != 0 {
		t.Errorf("hostKeyIds = %v, want none", got.Spec.HostKeyIDs)
	}
	if got.Spec.RateLimitID != "" {
		t.Errorf("rateLimitId = %q, want it cleared", got.Spec.RateLimitID)
	}
	if len(got.Spec.RLBindings) != 1 || got.Spec.RLBindings[0].RateLimitID != f.otherRateLimit {
		t.Errorf("rlBindings = %+v, want the per-model binding", got.Spec.RLBindings)
	}

	if err := f.store.Delete(ctx, p.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := f.store.Get(ctx, p.Meta.ID); err != nil || got != nil {
		t.Fatalf("Get after Delete = (%v, %v), want (nil, nil)", got, err)
	}
}

// A miss is not an error: callers distinguish "no such policy" from "the
// store is broken" by the nil row, not by an error string.
func TestIntegration_PolicyStoreGetMissIsNotAnError(t *testing.T) {
	f, ctx := setupPolicyStore(t)
	got, err := f.store.Get(ctx, meta.NewID())
	if err != nil {
		t.Fatalf("Get on a missing id: %v", err)
	}
	if got != nil {
		t.Fatalf("row = %+v, want nil", got)
	}
}
