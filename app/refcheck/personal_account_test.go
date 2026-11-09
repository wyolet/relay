package refcheck

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// personalOffRows is bob's personal account and a provider account he owns, with bob's personal account turned off and alice's on.
func personalOffRows() Checker {
	personal := account.Personal("u-bob", "bob")
	personal.Meta.ID = "acct-bob"
	provider := &account.Account{Meta: meta.Metadata{ID: "acct-bob-org", Name: "bob-org", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: account.Spec{HostID: "h-1"}}
	member := account.Member("u-bob", "bob", teamOne, "one", "")
	member.Meta.ID, member.FollowsRoleBindings = "acct-bob-member", true
	rows := map[string]*account.Account{personal.Meta.ID: personal, provider.Meta.ID: provider, member.Meta.ID: member}
	return Checker{Rows: Lookup{
		Account:        func(_ context.Context, id string) *account.Account { return rows[id] },
		DefaultAccount: func(context.Context, key.Principal) (string, error) { return "acct-bob", nil },
		PersonalAccountOff: func(_ context.Context, id string) (bool, string) {
			return id == "u-bob", map[string]string{"u-bob": "bob"}[id]
		},
	}}
}

// While a user's personal account is off, a key of theirs must name a lane other than it, and their own provider accounts take no keys.
func TestKeyAccountWhilePersonalAccountIsOff(t *testing.T) {
	c := personalOffRows()
	ctx := context.Background()
	keyIn := func(accountID string) *key.Key {
		return &key.Key{Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: "u-bob"}, AccountID: accountID}}
	}
	for _, tc := range []struct {
		name, account, code string
		status              int
	}{
		{"no lane", "", CodeLaneRequired, http.StatusBadRequest},
		{"the personal lane", "acct-bob", CodePersonalAccountOff, http.StatusForbidden},
		{"a provider account he owns", "acct-bob-org", CodePersonalAccountOff, http.StatusForbidden},
		{"a member lane", "acct-bob-member", "", 0},
	} {
		err := c.KeyAccount(ctx, keyIn(tc.account))
		if status(err) != tc.status || code(err) != tc.code {
			t.Errorf("%s: %v, want %d %s", tc.name, err, tc.status, tc.code)
		}
	}
	if err := c.PersonalProviderAccount(ctx, &account.Account{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: account.Spec{HostID: "h-1"}}); code(err) != CodePersonalAccountOff || status(err) != http.StatusForbidden {
		t.Errorf("a new provider account of bob's = %v, want 403 %s", err, CodePersonalAccountOff)
	}
	if err := c.PersonalProviderAccount(ctx, &account.Account{Meta: meta.Metadata{Owner: alice}, Spec: account.Spec{HostID: "h-1"}}); err != nil {
		t.Errorf("a new provider account of alice's = %v, want it allowed", err)
	}
}

// A share with a user whose personal account is off would serve nothing, so it is refused with a pointer to their team or project.
func TestShareWithAUserWhosePersonalAccountIsOff(t *testing.T) {
	c := personalOffRows()
	src := &account.Account{Meta: meta.Metadata{ID: "acct-team", Name: "team-org", Owner: teamOne}, Spec: account.Spec{HostID: "h-1"}}
	share := account.Share(src, account.Receiver{Kind: account.ReceiverUser, ID: "u-bob"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}, "bob")
	err := c.newShare(context.Background(), src, share)
	if code(err) != CodePersonalAccountOff || status(err) != http.StatusForbidden {
		t.Fatalf("share with bob = %v, want 403 %s", err, CodePersonalAccountOff)
	}
}
