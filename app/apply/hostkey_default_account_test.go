package apply

import (
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

// A key declared without an account keeps the default account its owner's earlier save joined, so re-applying the same document plans no change; any other account it held is reported as dropped.
func TestKeepDefaultAccount(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	hostID := meta.NewID()
	def := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Owner: system}, Spec: account.Spec{HostID: hostID, Default: true}}
	plain := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Owner: system}, Spec: account.Spec{HostID: hostID}}
	other := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Owner: meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}}, Spec: account.Spec{HostID: hostID, Default: true}}
	accounts := []*account.Account{def, plain, other}
	key := func(accountID string) *hostkey.HostKey {
		return &hostkey.HostKey{Meta: meta.Metadata{Owner: system}, Spec: hostkey.Spec{HostID: hostID, AccountID: accountID}}
	}
	for _, tc := range []struct {
		name       string
		prev, next string
		want       string
	}{
		{"default kept", def.Meta.ID, "", def.Meta.ID},
		{"non-default dropped", plain.Meta.ID, "", ""},
		{"another owner's default dropped", other.Meta.ID, "", ""},
		{"declared account wins", def.Meta.ID, plain.Meta.ID, plain.Meta.ID},
		{"unknown account dropped", meta.NewID(), "", ""},
	} {
		next := key(tc.next)
		keepDefaultAccount(accounts, key(tc.prev), next)
		if next.Spec.AccountID != tc.want {
			t.Errorf("%s: account = %q, want %q", tc.name, next.Spec.AccountID, tc.want)
		}
	}
}
