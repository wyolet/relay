package control

import (
	"testing"

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
	p1 := &provider.Provider{Meta: meta.Metadata{ID: "P1", Name: "openai"}}
	p2 := &provider.Provider{Meta: meta.Metadata{ID: "P2", Name: "acme"}}
	h1 := &host.Host{Meta: meta.Metadata{ID: "H1", Name: "openai-cloud"}}
	h2 := &host.Host{Meta: meta.Metadata{ID: "H2", Name: "dead-host"}}
	h2.Spec.Enabled = boolPtr(false)

	m1 := &model.Model{Meta: meta.Metadata{ID: "M1", Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m1.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai"}}
	m2 := &model.Model{Meta: meta.Metadata{ID: "M2", Name: "gpt-3-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P1"}}}
	m2.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai", Enabled: boolPtr(false)}}
	m3 := &model.Model{Meta: meta.Metadata{ID: "M3", Name: "old", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "P2"}}}
	m3.Spec.Enabled = boolPtr(false)
	m3.Spec.Hosts = []model.HostBinding{{HostID: "H1", Adapter: "openai"}}

	return &resolveIndex{
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
