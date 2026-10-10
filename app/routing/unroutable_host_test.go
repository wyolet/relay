package routing_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/routing"
)

// A host with no baseURL is valid catalog data (the operator points it at
// their own endpoint after install) but has nothing to dial: routing passes
// over its bindings, and listing agrees.
func TestResolve_SkipsHostWithoutBaseURL(t *testing.T) {
	provID, polID := meta.NewID(), meta.NewID()
	unsetID, liveID := meta.NewID(), meta.NewID()
	sharedID, onlyUnsetID := meta.NewID(), meta.NewID()

	sys := meta.Owner{Kind: meta.OwnerSystem}
	newModel := func(id, name string) *model.Model {
		return &model.Model{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name}}, Pointer: name},
		}
	}
	newBinding := func(name, modelID, hostID string) *binding.Binding {
		return &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: sys},
			Spec: binding.Spec{ModelID: modelID, HostID: hostID, Adapter: adapters.OpenAI},
		}
	}
	unset := &host.Host{
		Meta: meta.Metadata{ID: unsetID, Name: "self-hosted", Owner: sys},
		Spec: host.Spec{NoAuth: true},
	}
	live := &host.Host{
		Meta: meta.Metadata{ID: liveID, Name: "live", Owner: sys},
		Spec: host.Spec{BaseURL: "http://live.example", NoAuth: true},
	}
	shared, onlyUnset := newModel(sharedID, "shared"), newModel(onlyUnsetID, "only-unset")
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "grant", Owner: sys},
		Spec: policy.Spec{Models: []string{"acme/shared", "acme/only-unset"}},
	}

	c := catalogtest.Catalog{
		Providers: []*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: sys}}},
		Hosts:     []*host.Host{unset, live},
		Policies:  []*policy.Policy{pol},
		Models:    []*model.Model{shared, onlyUnset},
		// "a-" sorts first, so the unroutable binding is tried before the live one.
		Bindings: []*binding.Binding{
			newBinding("a-shared-on-self-hosted", sharedID, unsetID),
			newBinding("b-shared-on-live", sharedID, liveID),
			newBinding("only-unset-on-self-hosted", onlyUnsetID, unsetID),
		},
	}.Load(t)
	r := routing.New(c)
	snap := c.Current()

	plan, err := r.Resolve(routing.Request{ModelName: "shared", Policy: pol})
	if err != nil {
		t.Fatalf("resolve shared: %v", err)
	}
	if plan.Host.Meta.ID != liveID {
		t.Fatalf("plan host = %q, want the host with a baseURL", plan.Host.Meta.Name)
	}

	if _, err := r.Resolve(routing.Request{ModelName: "only-unset", Policy: pol}); !errors.Is(err, routing.ErrNoKeys) {
		t.Fatalf("resolve only-unset = %v, want ErrNoKeys", err)
	}
	if _, err := r.Resolve(routing.Request{ModelName: "only-unset", Policy: pol, SkipKeyCheck: true}); err == nil {
		t.Fatal("proxy-mode resolve picked a host with no baseURL")
	}

	if !r.PolicyAllows(snap, pol, shared) {
		t.Fatal("PolicyAllows(shared) = false, want true")
	}
	if r.PolicyAllows(snap, pol, onlyUnset) {
		t.Fatal("PolicyAllows(only-unset) = true; listing would advertise an unroutable model")
	}
	if r.PolicylessAllows(snap, onlyUnset, "", "") {
		t.Fatal("PolicylessAllows(only-unset) = true; listing would advertise an unroutable model")
	}
}
