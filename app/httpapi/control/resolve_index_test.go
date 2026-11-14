package control

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/modelref"
)

// resolveCatalog is the rows the resolve, graph and policy-grant tests read:
//
//	openai/gpt-3-5  at cloud, binding disabled
//	openai/gpt-4o   at cloud and edge, featured
//	openai/legacy   at cloud, deprecated
//	acme/old        at cloud, model disabled
//	dead-host       disabled, serving nothing
func resolveCatalog() catalogtest.Catalog {
	sys := meta.Owner{Kind: meta.OwnerSystem}
	off := false
	mdl := func(id, name, providerID string) *model.Model {
		return &model.Model{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: providerID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name + "-snap"}}, Pointer: name + "-snap"},
		}
	}
	bind := func(id, modelID, hostID string) *binding.Binding {
		return &binding.Binding{Meta: meta.Metadata{ID: id, Name: id, Owner: sys}, Spec: binding.Spec{ModelID: modelID, HostID: hostID, Adapter: adapters.OpenAI}}
	}
	gpt35, gpt4o, legacy, old := mdl("M1", "gpt-3-5", "P1"), mdl("M2", "gpt-4o", "P1"), mdl("M3", "legacy", "P1"), mdl("M4", "old", "P2")
	gpt4o.Meta.DisplayName, gpt4o.Meta.Labels = "GPT-4o", map[string]string{"featured": "true", "tier": "flagship"}
	gpt4o.Spec.ContextWindowTotal, gpt4o.Spec.ContextWindowInput, gpt4o.Spec.Capabilities.Tools = 128000, 100000, true
	legacy.Spec.DeprecationDate = "2025-01-01"
	old.Spec.Enabled = &off
	disabledBinding := bind("B1", "M1", "H1")
	disabledBinding.Spec.Enabled = &off
	return catalogtest.Catalog{
		Providers: []*provider.Provider{
			{Meta: meta.Metadata{ID: "P1", Name: "openai", DisplayName: "OpenAI", Owner: sys}},
			{Meta: meta.Metadata{ID: "P2", Name: "acme", Owner: sys}},
		},
		Hosts: []*host.Host{
			{Meta: meta.Metadata{ID: "H1", Name: "cloud", DisplayName: "Cloud", Owner: sys}, Spec: host.Spec{BaseURL: "http://cloud.example", Icon: &meta.Icon{Path: "icons/cloud.svg"}}},
			{Meta: meta.Metadata{ID: "H2", Name: "edge", Owner: sys}, Spec: host.Spec{BaseURL: "http://edge.example"}},
			{Meta: meta.Metadata{ID: "H3", Name: "dead-host", Owner: sys}, Spec: host.Spec{BaseURL: "http://dead.example", Enabled: &off}},
		},
		Models:   []*model.Model{gpt35, gpt4o, legacy, old},
		Bindings: []*binding.Binding{disabledBinding, bind("B2", "M2", "H1"), bind("B3", "M2", "H2"), bind("B4", "M3", "H1"), bind("B5", "M4", "H1")},
	}
}

// resolveIndexes is the fixture indexed twice: as a loaded catalog holds it, with the disabled rows gone, and with every row still indexed, which is what the enabled checks guard against. A reference answers the same on both.
func resolveIndexes(t *testing.T) map[string]*resolveIndex {
	t.Helper()
	c := resolveCatalog()
	loaded, err := loadResolveIndex(Deps{Catalog: c.Load(t)})
	if err != nil {
		t.Fatal(err)
	}
	all := &resolveIndex{
		snap:          c.Snapshot(),
		providersByID: map[string]*provider.Provider{}, providersByName: map[string]*provider.Provider{},
		hostsByID: map[string]*host.Host{}, modelsByID: map[string]*model.Model{}, modelsByProvider: map[string][]*model.Model{},
	}
	for _, p := range c.Providers {
		all.providersByID[p.Meta.ID], all.providersByName[p.Meta.Name] = p, p
	}
	for _, h := range c.Hosts {
		all.hostsByID[h.Meta.ID] = h
	}
	for _, m := range c.Models {
		all.allModels, all.modelsByID[m.Meta.ID] = append(all.allModels, m), m
		all.modelsByProvider[m.Meta.Owner.ID] = append(all.modelsByProvider[m.Meta.Owner.ID], m)
	}
	return map[string]*resolveIndex{"loaded catalog": loaded, "disabled rows indexed": all}
}

