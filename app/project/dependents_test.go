package project

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

// A default account no key names goes with its project, so it is no dependent; a named account, or a default one a key still uses, is.
func TestDefaultAccountWithoutKeysIsNoDependent(t *testing.T) {
	id := meta.NewID()
	owned := meta.Owner{Kind: meta.OwnerProject, ID: id}
	acct := func(name string, def bool) *account.Account {
		return &account.Account{Meta: meta.Metadata{ID: name, Name: name, Owner: owned}, Spec: account.Spec{Default: def}}
	}
	rows := Rows{
		Accounts: []*account.Account{acct("keyless-default", true), acct("used-default", true), acct("named", false)},
		HostKeys: []*hostkey.HostKey{
			{Meta: meta.Metadata{ID: "k-1", Name: "elsewhere-key", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{AccountID: "used-default"}},
			{Meta: meta.Metadata{ID: "k-2", Name: "pruned-key", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{AccountID: "keyless-default"}},
		},
	}
	gone := func(kind, id string) bool { return kind == "HostKey" && id == "k-2" }
	named, hidden := rows.Dependents(id, gone, nil)
	want := []string{"Account/used-default", "Account/named"}
	if hidden != 0 || !slices.Equal(named, want) {
		t.Fatalf("dependents = %v (+%d hidden), want %v", named, hidden, want)
	}
}
