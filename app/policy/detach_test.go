package policy

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
)

// A deleted policy leaves every host menu naming it, the default included.
func TestDetach_ClearsTheHostMenus(t *testing.T) {
	h := &host.Host{}
	h.Meta.Name, h.Spec.DefaultPolicy = "h1", "pol-1"
	h.Spec.Policies = []string{"pol-1", "pol-2"}

	if err := Detach(context.Background(), DetachStores{Hosts: &fakeHosts{rows: []*host.Host{h}}}, "pol-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if h.Spec.DefaultPolicy != "" || len(h.Spec.Policies) != 1 || h.Spec.Policies[0] != "pol-2" {
		t.Fatalf("host refs left: default=%q policies=%v", h.Spec.DefaultPolicy, h.Spec.Policies)
	}
}

type fakeHosts struct{ rows []*host.Host }

func (f *fakeHosts) List(context.Context) ([]*host.Host, error) { return f.rows, nil }
func (f *fakeHosts) Upsert(context.Context, *host.Host) error   { return nil }
