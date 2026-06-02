package control

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/provider"
)

// enabledIndex mirrors a real snapshot (enabled providers/hosts/models only):
//
//	openai (P1) on host cloud (H1)
//	  gpt-4o   (M1) → binding enabled
//	  gpt-3-5  (M2) → binding DISABLED
//	  legacy   (M3, DEPRECATED) → binding enabled
func enabledIndex() *resolveIndex {
	p1 := &provider.Provider{Meta: meta.Metadata{ID: "P1", Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h1 := &host.Host{Meta: meta.Metadata{ID: "H1", Name: "cloud", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://x.example"}}

	m1 := &model.Model{
		Meta: meta.Metadata{ID: "M1", Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{ID: "M2", Name: "gpt-3-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-3-5"}}, Pointer: "gpt-3-5"},
	}
	m3 := &model.Model{
		Meta: meta.Metadata{ID: "M3", Name: "legacy", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "legacy"}}, Pointer: "legacy"},
	}
	m3.Spec.DeprecationDate = "2025-01-01"

	enabled := true
	disabled := false
	b1 := &binding.Binding{
		Meta: meta.Metadata{ID: "B1", Name: "b1", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: "M1", HostID: "H1", Adapter: adapters.OpenAI, Enabled: &enabled},
	}
	b2 := &binding.Binding{
		Meta: meta.Metadata{ID: "B2", Name: "b2", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: "M2", HostID: "H1", Adapter: adapters.OpenAI, Enabled: &disabled},
	}
	b3 := &binding.Binding{
		Meta: meta.Metadata{ID: "B3", Name: "b3", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: "M3", HostID: "H1", Adapter: adapters.OpenAI, Enabled: &enabled},
	}

	snap := catalog.Build(
		[]*provider.Provider{p1},
		[]*host.Host{h1},
		nil, nil,
		[]*model.Model{m1, m2, m3},
		nil, nil, nil,
		[]*binding.Binding{b1, b2, b3},
	)

	return &resolveIndex{
		snap:             snap,
		providersByID:    map[string]*provider.Provider{"P1": p1},
		providersByName:  map[string]*provider.Provider{"openai": p1},
		hostsByID:        map[string]*host.Host{"H1": h1},
		allModels:        []*model.Model{m1, m2, m3},
		modelsByID:       map[string]*model.Model{"M1": m1, "M2": m2, "M3": m3},
		modelsByProvider: map[string][]*model.Model{"P1": {m1, m2, m3}},
	}
}

func expand(t *testing.T, idx *resolveIndex, raw string, includeDeprecated bool) refResult {
	t.Helper()
	return expandOne(idx, raw, modelref.MustParse(raw), includeDeprecated)
}

func TestExpandOne_Grouping(t *testing.T) {
	idx := enabledIndex()
	r := expand(t, idx, "openai/gpt-4o", false)
	if len(r.Expanded) != 1 || r.Expanded[0] != "openai/gpt-4o@cloud" {
		t.Fatalf("expanded = %v", r.Expanded)
	}
	if len(r.ModelIDs) != 1 || r.ModelIDs[0] != "M1" {
		t.Fatalf("modelIds = %v", r.ModelIDs)
	}
	if len(r.HostIDs) != 1 || r.HostIDs[0] != "H1" {
		t.Fatalf("hostIds = %v", r.HostIDs)
	}
}

func TestExpandOne_DeadRef(t *testing.T) {
	idx := enabledIndex()
	// gpt-3-5's only binding is disabled → dead.
	if r := expand(t, idx, "openai/gpt-3-5", false); len(r.Expanded) != 0 {
		t.Fatalf("expected dead ref, got %v", r.Expanded)
	}
}

func TestExpandOne_Deprecation(t *testing.T) {
	idx := enabledIndex()
	if r := expand(t, idx, "openai/legacy", false); len(r.Expanded) != 0 {
		t.Fatalf("deprecated excluded by default, got %v", r.Expanded)
	}
	if r := expand(t, idx, "openai/legacy", true); len(r.Expanded) != 1 {
		t.Fatalf("deprecated included with flag, got %v", r.Expanded)
	}
	// Provider wildcard, deprecated off: only gpt-4o (gpt-3-5 binding disabled,
	// legacy deprecated).
	if r := expand(t, idx, "openai", false); len(r.Expanded) != 1 || r.Expanded[0] != "openai/gpt-4o@cloud" {
		t.Fatalf("provider expansion = %v", r.Expanded)
	}
}
