package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

type keyRows struct{ rows []*key.Key }

func (k *keyRows) List(context.Context) ([]*key.Key, error) { return k.rows, nil }
func (k *keyRows) Upsert(context.Context, *key.Key) error   { return nil }

// Pruning a policy runs the same detach the control API's delete
// cascade runs — without it, prune left keys and accounts pointing at a row
// that no longer exists.
func TestPrunedPolicyIsDetachedBeforeDeletion(t *testing.T) {
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "k"},
		Spec: key.Spec{PolicyID: "pol-1"},
	}
	deleted := ""
	del := detachThenDelete(
		policy.DetachStores{Keys: &keyRows{rows: []*key.Key{k}}},
		func(_ context.Context, id string) error { deleted = id; return nil },
	)
	if err := del(context.Background(), "pol-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if k.Spec.PolicyID != "" {
		t.Fatalf("key still points at the pruned policy %q", k.Spec.PolicyID)
	}
	if deleted != "pol-1" {
		t.Fatalf("deleted = %q, want pol-1", deleted)
	}
}

type hostKeyRows struct{ rows []*hostkey.HostKey }

func (h *hostKeyRows) List(context.Context) ([]*hostkey.HostKey, error) { return h.rows, nil }

// D76/D81: prune refuses a policy host keys mirror as their tier, the same
// way the control API's delete does. HostKey.policyId is required, so there
// is no valid row to leave behind — the error names the keys to reattach.
func TestPrunedPolicyRefusedWhileHostKeysUseTheTier(t *testing.T) {
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-key"},
		Spec: hostkey.Spec{PolicyID: "pol-1"},
	}
	deleted := ""
	del := detachThenDelete(
		policy.DetachStores{HostKeys: &hostKeyRows{rows: []*hostkey.HostKey{hk}}},
		func(_ context.Context, id string) error { deleted = id; return nil },
	)
	err := del(context.Background(), "pol-1")
	if err == nil {
		t.Fatal("prune removed a policy host keys still mirror")
	}
	if !strings.Contains(err.Error(), "upstream-key") {
		t.Fatalf("err = %v, want it to name the host key to reattach", err)
	}
	if deleted != "" {
		t.Fatalf("deleted %q despite the refusal", deleted)
	}
	if hk.Spec.PolicyID != "pol-1" {
		t.Fatalf("host key tier policy = %q, want it untouched", hk.Spec.PolicyID)
	}
}
