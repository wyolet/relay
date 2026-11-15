package control

import (
	"testing"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// A refusal names the accounts by kind and display name, at most namedInMessage of them, and counts the rest and the hidden ones.
func TestAccountsText(t *testing.T) {
	team := func(name string) policy.AccountRef {
		return policy.AccountRef{Name: "team-" + name, DisplayName: name, Kind: "team"}
	}
	for _, tc := range []struct {
		name string
		by   policy.EditBlockers
		want string
	}{
		{"one", policy.EditBlockers{Accounts: []policy.AccountRef{team("Research")}}, `team "Research"`},
		{"kinds", policy.EditBlockers{Accounts: []policy.AccountRef{
			{Name: "project-web", DisplayName: "Web", Kind: "project"},
			{Name: "sa-bot", Kind: "service"},
			{Name: "personal-alice", DisplayName: "alice", Kind: "personal"},
		}}, `project "Web", service account "sa-bot" and personal account "alice"`},
		{"hidden only", policy.EditBlockers{Hidden: 1}, "an account you cannot see"},
		{"named and hidden", policy.EditBlockers{Accounts: []policy.AccountRef{team("A")}, Hidden: 3}, `team "A" and 3 accounts you cannot see`},
		{"capped", policy.EditBlockers{Accounts: []policy.AccountRef{team("A"), team("B"), team("C"), team("D"), team("E"), team("F"), team("G")}, Hidden: 1},
			`team "A", team "B", team "C", team "D", team "E", 2 more and an account you cannot see`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountsText(tc.by); got != tc.want {
				t.Fatalf("accountsText = %q, want %q", got, tc.want)
			}
		})
	}
}
