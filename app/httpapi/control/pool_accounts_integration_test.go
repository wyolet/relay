//go:build integration

package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/ids"
)

// A pool naming only an account reaches its keys on every read: the policy's hosts, the key's policies and the pool list's account_id filter. Through a share the keys are read-only, and the sharer is not told the receiver's policies.
func TestPoolAccounts_ReachKeysOnReads(t *testing.T) {
	const secret = "sk-share-0123456789abcdefghijkl"
	t.Setenv("SHARE_TEST_KEY", secret)
	w := newSharesWorld(t)
	ctx := t.Context()
	toBob := decodeAccount(t, w.share("alice", w.alicesAcct, account.ReceiverUser, w.users["bob"]))
	alice := meta.Owner{Kind: meta.OwnerUser, ID: w.users["alice"]}
	bob := meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}
	for _, x := range []struct {
		owner             meta.Owner
		pool, pol, acctID string
	}{
		{alice, "alice-pool", "alice-pol", w.alicesAcct.Meta.ID},
		{bob, "bob-pool", "bob-pol", toBob.Meta.ID},
	} {
		p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: x.pool, Owner: x.owner}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{x.acctID}}}
		must(t, "pool", w.stores.Pool.Upsert(ctx, p))
		must(t, "policy", w.stores.Policy.Upsert(ctx, &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: x.pol, Owner: x.owner}, Spec: policy.Spec{PoolIDs: []string{p.Meta.ID}}}))
	}

	rec := w.send("alice", http.MethodGet, "/policies/alice-pol/hosts", "")
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name":"alice-key"`) || strings.Contains(body, `"readOnly"`) {
		t.Fatalf("alice's policy hosts = %s, want her account's key", body)
	}
	rec = w.send("bob", http.MethodGet, "/policies/bob-pol/hosts", "")
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name":"alice-key"`) || !strings.Contains(body, `"readOnly":true`) || strings.Contains(body, "sk-share") || strings.Contains(body, "SHARE_TEST_KEY") {
		t.Fatalf("bob's policy hosts = %s, want the shared key read-only, no secret", body)
	}

	rec = w.send("alice", http.MethodGet, "/host-keys/"+w.aliceKey.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name":"alice-pol"`) || strings.Contains(body, "bob-pol") {
		t.Fatalf("alice's key policies = %s, want alice-pol only", body)
	}
	rec = w.send("root", http.MethodGet, "/host-keys/"+w.aliceKey.Meta.ID, "")
	if body := rec.Body.String(); !strings.Contains(body, `"name":"alice-pol"`) || !strings.Contains(body, `"name":"bob-pol"`) {
		t.Fatalf("the admin's view of the key's policies = %s, want both", body)
	}

	rec = w.send("root", http.MethodGet, "/pools?account_id="+toBob.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"name":"bob-pool"`) || strings.Contains(body, "alice-pool") {
		t.Fatalf("pools drawing on bob's share = %s, want bob-pool only", body)
	}
}
