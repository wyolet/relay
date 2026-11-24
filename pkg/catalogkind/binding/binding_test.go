package binding

import (
	"errors"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/meta"
)

// problems reports why Validate refused: "Namespace tag" per failed field rule, or the message of a kind-specific rule.
func problems(err error) []string {
	var fields validator.ValidationErrors
	if errors.As(err, &fields) {
		var out []string
		for _, f := range fields {
			out = append(out, f.Namespace()+" "+f.Tag())
		}
		return out
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}

func TestValidate(t *testing.T) {
	// A binding decides which host serves a model's traffic, so no user, team or project owns one.
	const sharedData = `binding "m-on-h": host bindings are shared catalog data: owner.kind cannot be `
	for _, tc := range []struct {
		name        string
		edit        func(*Binding)
		want        []string
		wantAdapter adapters.Name
	}{
		{"no owner, no adapter: the default adapter is filled in", func(*Binding) {}, nil, adapters.OpenAI},
		{"system owner", func(b *Binding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }, nil, adapters.OpenAI},
		{"a named adapter is kept", func(b *Binding) { b.Spec.Adapter = adapters.Anthropic }, nil, adapters.Anthropic},
		{"responses adapter", func(b *Binding) { b.Spec.Adapter = adapters.OpenAIResponses }, nil, adapters.OpenAIResponses},
		{"priced", func(b *Binding) { b.Spec.PricingID = meta.NewID() }, nil, adapters.OpenAI},
		{"embeddings are not an upstream binding", func(b *Binding) { b.Spec.Adapter = adapters.OpenAIEmbeddings },
			[]string{`binding "m-on-h": adapter "openai_embeddings" is not a valid upstream binding (want one of [openai anthropic gemini openai_responses])`}, adapters.OpenAIEmbeddings},
		{"unknown adapter", func(b *Binding) { b.Spec.Adapter = "cohere" },
			[]string{`binding "m-on-h": adapter "cohere" is not a valid upstream binding (want one of [openai anthropic gemini openai_responses])`}, "cohere"},
		{"missing ids", func(b *Binding) { b.Spec.ModelID, b.Spec.HostID = "", "" },
			[]string{"Binding.Spec.ModelID required", "Binding.Spec.HostID required"}, adapters.OpenAI},
		{"ids are not UUIDs", func(b *Binding) { b.Spec.ModelID, b.Spec.PricingID = "gpt-4o", "sheet" },
			[]string{"Binding.Spec.ModelID uuid", "Binding.Spec.PricingID uuid"}, adapters.OpenAI},
		{"user owner", func(b *Binding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-1"} }, []string{sharedData + `"user"`}, adapters.OpenAI},
		{"user owner without an id", func(b *Binding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerUser} }, []string{sharedData + `"user"`}, adapters.OpenAI},
		{"team owner", func(b *Binding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"} }, []string{sharedData + `"team"`}, adapters.OpenAI},
		{"project owner", func(b *Binding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"} }, []string{sharedData + `"project"`}, adapters.OpenAI},
		{"upstream name for its one snapshot", func(b *Binding) {
			b.Spec.UpstreamName, b.Spec.Snapshots = "global.acme.m-v1:0", []string{"m-2026-01-01"}
		}, nil, adapters.OpenAI},
		{"upstream name for every snapshot", func(b *Binding) { b.Spec.UpstreamName = "global.acme.m-v1:0" },
			[]string{`binding "m-on-h": upstreamName "global.acme.m-v1:0" names one upstream model, so snapshots must list exactly one snapshot (got 0)`}, adapters.OpenAI},
		{"upstream name for two snapshots", func(b *Binding) {
			b.Spec.UpstreamName, b.Spec.Snapshots = "acme/m", []string{"m-2026-01-01", "m-2026-02-01"}
		}, []string{`binding "m-on-h": upstreamName "acme/m" names one upstream model, so snapshots must list exactly one snapshot (got 2)`}, adapters.OpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Binding{Meta: meta.Metadata{Name: "m-on-h"}, Spec: Spec{ModelID: meta.NewID(), HostID: meta.NewID()}}
			tc.edit(b)
			if diff := cmp.Diff(tc.want, problems(b.Validate())); diff != "" {
				t.Errorf("Validate (-want +got):\n%s", diff)
			}
			if b.Spec.Adapter != tc.wantAdapter {
				t.Errorf("adapter after Validate = %q, want %q", b.Spec.Adapter, tc.wantAdapter)
			}
		})
	}
}

func TestServesAndIsEnabled(t *testing.T) {
	no := false
	all := &Binding{}
	some := &Binding{Spec: Spec{Snapshots: []string{"2026-01-01", "latest"}, Enabled: &no}}
	for _, tc := range []struct {
		b        *Binding
		snapshot string
		want     bool
	}{
		{all, "2026-01-01", true}, {all, "", true},
		{some, "2026-01-01", true}, {some, "latest", true}, {some, "2025-01-01", false}, {some, "", false}, {some, "LATEST", false},
	} {
		if got := tc.b.Serves(tc.snapshot); got != tc.want {
			t.Errorf("Serves(%q) with snapshots %v = %v, want %v", tc.snapshot, tc.b.Spec.Snapshots, got, tc.want)
		}
	}
	if !all.IsEnabled() || some.IsEnabled() {
		t.Errorf("IsEnabled: unset = %v, false = %v; want true, false", all.IsEnabled(), some.IsEnabled())
	}
}
