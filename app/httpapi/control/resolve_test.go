package control

import (
	"testing"

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
	p1 := &provider.Provider{Meta: meta.Metadata{ID: "P1", Name: "openai"}}
	h1 := &host.Host{Meta: meta.Metadata{ID: "H1", Name: "cloud"}}

	m1 := &model.Model{Meta: meta.Metadata{ID: "M1", Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m1.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai"}}
	m2 := &model.Model{Meta: meta.Metadata{ID: "M2", Name: "gpt-3-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m2.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai", Enabled: boolPtr(false)}}
	m3 := &model.Model{Meta: meta.Metadata{ID: "M3", Name: "legacy", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m3.Spec.DeprecationDate = "2025-01-01"
	m3.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai"}}

	return &resolveIndex{
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
