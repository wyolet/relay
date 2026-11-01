package policy

// detach_failures_test.go covers the paths the happy-path detach test does
// not: a caller that wired no store, hosts that name a different policy, and
// a store that fails mid-way. A partial failure has to be reported rather
// than leaving the caller believing the cascade ran.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
)

var errStore = errors.New("store is down")

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
	h := &host.Host{}
	h.Spec.DefaultPolicy = "pol-1"
	if err := Detach(context.Background(), DetachStores{Hosts: &failingHosts{rows: []*host.Host{h}}}, ""); err != nil {
		t.Fatalf("Detach with an empty id: %v", err)
	}
	if h.Spec.DefaultPolicy != "pol-1" {
		t.Fatalf("an empty id cleared %q", h.Spec.DefaultPolicy)
	}
}

// A host naming a different policy is left alone; a host whose whole list is
// the policy loses the list rather than keeping an empty one.
func TestDetach_LeavesUnrelatedRowsAlone(t *testing.T) {
	untouched := &host.Host{}
	untouched.Meta.Name, untouched.Spec.DefaultPolicy = "untouched", "pol-2"
	untouched.Spec.Policies = []string{"pol-2"}
	emptied := &host.Host{}
	emptied.Meta.Name = "emptied"
	emptied.Spec.Policies = []string{"pol-1"}

	st := DetachStores{Hosts: &failingHosts{rows: []*host.Host{untouched, emptied}}}
	if err := Detach(context.Background(), st, "pol-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if untouched.Spec.DefaultPolicy != "pol-2" || len(untouched.Spec.Policies) != 1 {
		t.Errorf("unrelated host changed: %+v", untouched.Spec)
	}
	if emptied.Spec.Policies != nil {
		t.Errorf("policies = %v, want nil rather than an empty list", emptied.Spec.Policies)
	}
}

// Every store failure is reported and names the row it happened on, so a
// half-applied cascade is visible instead of silent.
func TestDetach_ReportsStoreFailures(t *testing.T) {
	h := &host.Host{}
	h.Meta.Name, h.Spec.DefaultPolicy = "h1", "pol-1"
	for _, tc := range []struct {
		name  string
		st    DetachStores
		wants string
	}{
		{"listing hosts", DetachStores{Hosts: &failingHosts{listErr: true}}, "list hosts"},
		{"writing a host", DetachStores{Hosts: &failingHosts{rows: []*host.Host{h}, upsertErr: true}}, `detach from host "h1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.Spec.DefaultPolicy = "pol-1"
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
}
