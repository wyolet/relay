package project

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// Every provider account the project owns is a dependent, whether or not a key names it: nothing makes one implicitly, so none goes with its project unasked.
func TestProviderAccountsAreDependents(t *testing.T) {
	id := meta.NewID()
	owned := meta.Owner{Kind: meta.OwnerProject, ID: id}
	acct := func(name string, grouped bool) *account.Account {
		return &account.Account{Meta: meta.Metadata{ID: name, Name: name, Owner: owned}, Spec: account.Spec{HostID: "h-1", GroupedByRelay: grouped}}
	}
	rows := Rows{
		Accounts: []*account.Account{acct("keyless-grouped", true), acct("used", false)},
		HostKeys: []*hostkey.HostKey{
			{Meta: meta.Metadata{ID: "k-1", Name: "elsewhere-key", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{AccountID: "used"}},
		},
	}
	named, hidden := rows.Dependents(id, nil, nil)
	want := []string{"Account/keyless-grouped", "Account/used"}
	if hidden != 0 || !slices.Equal(named, want) {
		t.Fatalf("dependents = %v (+%d hidden), want %v", named, hidden, want)
	}
}

// The project's caller accounts go with it, so they are no dependents; a key running in one of them would go too, so it is.
func TestCallerAccountsGoWithTheirProject(t *testing.T) {
	id, user := meta.NewID(), meta.NewID()
	scope := meta.Owner{Kind: meta.OwnerProject, ID: id}
	own := account.ForProject(id, "ml", "")
	own.Meta.ID = "a-project"
	member := account.Member(user, "alice", scope, "ml", "")
	member.Meta.ID = "a-member"
	rows := Rows{
		Accounts: []*account.Account{own, member},
		Keys: []*key.Key{{Meta: meta.Metadata{ID: "k-1", Name: "alice-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: user}},
			Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: user}, AccountID: "a-member"}}},
	}
	named, hidden := rows.Dependents(id, nil, nil)
	if hidden != 0 || !slices.Equal(named, []string{"Key/alice-key"}) {
		t.Fatalf("dependents = %v (+%d hidden), want only the key running in the member account", named, hidden)
	}
}
