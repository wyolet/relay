package catalogview

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
)

// A pool that names only an account reaches that account's keys; a shared account the real account's, read-only even when the caller may not see them; an account with no keys reaches none.
func TestPolicyHosts_PoolAccounts(t *testing.T) {
	hostID := meta.NewID()
	alice := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	own, empty, real, share := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	ownKey, realKey := meta.NewID(), meta.NewID()
	poolOwn, poolShare, poolEmpty := meta.NewID(), meta.NewID(), meta.NewID()
	svc := &Service{
		Providers: fProviders{}, Models: fModels{}, Bindings: fBindings{}, Pricings: fPricings{}, RateLimits: fRLs{},
		Hosts: fHosts{{Meta: meta.Metadata{ID: hostID, Name: "h"}}},
		HostKeys: fHostKeys{
			{Meta: meta.Metadata{ID: ownKey, Name: "own-key", Owner: alice}, Spec: hostkey.Spec{HostID: hostID, AccountID: own}},
			{Meta: meta.Metadata{ID: realKey, Name: "bobs-key", Owner: bob}, Spec: hostkey.Spec{HostID: hostID, AccountID: real}},
		},
		Accounts: fAccounts{
			{Meta: meta.Metadata{ID: own, Name: "own", Owner: alice}, Spec: account.Spec{HostID: hostID}},
			{Meta: meta.Metadata{ID: empty, Name: "empty", Owner: alice}, Spec: account.Spec{HostID: hostID}},
			{Meta: meta.Metadata{ID: real, Name: "bobs", Owner: bob}, Spec: account.Spec{HostID: hostID}},
			{Meta: meta.Metadata{ID: share, Name: "bobs-for-alice", Owner: alice}, Spec: account.Spec{HostID: hostID, SourceAccountID: real}},
		},
		Pools: fPools{
			{Meta: meta.Metadata{ID: poolOwn, Name: "p-own", Owner: alice}, Spec: pool.Spec{HostID: hostID, AccountIDs: []string{own}}},
			{Meta: meta.Metadata{ID: poolShare, Name: "p-share", Owner: alice}, Spec: pool.Spec{HostID: hostID, AccountIDs: []string{share}}},
			{Meta: meta.Metadata{ID: poolEmpty, Name: "p-empty", Owner: alice}, Spec: pool.Spec{HostID: hostID, AccountIDs: []string{empty}}},
		},
		Policies: fPolicies{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "via-own", Owner: alice}, Spec: policy.Spec{PoolIDs: []string{poolOwn}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "via-share", Owner: alice}, Spec: policy.Spec{PoolIDs: []string{poolShare}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "via-empty", Owner: alice}, Spec: policy.Spec{PoolIDs: []string{poolEmpty}}},
		},
		Visible: asAlice,
	}
	for _, tc := range []struct {
		policy, key string
		readOnly    bool
	}{
		{"via-own", "own-key", false},
		{"via-share", "bobs-key", true},
		{"via-empty", "", false},
	} {
		_, rows, err := svc.PolicyHosts(context.Background(), tc.policy)
		if err != nil {
			t.Fatalf("%s: %v", tc.policy, err)
		}
		if len(rows) != 1 || rows[0].Host.ID != hostID {
			t.Fatalf("%s: rows = %+v, want the pool's host", tc.policy, rows)
		}
		keys := rows[0].HostKeys
		if tc.key == "" {
			if len(keys) != 0 {
				t.Errorf("%s: keys = %+v, want none", tc.policy, keys)
			}
			continue
		}
		if len(keys) != 1 || keys[0].Name != tc.key || keys[0].ReadOnly != tc.readOnly {
			t.Errorf("%s: keys = %+v, want %s readOnly=%v", tc.policy, keys, tc.key, tc.readOnly)
		}
	}
}
