package refcheck

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
)

// Reading another project's host key is not a licence to spend it: a
// project's policy may name only its own keys or shared ones.
func TestProjectPolicyCannotSpendAnotherProjectsHostKey(t *testing.T) {
	keys := map[string]*hostkey.HostKey{
		"hk-own":    {Meta: meta.Metadata{ID: "hk-own", Name: "own", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}},
		"hk-other":  {Meta: meta.Metadata{ID: "hk-other", Name: "other", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}}},
		"hk-shared": {Meta: meta.Metadata{ID: "hk-shared", Name: "shared", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
	}
	c := Checker{Rows: Lookup{HostKey: func(_ context.Context, id string) *hostkey.HostKey { return keys[id] }}}
	ref := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	ctx := context.Background()
	if err := c.HostKeyRefs(ctx, []string{"hk-own", "hk-shared"}, ref); err != nil {
		t.Fatalf("own and shared keys: %v", err)
	}
	if err := c.HostKeyRefs(ctx, []string{"hk-other"}, ref); err == nil {
		t.Fatal("project p-1's policy accepted project p-2's host key")
	}
}

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
