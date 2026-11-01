//go:build integration

// store_integration_test.go covers the Account store against a real Postgres: the host lives in a column, so the round trip and the host-delete cascade are the behaviour. Run with: make test-integration.

package account_test

import (
	"context"
	"errors"
	"sync"
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

// An owner's default account at a host is created once, named after the host, and found again; another owner gets its own under a suffixed name.
func TestIntegration_EnsureDefault(t *testing.T) {
	ctx := context.Background()
	q := gen.New(storagetest.Pool(t))
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai", DisplayName: "OpenAI", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	store := account.NewStore(q)
	system := meta.Owner{Kind: meta.OwnerSystem}
	alice := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}

	first, err := store.EnsureDefault(ctx, system, hst.Meta.ID)
	if err != nil || first == "" {
		t.Fatalf("EnsureDefault(system) = %q, %v", first, err)
	}
	again, err := store.EnsureDefault(ctx, system, hst.Meta.ID)
	if err != nil || again != first {
		t.Fatalf("second EnsureDefault(system) = %q, %v; want %q", again, err, first)
	}
	got, err := store.Get(ctx, first)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Meta.Name != "openai" || got.Meta.DisplayName != "OpenAI" || got.Meta.Owner != system || !got.Spec.Default || got.Spec.HostID != hst.Meta.ID {
		t.Fatalf("default account = %+v %+v", got.Meta, got.Spec)
	}

	other, err := store.EnsureDefault(ctx, alice, hst.Meta.ID)
	if err != nil || other == "" || other == first {
		t.Fatalf("EnsureDefault(alice) = %q, %v; want a second account", other, err)
	}
	if got, _ := store.Get(ctx, other); got == nil || got.Meta.Name != "openai-2" || got.Meta.Owner != alice || !got.Spec.Default {
		t.Fatalf("alice's default account = %+v", got)
	}

	// A second account marked default for the same owner and host is refused.
	dup := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-second", Owner: system},
		Spec: account.Spec{HostID: hst.Meta.ID, Default: true},
	}
	if err := store.Upsert(ctx, dup); !errors.Is(err, account.ErrDefaultTaken) {
		t.Fatalf("second default Upsert = %v, want ErrDefaultTaken", err)
	}
	dup.Spec.Default = false
	if err := store.Upsert(ctx, dup); err != nil {
		t.Fatalf("plain account Upsert: %v", err)
	}

	if id, err := store.EnsureDefault(ctx, system, meta.NewID()); err != nil || id != "" {
		t.Fatalf("EnsureDefault at a missing host = %q, %v; want none", id, err)
	}
}

// Concurrent first saves for one owner and host converge on one default account.
func TestIntegration_EnsureDefaultConcurrent(t *testing.T) {
	ctx := context.Background()
	q := gen.New(storagetest.Pool(t))
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	store := account.NewStore(q)
	owner := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ids[i], errs[i] = store.EnsureDefault(ctx, owner, hst.Meta.ID) })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("save %d = %q, %v; want %q", i, ids[i], errs[i], ids[0])
		}
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d accounts, %v; want one", len(list), err)
	}
}
