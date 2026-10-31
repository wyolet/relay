//go:build integration

// store_integration_test.go covers the Pool store against a real Postgres: the key list lives in a junction, so order, replacement and the delete cascades are the behaviour. Run with: make test-integration.

package pool_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

func TestIntegration_PoolStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	q := gen.New(db)
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pool-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	keys := hostkey.NewStore(db, reg, nil)
	var keyIDs []string
	for _, name := range []string{"key-a", "key-b", "key-c"} {
		k := &hostkey.HostKey{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
		}
		if err := keys.Upsert(ctx, k); err != nil {
			t.Fatalf("upsert host key %s: %v", name, err)
		}
		keyIDs = append(keyIDs, k.Meta.ID)
	}

	store := pool.NewStore(db)
	p := &pool.Pool{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-main", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: pool.Spec{HostID: hst.Meta.ID, CredentialIDs: []string{keyIDs[2], keyIDs[0]}, Selection: keypool.KeySelectionRoundRobin},
	}
	if err := store.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Spec.HostID != hst.Meta.ID || got.Spec.Selection != keypool.KeySelectionRoundRobin || !slices.Equal(got.Spec.CredentialIDs, p.Spec.CredentialIDs) {
		t.Fatalf("round trip = %+v, want %+v", got.Spec, p.Spec)
	}

	// A rewrite replaces the key list rather than adding to it.
	got.Spec.CredentialIDs = []string{keyIDs[1]}
	if err := store.Upsert(ctx, got); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Spec.CredentialIDs, []string{keyIDs[1]}) {
		t.Fatalf("List after rewrite = %+v, %v; want only %s", list, err, keyIDs[1])
	}

	stale := *p
	stale.Meta.ResourceVersion = "1"
	if err := store.Upsert(ctx, &stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
		t.Fatalf("stale Upsert = %v, want ErrStaleResourceVersion", err)
	}

	// Deleting a key drops it from the pool; the pool stays.
	if err := keys.Delete(ctx, keyIDs[1]); err != nil {
		t.Fatalf("delete host key: %v", err)
	}
	if got, err := store.Get(ctx, p.Meta.ID); err != nil || got == nil || len(got.Spec.CredentialIDs) != 0 {
		t.Fatalf("pool after its key was deleted = %+v, %v; want an empty pool", got, err)
	}

	if err := host.NewStore(q).Delete(ctx, hst.Meta.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	if got, err := store.Get(ctx, p.Meta.ID); err != nil || got != nil {
		t.Fatalf("Get after host delete = %v, %v; want the pool gone", got, err)
	}
}