func TestLoadResolveIndex(t *testing.T) {
	if _, err := loadResolveIndex(Deps{}); err == nil || err.Error() != "catalog not ready" {
		t.Errorf("no catalog: %v", err)
	}
	idx, err := loadResolveIndex(Deps{Catalog: resolveCatalog().Load(t)})
	if err != nil {
		t.Fatal(err)
	}
	names := func(ms []*model.Model) (out []string) {
		for _, m := range ms {
			out = append(out, m.Meta.Name)
		}
		return out
	}
	got := map[string]any{
		"providers": len(idx.providersByID), "byName": idx.providersByName["openai"] == idx.providersByID["P1"],
		"hosts": len(idx.hostsByID), "models": names(idx.allModels), "openai": names(idx.modelsByProvider["P1"]), "acme": names(idx.modelsByProvider["P2"]),
	}
	enabled := []string{"gpt-3-5", "gpt-4o", "legacy"}
	want := map[string]any{"providers": 2, "byName": true, "hosts": 2, "models": enabled, "openai": enabled, "acme": []string(nil)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("index (-want +got):\n%s", diff)
	}
}

func TestExpandOne(t *testing.T) {
	gpt4oCloud, gpt4oEdge, legacyCloud := resolveBindingRef{ModelID: "M2", HostID: "H1"}, resolveBindingRef{ModelID: "M2", HostID: "H2"}, resolveBindingRef{ModelID: "M3", HostID: "H1"}
	dead := func(ref string) refResult {
		return refResult{Ref: ref, Expanded: []string{}, ModelIDs: []string{}, HostIDs: []string{}, Bindings: []resolveBindingRef{}}
	}
	for _, tc := range []struct {
		name, ref         string
		includeDeprecated bool
		want              refResult
	}{
		{name: "a model at every host serving it", ref: "openai/gpt-4o", want: refResult{
			Expanded: []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge"}, ModelIDs: []string{"M2"}, HostIDs: []string{"H1", "H2"}, Bindings: []resolveBindingRef{gpt4oCloud, gpt4oEdge}}},
		{name: "a model at one host", ref: "openai/gpt-4o@edge", want: refResult{
			Expanded: []string{"openai/gpt-4o@edge"}, ModelIDs: []string{"M2"}, HostIDs: []string{"H2"}, Bindings: []resolveBindingRef{gpt4oEdge}}},
		{name: "a host", ref: "@edge", want: refResult{
			Expanded: []string{"openai/gpt-4o@edge"}, ModelIDs: []string{"M2"}, HostIDs: []string{"H2"}, Bindings: []resolveBindingRef{gpt4oEdge}}},
		{name: "a provider leaves out its deprecated and unserved models", ref: "openai", want: refResult{
			Expanded: []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge"}, ModelIDs: []string{"M2"}, HostIDs: []string{"H1", "H2"}, Bindings: []resolveBindingRef{gpt4oCloud, gpt4oEdge}}},
		{name: "a provider with its deprecated models", ref: "openai", includeDeprecated: true, want: refResult{
			Expanded: []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge", "openai/legacy@cloud"}, ModelIDs: []string{"M2", "M3"}, HostIDs: []string{"H1", "H2"}, Bindings: []resolveBindingRef{gpt4oCloud, gpt4oEdge, legacyCloud}}},
		{name: "a deprecated model when asked for", ref: "openai/legacy", includeDeprecated: true, want: refResult{
			Expanded: []string{"openai/legacy@cloud"}, ModelIDs: []string{"M3"}, HostIDs: []string{"H1"}, Bindings: []resolveBindingRef{legacyCloud}}},
		{name: "a deprecated model by default", ref: "openai/legacy", want: dead("openai/legacy")},
		{name: "a model whose only binding is disabled", ref: "openai/gpt-3-5", want: dead("openai/gpt-3-5")},
		{name: "a model at a host that does not serve it", ref: "openai/legacy@edge", includeDeprecated: true, want: dead("openai/legacy@edge")},
		{name: "a disabled host", ref: "@dead-host", want: dead("@dead-host")},
		{name: "an unknown model", ref: "openai/nope", want: dead("openai/nope")},
		{name: "an unknown provider", ref: "ghost/gpt-4o", want: dead("ghost/gpt-4o")},
	} {
		tc.want.Ref = tc.ref
		for name, idx := range resolveIndexes(t) {
			t.Run(tc.name+", "+name, func(t *testing.T) {
				got := expandOne(idx, tc.ref, modelref.MustParse(tc.ref), tc.includeDeprecated)
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("expandOne(%q) (-want +got):\n%s", tc.ref, diff)
				}
			})
		}
	}
}

func TestExpandOne_ModelMarkedActiveIsNotDeprecated(t *testing.T) {
	c := resolveCatalog()
	c.Models[1].Spec.Deprecation = &model.Deprecation{Status: model.DeprecationActive}
	idx, err := loadResolveIndex(Deps{Catalog: c.Load(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got := expandOne(idx, "openai/gpt-4o", modelref.MustParse("openai/gpt-4o"), false).Expanded; len(got) != 2 {
		t.Errorf("expanded = %v, want both hosts", got)
	}
	if got := graphModels(idx, true, map[string]string{"featured": "true"}); len(got) != 1 || got[0].Deprecated != "" {
		t.Errorf("graph models = %+v, want gpt-4o with no deprecated marker", got)
	}
}
