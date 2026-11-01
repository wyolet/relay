package apply

import (
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

// A key declared without an account keeps the one it has, default or not, so re-applying a document that omits it plans no change; a key moved to another owner or host leaves it, and a declared account wins.
func TestKeepAccount(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
	hostID, otherHost, held, declared := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	key := func(owner meta.Owner, host, accountID string) *hostkey.HostKey {
		return &hostkey.HostKey{Meta: meta.Metadata{Owner: owner}, Spec: hostkey.Spec{HostID: host, AccountID: accountID}}
	}
	for _, tc := range []struct {
		name string
		next *hostkey.HostKey
		want string
	}{
		{"omitted keeps the account", key(system, hostID, ""), held},
		{"declared account wins", key(system, hostID, declared), declared},
		{"another owner leaves it", key(team, hostID, ""), ""},
		{"another host leaves it", key(system, otherHost, ""), ""},
	} {
		keepAccount(key(system, hostID, held), tc.next)
		if tc.next.Spec.AccountID != tc.want {
			t.Errorf("%s: account = %q, want %q", tc.name, tc.next.Spec.AccountID, tc.want)
		}
	}
}
