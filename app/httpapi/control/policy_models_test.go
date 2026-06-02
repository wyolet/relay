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

func boolPtr(b bool) *bool { return &b }

// testIndex builds a small catalog:
//
//	provider "openai" (id P1)
//	  model "gpt-4o" (M1, enabled)  → host "openai-cloud" (H1, enabled), binding enabled
//	  model "gpt-3-5" (M2, enabled) → host "openai-cloud" (H1), binding DISABLED
//	provider "acme" (P2)
//	  model "old"  (M3, DISABLED)   → host "openai-cloud" (H1), binding enabled
//	host "dead-host" (H2, DISABLED) with no bindings
func testIndex() *resolveIndex {
	p1 := &provider.Provider{Meta: meta.Metadata{ID: "P1", Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	p2 := &provider.Provider{Meta: meta.Metadata{ID: "P2", Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h1 := &host.Host{Meta: meta.Metadata{ID: "H1", Name: "openai-cloud", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h1.example"}}
	h2 := &host.Host{Meta: meta.Metadata{ID: "H2", Name: "dead-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://h2.example"}}
	h2.Spec.Enabled = boolPtr(false)

	m1 := &model.Model{
		Meta: meta.Metadata{ID: "M1", Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{ID: "M2", Name: "gpt-3-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-3-5"}}, Pointer: "gpt-3-5"},
	}
	m3 := &model.Model{
		Meta: meta.Metadata{ID: "M3", Name: "old", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P2"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "old"}}, Pointer: "old"},
	}
	m3.Spec.Enabled = boolPtr(false)

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

	// Build snapshot with all hosts (including disabled h2) but only enabled models/providers.
	// Note: m3 is disabled so it won't enter the snapshot — hostsByID still has H1.
	snap := catalog.Build(
		[]*provider.Provider{p1, p2},
		[]*host.Host{h1, h2},
		nil, nil,
		[]*model.Model{m1, m2},
		nil, nil, nil,
		[]*binding.Binding{b1, b2, b3},
	)

	return &resolveIndex{
		snap:             snap,
		providersByID:    map[string]*provider.Provider{"P1": p1, "P2": p2},
		providersByName:  map[string]*provider.Provider{"openai": p1, "acme": p2},
		hostsByID:        map[string]*host.Host{"H1": h1, "H2": h2},
		allModels:        []*model.Model{m1, m2, m3},
		modelsByProvider: map[string][]*model.Model{"P1": {m1, m2}, "P2": {m3}},
	}
}

func TestRefResolvesEnabled(t *testing.T) {
	idx := testIndex()
	cases := []struct {
		ref  string
		want bool
	}{
		{"openai/gpt-4o", true},              // enabled model + enabled binding
		{"openai/gpt-4o@openai-cloud", true}, // exact enabled binding
		{"openai/gpt-3-5", false},            // model enabled but binding disabled
		{"acme/old", false},                  // model disabled
		{"acme", false},                      // provider's only model disabled
		{"openai", true},                     // provider has ≥1 enabled binding
		{"@openai-cloud", true},              // enabled host
		{"@dead-host", false},                // disabled host
		{"openai/gpt-4o@dead-host", false},   // host disabled
		{"ghost/model", false},               // unknown provider
		{"openai/does-not-exist", false},     // unknown model
	}
	for _, c := range cases {
		ref := modelref.MustParse(c.ref)
		if got := refResolvesEnabled(idx, ref); got != c.want {
			t.Errorf("refResolvesEnabled(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

func TestNormalizePolicyRefs_Slugify(t *testing.T) {
	idx := testIndex()
	// Mixed-case / dotted inputs that should slugify to existing enabled refs.
	in := []string{"OpenAI/GPT-4o", "openai/gpt-4o", "@Openai-Cloud"}
	out, err := normalizePolicyRefs(idx, in, "p", "models")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "OpenAI/GPT-4o" and "openai/gpt-4o" collapse to one canonical entry.
	want := []string{"openai/gpt-4o", "@openai-cloud"}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("out[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}

func TestNormalizePolicyRefs_RejectsDisabled(t *testing.T) {
	idx := testIndex()
	for _, raw := range []string{"openai/gpt-3-5", "acme/old", "@dead-host"} {
		if _, err := normalizePolicyRefs(idx, []string{raw}, "p", "models"); err == nil {
			t.Errorf("expected rejection for %q, got nil", raw)
		}
	}
}
