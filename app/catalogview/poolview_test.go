package catalogview

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/pool"
)

type fAccounts []*account.Account

func (f fAccounts) List(context.Context) ([]*account.Account, error) { return f, nil }

// A pool lists its keys in pool order, and an account the keys naming it;
// rows the caller may not see drop out, and an unseen pool is a 404.
func TestPoolAndAccountKeys(t *testing.T) {
	hostID, acct := meta.NewID(), meta.NewID()
	first, second, hidden := meta.NewID(), meta.NewID(), meta.NewID()
	mine := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	other := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	svc := &Service{
		HostKeys: fHostKeys{
			{Meta: meta.Metadata{ID: second, Name: "b-key", Owner: mine}, Spec: hostkey.Spec{HostID: hostID, AccountID: acct}},
			{Meta: meta.Metadata{ID: first, Name: "z-key", Owner: mine}, Spec: hostkey.Spec{HostID: hostID, AccountID: acct}},
			{Meta: meta.Metadata{ID: hidden, Name: "hidden-key", Owner: other}, Spec: hostkey.Spec{HostID: hostID, AccountID: acct}},
		},
		Pools: fPools{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "mine", Owner: mine}, Spec: pool.Spec{HostID: hostID, CredentialIDs: []string{first, hidden, second}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "theirs", Owner: other}, Spec: pool.Spec{HostID: hostID}},
		},
		Accounts: fAccounts{{Meta: meta.Metadata{ID: acct, Name: "org", Owner: mine}, Spec: account.Spec{HostID: hostID}}},
		Visible:  func(_, _ string, owner meta.Owner) bool { return owner == mine },
	}
	ctx := context.Background()

	ref, rows, err := svc.PoolKeys(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if ref.HostID != hostID || len(rows) != 2 || rows[0].Name != "z-key" || rows[1].Name != "b-key" {
		t.Fatalf("pool keys = %+v %+v, want z-key then b-key", ref, rows)
	}
	if _, _, err := svc.PoolKeys(ctx, "theirs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unseen pool err = %v, want ErrNotFound", err)
	}

	_, rows, err = svc.AccountKeys(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "b-key" || rows[1].Name != "z-key" {
		t.Fatalf("account keys = %+v, want b-key and z-key by name", rows)
	}
}
