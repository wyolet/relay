package inference

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/model"
)

// makeModel builds a Model with the given enabled-state + adapter per binding.
func makeModel(bindings ...struct {
	enabled bool
	adapter adapters.Name
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
	adapter adapters.Name
}

func TestSnapshotCreated(t *testing.T) {
	cases := []struct {
		name     string
		s        model.Snapshot
		fallback int64
		want     int64
	}{
		{"empty released falls back", model.Snapshot{}, 999, 999},
		{"malformed released falls back", model.Snapshot{ReleasedAt: "yesterday"}, 999, 999},
		{"valid date parsed", model.Snapshot{ReleasedAt: "2024-11-20"}, 999, time.Date(2024, 11, 20, 0, 0, 0, 0, time.UTC).Unix()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotCreated(&tc.s, tc.fallback); got != tc.want {
				t.Errorf("snapshotCreated = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestModelHasAdapter(t *testing.T) {
	cases := []struct {
		name        string
		m           *model.Model
		adapterName adapters.Name
		expect      bool
	}{
		{
			name:        "no bindings",
			m:           makeModel(),
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "single enabled openai binding",
			m:           makeModel(bind{true, adapters.OpenAI}),
			adapterName: adapters.OpenAI,
			expect:      true,
		},
		{
			name:        "openai binding disabled",
			m:           makeModel(bind{false, adapters.OpenAI}),
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "openai disabled, anthropic enabled — looking for openai",
			m:           makeModel(bind{false, adapters.OpenAI}, bind{true, adapters.Anthropic}),
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "mixed adapters — finds anthropic when asked",
			m:           makeModel(bind{true, adapters.OpenAI}, bind{true, adapters.Anthropic}),
			adapterName: adapters.Anthropic,
			expect:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelHasAdapter(tc.m, tc.adapterName); got != tc.expect {
				t.Errorf("modelHasAdapter(_, %q) = %v, want %v", tc.adapterName, got, tc.expect)
			}
		})
	}
}
