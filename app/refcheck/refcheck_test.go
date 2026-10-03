package refcheck

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
)

func hostLookup(hosts ...*host.Host) func(context.Context, string) *host.Host {
	return func(_ context.Context, id string) *host.Host {
		for _, h := range hosts {
			if h.Meta.ID == id {
				return h
			}
		}
		return nil
	}
}

// A binding owned by anyone but a personal host's owner would carry that
// host into the binder's scope.
func TestBindingOnAPersonalHostMustShareItsOwner(t *testing.T) {
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	personal := &host.Host{Meta: meta.Metadata{ID: "h-bob", Name: "bob-box", Owner: bob}}
	shared := &host.Host{Meta: meta.Metadata{ID: "h-sys", Name: "sys", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	c := Checker{Rows: Lookup{Host: hostLookup(personal, shared)}}
	bind := func(hostID string, owner meta.Owner) *binding.Binding {
		return &binding.Binding{Meta: meta.Metadata{Name: "b", Owner: owner}, Spec: binding.Spec{HostID: hostID}}
	}
	ctx := context.Background()
	if err := c.HostBinding(ctx, bind("h-bob", bob)); err != nil {
		t.Fatalf("owner binding their own host: %v", err)
	}
	for _, other := range []meta.Owner{{Kind: meta.OwnerUser, ID: "u-alice"}, {Kind: meta.OwnerSystem}, {Kind: meta.OwnerUser}} {
		if err := c.HostBinding(ctx, bind("h-bob", other)); err == nil {
			t.Fatalf("binding owned by %+v accepted on bob's personal host", other)
		}
	}
	if err := c.HostBinding(ctx, bind("h-sys", meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})); err != nil {
		t.Fatalf("binding on a shared host: %v", err)
	}
}
