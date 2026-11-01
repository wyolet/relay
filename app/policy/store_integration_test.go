//go:build integration

// store_integration_test.go covers the Policy store against a real Postgres:
// Upsert fans a single row out across `policies` and the link junctions in
// one transaction, and the read path reassembles it. There is no fake seam
// for that — the relational split is the behaviour.
// Run with: make test-integration.

package policy_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// storeFixture is the row graph a policy needs before it can link anything:
// a provider and host, a host key, and two rate limits.
type storeFixture struct {
	store          *policy.Store
	db             storage.DB
	hostID         string
	hostKeyID      string
	rateLimitID    string
	otherRateLimit string
}

func setupPolicyStore(t *testing.T) (storeFixture, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool := storagetest.Pool(t)
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
	f := storeFixture{store: policy.NewStore(pool), db: pool, hostID: hst.Meta.ID}
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
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "store-key-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "STORE_TEST_KEY"}},
	}
	if err := hostkey.NewStore(pool, nil, nil).Upsert(ctx, hk); err != nil {
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

// A Policy's model grant lives in a column and its links in junctions, so a
// round trip through the store has to reassemble both — and a rewrite has to
// replace them rather than accumulate.
func TestIntegration_PolicyStoreRoundTrip(t *testing.T) {
	f, ctx := setupPolicyStore(t)

	p := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "store-policy-" + short(), DisplayName: "Store Policy", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{
			Models:                []string{"acme/m", "acme"},
			CredentialIDs:         []string{f.hostKeyID},
			RateLimits:            []policy.RateLimitLink{{RateLimitID: f.rateLimitID}},
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
	if len(got.Spec.CredentialIDs) != 1 || got.Spec.CredentialIDs[0] != f.hostKeyID {
		t.Errorf("credentialIds = %v, want [%s]", got.Spec.CredentialIDs, f.hostKeyID)
	}
	if len(got.Spec.RateLimits) != 1 || got.Spec.RateLimits[0].RateLimitID != f.rateLimitID {
		t.Errorf("rateLimits = %+v", got.Spec.RateLimits)
	}
	if len(got.Spec.Models) != 2 || !got.Spec.PayloadLoggingEnabled {
		t.Errorf("spec = %+v", got.Spec)
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
	if len(listed.Spec.CredentialIDs) != 1 || len(listed.Spec.RateLimits) != 1 || len(listed.Spec.Models) != 2 {
		t.Errorf("List hydration = %+v, want the same as Get %+v", listed.Spec, got.Spec)
	}

	// Rewriting with other links replaces the junction rows.
	p.Spec.Models = []string{"acme/m"}
	p.Spec.CredentialIDs = nil
	p.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: f.otherRateLimit, Models: []string{"acme/m"}}}
	if err := f.store.Upsert(ctx, p); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got, err = f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after rewrite: %v", err)
	}
	if len(got.Spec.Models) != 1 || len(got.Spec.CredentialIDs) != 0 {
		t.Errorf("spec = %+v, want one model and no credentials", got.Spec)
	}
	if len(got.Spec.RateLimits) != 1 || got.Spec.RateLimits[0].RateLimitID != f.otherRateLimit {
		t.Errorf("rateLimits = %+v, want only the scoped link", got.Spec.RateLimits)
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
