//go:build integration

package pool_test

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// A pool's accounts keep their order through a rewrite, and a deleted account drops out of the pool.
func TestIntegration_PoolAccounts(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	q := gen.New(db)
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "pool-acct-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	accounts := account.NewStore(q)
	var ids []string
	for _, name := range []string{"org-a", "org-b"} {
		a := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
		if err := accounts.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert account: %v", err)
		}
		ids = append(ids, a.Meta.ID)
	}
	store := pool.NewStore(db)
	p := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "by-account", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: pool.Spec{HostID: hst.Meta.ID, AccountIDs: []string{ids[1], ids[0]}}}
	if err := store.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil || !slices.Equal(got.Spec.AccountIDs, p.Spec.AccountIDs) {
		t.Fatalf("Get = %+v, %v; want accounts %v", got, err, p.Spec.AccountIDs)
	}
	if list, err := store.List(ctx); err != nil || len(list) != 1 || !slices.Equal(list[0].Spec.AccountIDs, p.Spec.AccountIDs) {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := accounts.Delete(ctx, ids[1]); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if got, err := store.Get(ctx, p.Meta.ID); err != nil || got == nil || !slices.Equal(got.Spec.AccountIDs, []string{ids[0]}) {
		t.Fatalf("pool after an account was deleted = %+v, %v; want only %s", got, err, ids[0])
	}
}
