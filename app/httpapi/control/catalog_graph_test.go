package control

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
)

func TestGraphModels_DeprecationFilter(t *testing.T) {
	snap := catalog.Build(
		[]*provider.Provider{{Meta: meta.Metadata{ID: "P1", Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	live := &model.Model{Meta: meta.Metadata{ID: "M1", Name: "live", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	dep := &model.Model{Meta: meta.Metadata{ID: "M2", Name: "old", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	dep.Spec.DeprecationDate = "2025-01-01"
	idx := &resolveIndex{snap: snap, allModels: []*model.Model{live, dep}}

	excl := graphModels(idx, false)
	if len(excl) != 1 || excl[0].Name != "live" {
		t.Fatalf("includeDeprecated=false: got %d models %+v, want only 'live'", len(excl), excl)
	}

	incl := graphModels(idx, true)
	if len(incl) != 2 {
		t.Fatalf("includeDeprecated=true: got %d models, want 2", len(incl))
	}
	var oldEntry *graphModel
	for i := range incl {
		if incl[i].Name == "old" {
			oldEntry = &incl[i]
		}
	}
	if oldEntry == nil || oldEntry.Deprecated == "" {
		t.Fatalf("deprecated model should be present and flagged, got %+v", incl)
	}
}

func TestGraphModels_PrunesDisabledBindings(t *testing.T) {
	hostID1 := meta.NewID()
	hostID2 := meta.NewID()
	modID := meta.NewID()
	provID := meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h1 := &host.Host{Meta: meta.Metadata{ID: hostID1, Name: "h1", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h1.example"}}
	h2 := &host.Host{Meta: meta.Metadata{ID: hostID2, Name: "h2", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h2.example"}}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m-snap"}}, Pointer: "m-snap"},
	}
	enabled := true
	disabled := false
	b1 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "b1", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID1, Adapter: adapters.OpenAI, Enabled: &enabled},
	}
	b2 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "b2", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID2, Adapter: adapters.OpenAI, Enabled: &disabled},
	}

	snap := catalog.Build([]*provider.Provider{prov}, []*host.Host{h1, h2}, nil, nil, []*model.Model{m}, nil, nil, nil, []*binding.Binding{b1, b2})
	idx := &resolveIndex{snap: snap, allModels: []*model.Model{m}}

	got := graphModels(idx, false)
	if len(got) != 1 || len(got[0].Bindings) != 1 || got[0].Bindings[0].HostID != hostID1 {
		t.Fatalf("disabled binding not pruned: %+v", got)
	}
}
