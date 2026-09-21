package policy

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/serviceaccount"
)

// One helper clears every clearable reference to a policy — including the
// ServiceAccount override the API cascade used to miss. A host key's tier
// policy is NOT one of them: policyId is required, so clearing it would write
// a row that fails its own Validate. Removal is refused instead (D76, D81).
func TestDetach_ClearsEveryClearableReference(t *testing.T) {
	k := &key.Key{}
	k.Meta.Name, k.Spec.PolicyID = "k1", "pol-1"
	sa := &serviceaccount.ServiceAccount{}
	sa.Meta.Name, sa.Spec.PolicyID = "sa1", "pol-1"
	hk := &hostkey.HostKey{}
	hk.Meta.Name, hk.Spec.PolicyID = "hk1", "pol-1"
	h := &host.Host{}
	h.Meta.Name, h.Spec.DefaultPolicy = "h1", "pol-1"
	h.Spec.Policies = []string{"pol-1", "pol-2"}

	st := DetachStores{
		Keys:            &fakeKeys{rows: []*key.Key{k}},
		ServiceAccounts: &fakeSAs{rows: []*serviceaccount.ServiceAccount{sa}},
		HostKeys:        &fakeHostKeys{rows: []*hostkey.HostKey{hk}},
		Hosts:           &fakeHosts{rows: []*host.Host{h}},
	}
	if err := Detach(context.Background(), st, "pol-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if k.Spec.PolicyID != "" || sa.Spec.PolicyID != "" {
		t.Fatalf("refs left: key=%q sa=%q", k.Spec.PolicyID, sa.Spec.PolicyID)
	}
	if hk.Spec.PolicyID != "pol-1" {
		t.Fatalf("host-key tier policy = %q, want it untouched: clearing it writes an invalid row", hk.Spec.PolicyID)
	}
	if h.Spec.DefaultPolicy != "" || len(h.Spec.Policies) != 1 || h.Spec.Policies[0] != "pol-2" {
		t.Fatalf("host refs left: default=%q policies=%v", h.Spec.DefaultPolicy, h.Spec.Policies)
	}
}

// D76: the names a refused delete has to report.
func TestHostKeysUsingPolicy_NamesTheTierKeys(t *testing.T) {
	a := &hostkey.HostKey{}
	a.Meta.Name, a.Spec.PolicyID = "tier-a", "pol-1"
	b := &hostkey.HostKey{}
	b.Meta.Name, b.Spec.PolicyID = "other", "pol-2"

	names, err := HostKeysUsingPolicy(context.Background(),
		DetachStores{HostKeys: &fakeHostKeys{rows: []*hostkey.HostKey{a, b}}}, "pol-1")
	if err != nil {
		t.Fatalf("HostKeysUsingPolicy: %v", err)
	}
	if len(names) != 1 || names[0] != "tier-a" {
		t.Fatalf("names = %v, want [tier-a]", names)
	}
}

type fakeKeys struct{ rows []*key.Key }

func (f *fakeKeys) List(context.Context) ([]*key.Key, error) { return f.rows, nil }
func (f *fakeKeys) Upsert(context.Context, *key.Key) error   { return nil }

type fakeSAs struct {
	rows []*serviceaccount.ServiceAccount
}

func (f *fakeSAs) List(context.Context) ([]*serviceaccount.ServiceAccount, error) { return f.rows, nil }
func (f *fakeSAs) Upsert(context.Context, *serviceaccount.ServiceAccount) error   { return nil }

type fakeHostKeys struct{ rows []*hostkey.HostKey }

func (f *fakeHostKeys) List(context.Context) ([]*hostkey.HostKey, error) { return f.rows, nil }

type fakeHosts struct{ rows []*host.Host }

func (f *fakeHosts) List(context.Context) ([]*host.Host, error) { return f.rows, nil }
func (f *fakeHosts) Upsert(context.Context, *host.Host) error   { return nil }
