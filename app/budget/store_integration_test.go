//go:build integration

// store_integration_test.go covers the Budget store against a real Postgres. Run with: make test-integration.

package budget_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

func TestIntegration_BudgetStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := budget.NewStore(gen.New(storagetest.Pool(t)))
	b := &budget.Budget{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-monthly", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: budget.Spec{AmountUSD: "2000.50", Period: budget.PeriodWeek, Mode: budget.ModeAlert},
	}
	if err := store.Upsert(ctx, b); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, b.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Spec != b.Spec || got.Meta.Owner != b.Meta.Owner {
		t.Fatalf("round trip = %+v owner %+v, want %+v owner %+v", got.Spec, got.Meta.Owner, b.Spec, b.Meta.Owner)
	}

	stale := *got
	got.Spec.AmountUSD = "3000"
	if err := store.Upsert(ctx, got); err != nil {
		t.Fatalf("conditional Upsert: %v", err)
	}
	if err := store.Upsert(ctx, &stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
		t.Fatalf("stale Upsert = %v, want ErrStaleResourceVersion", err)
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != 1 || list[0].Spec.AmountUSD != "3000" {
		t.Fatalf("List = %+v, %v; want the one updated budget", list, err)
	}
	if err := store.Delete(ctx, b.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := store.Get(ctx, b.Meta.ID); err != nil || got != nil {
		t.Fatalf("Get after delete = %v, %v; want nil, nil", got, err)
	}
}
