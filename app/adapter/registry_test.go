package adapter_test

import (
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
)

func buildSpec(name adapters.Name) *adapter.Spec {
	return (&adapter.Spec{
		Name:         name,
		UpstreamPath: "/v1/test",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}).Build()
}

func TestRegistry_Get(t *testing.T) {
	s := buildSpec(adapters.OpenAI)
	reg := adapter.NewRegistry(s)

	got := reg.Spec(adapters.OpenAI)
	if got == nil {
		t.Fatal("expected non-nil spec for openai")
	}
	if got.Name != adapters.OpenAI {
		t.Errorf("name: want openai, got %s", got.Name)
	}
}

func TestRegistry_Get_Missing(t *testing.T) {
	reg := adapter.NewRegistry(buildSpec(adapters.OpenAI))
	if s := reg.Spec(adapters.Anthropic); s != nil {
		t.Errorf("expected nil for unregistered name, got %v", s)
	}
}

func TestRegistry_PipelineAdapter(t *testing.T) {
	reg := adapter.NewRegistry(buildSpec(adapters.OpenAI), buildSpec(adapters.Anthropic))
	if a := reg.PipelineAdapter(adapters.OpenAI); a == nil {
		t.Error("expected non-nil pipeline.Adapter for openai")
	}
	if a := reg.PipelineAdapter(adapters.Anthropic); a == nil {
		t.Error("expected non-nil pipeline.Adapter for anthropic")
	}
	if a := reg.PipelineAdapter(adapters.OpenAIResponses); a != nil {
		t.Errorf("expected nil for unregistered name, got %v", a)
	}
}

func TestRegistry_AdapterMap(t *testing.T) {
	reg := adapter.NewRegistry(buildSpec(adapters.OpenAI), buildSpec(adapters.Anthropic))
	m := reg.AdapterMap()
	if len(m) != 2 {
		t.Errorf("map len: want 2, got %d", len(m))
	}
}

func TestRegistry_DuplicatePanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate name")
		}
	}()
	adapter.NewRegistry(buildSpec(adapters.OpenAI), buildSpec(adapters.OpenAI))
}

func TestRegistry_Specs(t *testing.T) {
	reg := adapter.NewRegistry(
		buildSpec(adapters.OpenAI),
		buildSpec(adapters.Anthropic),
		buildSpec(adapters.OpenAIResponses),
	)
	if len(reg.Specs()) != 3 {
		t.Errorf("want 3 specs, got %d", len(reg.Specs()))
	}
}
