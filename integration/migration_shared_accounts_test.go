//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// Rolling back below 000040 deletes the shares an older release cannot route and keeps the real account, the pool, which loses the accounts it listed, and a key whose lane was a share, which falls back to its principal's account; upgrading again adds the columns and the junction back, and a share writes as before.
func TestMigrateDownAndUpAcrossSharedAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := st.Pool()
	q := gen.New(db)
	system := meta.Owner{Kind: meta.OwnerSystem}
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: system}}
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	org := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: system}, Spec: account.Spec{HostID: hst.Meta.ID}}
	teamOwner := meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}
	share := account.Share(org, account.Receiver{Kind: account.ReceiverTeam, ID: tm.Meta.ID}, teamOwner, tm.Meta.Name)
	accounts := account.NewStore(q)
	pl := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform-pool", Owner: teamOwner}, Spec: pool.Spec{HostID: hst.Meta.ID}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	if err := accounts.Upsert(ctx, org); err != nil {
		t.Fatalf("upsert real account: %v", err)
	}
	if err := accounts.CreateShare(ctx, share); err != nil {
		t.Fatalf("share: %v", err)
	}
	pl.Spec.AccountIDs = []string{share.Meta.ID, org.Meta.ID}
	if err := pool.NewStore(db).Upsert(ctx, pl); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	bob := &user.User{ID: meta.NewID(), Username: "bob"}
	if err := user.NewStore(db).Upsert(ctx, bob); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	toBob := account.Share(org, account.Receiver{Kind: account.ReceiverUser, ID: bob.ID}, meta.Owner{Kind: meta.OwnerUser, ID: bob.ID}, "bob")
	if err := accounts.CreateShare(ctx, toBob); err != nil {
		t.Fatalf("share with bob: %v", err)
	}
	lane := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "bob-lane", Owner: meta.Owner{Kind: meta.OwnerUser, ID: bob.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: bob.ID}, AccountID: toBob.Meta.ID, KeyHash: strings.Repeat("ef", 32)}}
	if err := key.NewStore(q).Upsert(ctx, lane); err != nil {
		t.Fatalf("upsert key: %v", err)
	}
	st.Close()

	if err := storagemod.MigrateTo(dsn, 39); err != nil {
		t.Fatalf("migrate down to 39: %v", err)
	}
	conn := testPool(t, dsn)
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE id = ANY($1)", []string{org.Meta.ID, share.Meta.ID}).Scan(&n); err != nil || n != 1 {
		t.Fatalf("accounts at 39 = %d (err %v), want the real account alone", n, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pools WHERE id = $1", pl.Meta.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pool at 39: %d (err %v)", n, err)
	}
	// A key whose lane was a share stays, in its principal's own account.
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM relay_keys WHERE id = $1 AND account_id IS NULL", lane.Meta.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("bob's key at 39: %d (err %v), want it kept with no account", n, err)
	}
	for _, probe := range []string{
		"SELECT count(*) FROM information_schema.columns WHERE table_name = 'accounts' AND column_name IN ('source_account_id', 'share_receiver')",
		"SELECT count(*) FROM information_schema.tables WHERE table_name = 'pool_accounts'",
	} {
		if err := conn.QueryRow(ctx, probe).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s at 39 = %d (err %v), want none", probe, n, err)
		}
	}

	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	st, err = storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open at head: %v", err)
	}
	defer st.Close()
	q = gen.New(st.Pool())
	again := account.Share(org, account.Receiver{Kind: account.ReceiverTeam, ID: tm.Meta.ID}, teamOwner, tm.Meta.Name)
	if err := account.NewStore(q).CreateShare(ctx, again); err != nil {
		t.Fatalf("share at head again: %v", err)
	}
	got, err := pool.NewStore(st.Pool()).Get(ctx, pl.Meta.ID)
	if err != nil || got == nil || len(got.Spec.AccountIDs) != 0 {
		t.Fatalf("pool at head = %+v (err %v), want it back without accounts", got, err)
	}
	got.Spec.AccountIDs = []string{again.Meta.ID}
	if err := pool.NewStore(st.Pool()).Upsert(ctx, got); err != nil {
		t.Fatalf("pool accounts at head: %v", err)
	}
	if got, err := pool.NewStore(st.Pool()).Get(ctx, pl.Meta.ID); err != nil || !slices.Equal(got.Spec.AccountIDs, []string{again.Meta.ID}) {
		t.Fatalf("pool accounts after the round trip = %+v (err %v)", got, err)
	}
}
