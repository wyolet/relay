package routing

import (
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

// twoHostFixture builds one model served by two hosts, both keyed by their
// own tier policy, and returns the ids the tests pin behaviour on.
type twoHostFixture struct {
	model            *model.Model
	hostA, hostB     string
	keyA, keyB       *hostkey.HostKey
	tierA, tierB     *policy.Policy
	provider         *provider.Provider
	bindingA, bindB  *binding.Binding
	hostRowA, hostRB *host.Host
}

func newTwoHostParts() twoHostFixture {
	provID, hostA, hostB, modID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	tierAID, tierBID := meta.NewID(), meta.NewID()
	f := twoHostFixture{hostA: hostA, hostB: hostB}
	f.provider = &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	f.hostRowA = &host.Host{
		Meta: meta.Metadata{ID: hostA, Name: "host-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://a.example"},
	}
	f.hostRB = &host.Host{
		Meta: meta.Metadata{ID: hostB, Name: "host-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://b.example"},
	}
	f.model = &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "m1", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1"}}, Pointer: slug.From("m1")},
	}
	f.bindingA = &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI},
	}
	f.bindB = &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostB, Adapter: adapters.OpenAI},
	}
	f.tierA = &policy.Policy{Meta: meta.Metadata{ID: tierAID, Name: "tier-a", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}}}
	f.tierB = &policy.Policy{Meta: meta.Metadata{ID: tierBID, Name: "tier-b", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostB}}}
	f.keyA = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "key-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hostA, PolicyID: tierAID, Value: "sk-a", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	f.keyB = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "key-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hostB, PolicyID: tierBID, Value: "sk-b", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	return f
}

// hostARows serves the model on host A alone, keyed through tier A; pols
// follow the tier in the policy list.
func (f twoHostFixture) hostARows(pols ...*policy.Policy) catalogtest.Catalog {
	return catalogtest.Catalog{
		Providers: []*provider.Provider{f.provider},
		Hosts:     []*host.Host{f.hostRowA},
		Policies:  append([]*policy.Policy{f.tierA}, pols...),
		Models:    []*model.Model{f.model},
		HostKeys:  []*hostkey.HostKey{f.keyA},
		Bindings:  []*binding.Binding{f.bindingA},
	}
}

// twoHostRows serves the model on both hosts, each keyed through its own
// tier; pols follow the tiers in the policy list.
func (f twoHostFixture) twoHostRows(pols ...*policy.Policy) catalogtest.Catalog {
	return catalogtest.Catalog{
		Providers: []*provider.Provider{f.provider},
		Hosts:     []*host.Host{f.hostRowA, f.hostRB},
		Policies:  append([]*policy.Policy{f.tierA, f.tierB}, pols...),
		Models:    []*model.Model{f.model},
		HostKeys:  []*hostkey.HostKey{f.keyA, f.keyB},
		Bindings:  []*binding.Binding{f.bindingA, f.bindB},
	}
}

// otherModelOnA is a second model of the same provider bound to host A, so a
// tier can grant something other than the fixture's model.
func (f twoHostFixture) otherModelOnA() (*model.Model, *binding.Binding) {
	other := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: f.model.Meta.Owner},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
	}
	otherBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: other.Meta.ID, HostID: f.hostA, Adapter: adapters.OpenAI},
	}
	return other, otherBnd
}
