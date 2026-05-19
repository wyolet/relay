package inference

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/model"
)

// makeModel builds a Model with the given enabled-state + adapter per binding.
func makeModel(bindings ...struct {
	enabled bool
	adapter adapters.Kind
}) *model.Model {
	m := &model.Model{}
	for _, b := range bindings {
		hb := model.HostBinding{Adapter: b.adapter}
		// HostBinding.IsEnabled() reads the Enabled flag, which defaults
		// to true if the pointer is nil. Mirror that contract: set a
		// pointer to b.enabled.
		en := b.enabled
		hb.Enabled = &en
		m.Spec.Hosts = append(m.Spec.Hosts, hb)
	}
	return m
}

type bind = struct {
	enabled bool
	adapter adapters.Kind
}

func TestModelHasAdapter(t *testing.T) {
	cases := []struct {
		name   string
		m      *model.Model
		kind   adapters.Kind
		expect bool
	}{
		{
			name:   "no bindings",
			m:      makeModel(),
			kind:   adapters.OpenAI,
			expect: false,
		},
		{
			name:   "single enabled openai binding",
			m:      makeModel(bind{true, adapters.OpenAI}),
			kind:   adapters.OpenAI,
			expect: true,
		},
		{
			name:   "openai binding disabled",
			m:      makeModel(bind{false, adapters.OpenAI}),
			kind:   adapters.OpenAI,
			expect: false,
		},
		{
			name:   "openai disabled, anthropic enabled — looking for openai",
			m:      makeModel(bind{false, adapters.OpenAI}, bind{true, adapters.Anthropic}),
			kind:   adapters.OpenAI,
			expect: false,
		},
		{
			name:   "mixed adapters — finds anthropic when asked",
			m:      makeModel(bind{true, adapters.OpenAI}, bind{true, adapters.Anthropic}),
			kind:   adapters.Anthropic,
			expect: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelHasAdapter(tc.m, tc.kind); got != tc.expect {
				t.Errorf("modelHasAdapter(_, %q) = %v, want %v", tc.kind, got, tc.expect)
			}
		})
	}
}
