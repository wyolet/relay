package adapter_test

import (
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// stubTranslator is a translator the registry only stores, never calls.
type stubTranslator struct{ v1.Translator }

// uncomparableTranslator panics when compared with ==.
type uncomparableTranslator struct {
	v1.Translator
	keys []string
}

func TestSpec_FamilyName(t *testing.T) {
	var none *adapter.Spec
	for _, tc := range []struct {
		spec *adapter.Spec
		want adapters.Name
	}{
		{&adapter.Spec{Name: "member", Family: "base"}, "base"},
		{&adapter.Spec{Name: "base"}, "base"},
		{none, ""},
	} {
		if got := tc.spec.FamilyName(); got != tc.want {
			t.Errorf("FamilyName() of %+v = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

func TestRegistry_Lookups(t *testing.T) {
	a, b := newBearerSpec("/a"), dualAuthSpec()
	a.Translator = stubTranslator{}
	reg := adapter.NewRegistry(a, b)

	if specs := reg.Specs(); len(specs) != 2 || specs[0] != a || specs[1] != b {
		t.Errorf("Specs() = %v, want the two specs in registration order", specs)
	}
	if reg.Spec(a.Name) != a || reg.Spec(b.Name) != b || reg.Spec(adapters.OpenAIResponses) != nil {
		t.Error("Spec() does not return the registered spec by name, or nil for an unknown one")
	}
	if reg.PipelineAdapter(a.Name) == nil || reg.PipelineAdapter(b.Name) == nil || reg.PipelineAdapter(adapters.OpenAIResponses) != nil {
		t.Error("PipelineAdapter() is not set for exactly the registered names")
	}

	m := reg.AdapterMap()
	if len(m) != 2 || m[a.Name] != reg.PipelineAdapter(a.Name) || m[b.Name] != reg.PipelineAdapter(b.Name) {
		t.Errorf("AdapterMap() = %v, want the two registered adapters", m)
	}
	delete(m, a.Name)
	if reg.PipelineAdapter(a.Name) == nil {
		t.Error("mutating the AdapterMap() copy changed the registry")
	}

	// A spec without a translator is left out.
	if tm := reg.TranslatorMap(); len(tm) != 1 || tm[v1.Name(a.Name)] != v1.Translator(stubTranslator{}) {
		t.Errorf("TranslatorMap() = %v, want only %q", tm, a.Name)
	}
}

func TestRegistry_NilReceiver(t *testing.T) {
	var reg *adapter.Registry
	if reg.Spec(adapters.OpenAI) != nil || reg.PipelineAdapter(adapters.OpenAI) != nil {
		t.Error("a nil registry returned a spec or an adapter")
	}
}

func TestNewRegistry_DuplicateNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r != "adapter.Registry: duplicate spec name openai" {
			t.Errorf("panic = %v", r)
		}
	}()
	adapter.NewRegistry(newBearerSpec("/a"), newBearerSpec("/b"))
}

func TestRegistry_AssertWired(t *testing.T) {
	// wired registers every upstream-binding name, then applies edit to the list.
	wired := func(edit func([]*adapter.Spec) []*adapter.Spec) *adapter.Registry {
		var specs []*adapter.Spec
		for _, n := range adapters.UpstreamBindingNames() {
			specs = append(specs, (&adapter.Spec{Name: n}).Build())
		}
		return adapter.NewRegistry(edit(specs)...)
	}
	inbound := []adapter.InboundPath{{Path: "/v1/x"}}
	first := adapters.UpstreamBindingNames()[0]
	tests := []struct {
		name    string
		edit    func([]*adapter.Spec) []*adapter.Spec
		wantErr string
	}{
		{"every binding registered", func(s []*adapter.Spec) []*adapter.Spec { return s }, ""},
		{"a binding without a spec", func(s []*adapter.Spec) []*adapter.Spec { return s[1:] },
			`adapter.Registry: upstream binding "` + string(first) + `" has no registered spec`},
		{"inbound spec without a translator", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", InboundPaths: inbound}).Build())
		}, `adapter.Registry: inbound spec "extra" has no canonical translator`},
		{"inbound byte-pass spec needs no translator", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", InboundPaths: inbound, BytePass: true}).Build())
		}, ""},
		{"inbound spec with a translator", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", InboundPaths: inbound, Translator: stubTranslator{}}).Build())
		}, ""},
		{"family of a registered spec", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", Family: first}).Build())
		}, ""},
		{"unknown family", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", Family: "nope"}).Build())
		}, `adapter.Registry: spec "extra" names unregistered family "nope"`},
		{"uncomparable translator", func(s []*adapter.Spec) []*adapter.Spec {
			return append(s, (&adapter.Spec{Name: "extra", Translator: uncomparableTranslator{}}).Build())
		}, `adapter.Registry: spec "extra" has a translator of uncomparable type adapter_test.uncomparableTranslator`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wired(tt.edit).AssertWired()
			var got string
			if err != nil {
				got = err.Error()
			}
			if got != tt.wantErr {
				t.Errorf("AssertWired = %q, want %q", got, tt.wantErr)
			}
		})
	}
}
