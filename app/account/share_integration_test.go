//go:build integration

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

// A share round-trips with its real account and receiver, takes a free name, is refused a second time for one receiver, and goes with its real account.
func TestIntegration_ShareStore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(storagetest.Pool(t))
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "share-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	store := account.NewStore(q)
	src := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
	if err := store.Upsert(ctx, src); err != nil {
		t.Fatalf("upsert real account: %v", err)
	}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
	taken := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "anthropic-for-platform", Owner: team}, Spec: account.Spec{HostID: hst.Meta.ID}}
	if err := store.Upsert(ctx, taken); err != nil {
		t.Fatalf("upsert account holding the name: %v", err)
	}

	share := account.Share(src, account.Receiver{Kind: account.ReceiverTeam, ID: team.ID}, team, "platform")
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	got, err := store.Get(ctx, share.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.Meta.Name != "anthropic-for-platform-2" || got.Spec.SourceAccountID != src.Meta.ID || got.Meta.Owner != team || got.Spec.Kind != account.KindProvider {
		t.Fatalf("share = %+v %+v", got.Meta, got.Spec)
	}

	again := account.Share(src, account.Receiver{Kind: account.ReceiverTeam, ID: team.ID}, team, "platform")
	if err := store.CreateShare(ctx, again); !errors.Is(err, account.ErrShareTaken) {
		t.Fatalf("second share = %v, want ErrShareTaken", err)
	}

	proj, sa := meta.NewID(), meta.NewID()
	held := account.Share(src, account.Receiver{Kind: account.ReceiverServiceAccount, ID: sa}, meta.Owner{Kind: meta.OwnerProject, ID: proj}, "bot")
	// A holder names a real service account; this one does not exist, so the foreign key refuses it.
	if err := store.CreateShare(ctx, held); err == nil {
		t.Fatal("a share held by a missing service account was stored")
	}

	if err := store.Delete(ctx, src.Meta.ID); err != nil {
		t.Fatalf("delete real account: %v", err)
	}
	if got, err := store.Get(ctx, share.Meta.ID); err != nil || got != nil {
		t.Fatalf("share after its real account went = %+v, %v", got, err)
	}
}
