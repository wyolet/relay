package control

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

func TestGraphModels_DeprecationFilter(t *testing.T) {
	live := &model.Model{Meta: meta.Metadata{ID: "M1", Name: "live", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	dep := &model.Model{Meta: meta.Metadata{ID: "M2", Name: "old", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	dep.Spec.DeprecationDate = "2025-01-01"
	idx := &resolveIndex{allModels: []*model.Model{live, dep}}

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
	m := &model.Model{Meta: meta.Metadata{ID: "M1", Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m.Spec.Hosts = []model.HostBinding{
		{HostID: "H1", Adapter: "openai"},
		{HostID: "H2", Adapter: "openai", Enabled: boolPtr(false)},
	}
	idx := &resolveIndex{allModels: []*model.Model{m}}

	got := graphModels(idx, false)
	if len(got) != 1 || len(got[0].Bindings) != 1 || got[0].Bindings[0].HostID != "H1" {
		t.Fatalf("disabled binding not pruned: %+v", got)
	}
}
