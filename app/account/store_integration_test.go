//go:build integration

// store_integration_test.go covers the Account store against a real Postgres: the host lives in a column, so the round trip and the host-delete cascade are the behaviour. Run with: make test-integration.

package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

func TestIntegration_AccountStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := gen.New(storagetest.Pool(t))
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acct-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	store := account.NewStore(q)
	a := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: account.Spec{HostID: hst.Meta.ID, OrgID: "org-abc123"},
	}
	if err := store.Upsert(ctx, a); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, a.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Spec != a.Spec || got.Meta.Owner != a.Meta.Owner || got.Meta.Name != a.Meta.Name {
		t.Fatalf("round trip = %+v %+v, want %+v %+v", got.Meta, got.Spec, a.Meta, a.Spec)
	}

	// A write carrying a version the row no longer has is refused.
	stale := *got
	got.Spec.OrgID = "org-def456"
	if err := store.Upsert(ctx, got); err != nil {
		t.Fatalf("conditional Upsert: %v", err)
	}
	if err := store.Upsert(ctx, &stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
		t.Fatalf("stale Upsert = %v, want ErrStaleResourceVersion", err)
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != 1 || list[0].Spec.OrgID != "org-def456" {
		t.Fatalf("List = %+v, %v; want the one updated account", list, err)
	}

	if err := host.NewStore(q).Delete(ctx, hst.Meta.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	if got, err := store.Get(ctx, a.Meta.ID); err != nil || got != nil {
		t.Fatalf("Get after host delete = %v, %v; want the account gone", got, err)
	}
}
