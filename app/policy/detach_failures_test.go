package policy

// detach_failures_test.go covers the paths the happy-path detach test does
// not: a caller that wired only some stores, rows that name a different
// policy, and a store that fails mid-way. Both writers that can remove a
// policy share this helper, so a partial failure has to be reported rather
// than leaving the caller believing the cascade ran.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/serviceaccount"
)

var errStore = errors.New("store is down")

type failingKeys struct {
	rows      []*key.Key
	listErr   bool
	upsertErr bool
}

func (f *failingKeys) List(context.Context) ([]*key.Key, error) {
	if f.listErr {
		return nil, errStore
	}
	return f.rows, nil
}

func (f *failingKeys) Upsert(context.Context, *key.Key) error {
	if f.upsertErr {
		return errStore
	}
	return nil
}

type failingSAs struct {
	rows      []*serviceaccount.ServiceAccount
	listErr   bool
	upsertErr bool
}

func (f *failingSAs) List(context.Context) ([]*serviceaccount.ServiceAccount, error) {
	if f.listErr {
		return nil, errStore
	}
	return f.rows, nil
}

func (f *failingSAs) Upsert(context.Context, *serviceaccount.ServiceAccount) error {
	if f.upsertErr {
		return errStore
	}
	return nil
}

type failingHostKeys struct {
	rows    []*hostkey.HostKey
	listErr bool
}

func (f *failingHostKeys) List(context.Context) ([]*hostkey.HostKey, error) {
	if f.listErr {
		return nil, errStore
	}
	return f.rows, nil
}

type failingHosts struct {
	rows      []*host.Host
	listErr   bool
	upsertErr bool
}

func (f *failingHosts) List(context.Context) ([]*host.Host, error) {
	if f.listErr {
		return nil, errStore
	}
	return f.rows, nil
}

func (f *failingHosts) Upsert(context.Context, *host.Host) error {
	if f.upsertErr {
		return errStore
	}
	return nil
}

// A nil store is a kind the caller has not wired, not an error, and an empty
// id is a no-op: neither may turn a delete cascade into a failure.
func TestDetach_UnwiredStoresAndEmptyID(t *testing.T) {
	if err := Detach(context.Background(), DetachStores{}, "pol-1"); err != nil {
		t.Fatalf("Detach with no stores wired: %v", err)
	}
	k := &key.Key{}
	k.Spec.PolicyID = "pol-1"
	st := DetachStores{Keys: &failingKeys{rows: []*key.Key{k}}}
	if err := Detach(context.Background(), st, ""); err != nil {
		t.Fatalf("Detach with an empty id: %v", err)
	}
	if k.Spec.PolicyID != "pol-1" {
		t.Fatalf("an empty id cleared %q", k.Spec.PolicyID)
	}

	names, err := HostKeysUsingPolicy(context.Background(), DetachStores{}, "pol-1")
	if err != nil || names != nil {
		t.Fatalf("HostKeysUsingPolicy with no store = (%v, %v), want (nil, nil)", names, err)
	}
	names, err = HostKeysUsingPolicy(context.Background(),
		DetachStores{HostKeys: &failingHostKeys{}}, "")
	if err != nil || names != nil {
		t.Fatalf("HostKeysUsingPolicy with an empty id = (%v, %v), want (nil, nil)", names, err)
	}
}

// A row naming a different policy is left alone; a host that names none is
// not rewritten at all.
func TestDetach_LeavesUnrelatedRowsAlone(t *testing.T) {
	other := &key.Key{}
	other.Meta.Name, other.Spec.PolicyID = "other-key", "pol-2"
	otherSA := &serviceaccount.ServiceAccount{}
	otherSA.Meta.Name, otherSA.Spec.PolicyID = "other-sa", "pol-2"
	untouched := &host.Host{}
	untouched.Meta.Name, untouched.Spec.DefaultPolicy = "untouched", "pol-2"
	untouched.Spec.Policies = []string{"pol-2"}
	// A host whose whole list is the policy loses the list rather than
	// keeping an empty one.
	emptied := &host.Host{}
	emptied.Meta.Name = "emptied"
	emptied.Spec.Policies = []string{"pol-1"}

	st := DetachStores{
		Keys:            &failingKeys{rows: []*key.Key{other}},
		ServiceAccounts: &failingSAs{rows: []*serviceaccount.ServiceAccount{otherSA}},
		Hosts:           &failingHosts{rows: []*host.Host{untouched, emptied}},
	}
	if err := Detach(context.Background(), st, "pol-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if other.Spec.PolicyID != "pol-2" || otherSA.Spec.PolicyID != "pol-2" {
		t.Errorf("unrelated rows changed: key=%q sa=%q", other.Spec.PolicyID, otherSA.Spec.PolicyID)
	}
	if untouched.Spec.DefaultPolicy != "pol-2" || len(untouched.Spec.Policies) != 1 {
		t.Errorf("unrelated host changed: %+v", untouched.Spec)
	}
	if emptied.Spec.Policies != nil {
		t.Errorf("policies = %v, want nil rather than an empty list", emptied.Spec.Policies)
	}
}

// Every store failure is reported and names the kind and row it happened on,
// so a half-applied cascade is visible instead of silent.
func TestDetach_ReportsStoreFailures(t *testing.T) {
	k := &key.Key{}
	k.Meta.Name, k.Spec.PolicyID = "k1", "pol-1"
	sa := &serviceaccount.ServiceAccount{}
	sa.Meta.Name, sa.Spec.PolicyID = "sa1", "pol-1"
	h := &host.Host{}
	h.Meta.Name, h.Spec.DefaultPolicy = "h1", "pol-1"

	for _, tc := range []struct {
		name  string
		st    DetachStores
		wants string
	}{
		{
			name:  "listing keys",
			st:    DetachStores{Keys: &failingKeys{listErr: true}},
			wants: "list keys",
		},
		{
			name:  "writing a key",
			st:    DetachStores{Keys: &failingKeys{rows: []*key.Key{k}, upsertErr: true}},
			wants: `detach from key "k1"`,
		},
		{
			name:  "listing service accounts",
			st:    DetachStores{ServiceAccounts: &failingSAs{listErr: true}},
			wants: "list service accounts",
		},
		{
			name:  "writing a service account",
			st:    DetachStores{ServiceAccounts: &failingSAs{rows: []*serviceaccount.ServiceAccount{sa}, upsertErr: true}},
			wants: `detach from service account "sa1"`,
		},
		{
			name:  "listing hosts",
			st:    DetachStores{Hosts: &failingHosts{listErr: true}},
			wants: "list hosts",
		},
		{
			name:  "writing a host",
			st:    DetachStores{Hosts: &failingHosts{rows: []*host.Host{h}, upsertErr: true}},
			wants: `detach from host "h1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each case starts from the same refs; a prior subtest may have
			// cleared them on its own copy only.
			k.Spec.PolicyID, sa.Spec.PolicyID, h.Spec.DefaultPolicy = "pol-1", "pol-1", "pol-1"
			err := Detach(context.Background(), tc.st, "pol-1")
			if err == nil {
				t.Fatalf("Detach did not report the failure")
			}
			if !errors.Is(err, errStore) {
				t.Errorf("err = %v, want it to wrap the store error", err)
			}
			if got := err.Error(); !strings.Contains(got, tc.wants) {
				t.Errorf("err = %q, want it to mention %q", got, tc.wants)
			}
		})
	}

	if _, err := HostKeysUsingPolicy(context.Background(),
		DetachStores{HostKeys: &failingHostKeys{listErr: true}}, "pol-1"); !errors.Is(err, errStore) {
		t.Errorf("HostKeysUsingPolicy err = %v, want the store error", err)
	}
}
