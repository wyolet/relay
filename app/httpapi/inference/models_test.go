package inference

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
)

// helpers for building minimal catalog snapshots in tests.

type mProvList []*provider.Provider

func (l mProvList) List(context.Context) ([]*provider.Provider, error) { return l, nil }

type mHostList []*host.Host

func (l mHostList) List(context.Context) ([]*host.Host, error) { return l, nil }

type mModList []*model.Model

func (l mModList) List(context.Context) ([]*model.Model, error) { return l, nil }

type mBndList []*binding.Binding

func (l mBndList) List(context.Context) ([]*binding.Binding, error) { return l, nil }

// snapWithBindings builds a catalog snapshot containing one model with the
// given adapter bindings. enabled flags map to binding.Spec.Enabled.
func snapWithBindings(m *model.Model, bindings []*binding.Binding) *catalog.Snapshot {
	prov := &provider.Provider{
		Meta: meta.Metadata{ID: m.Meta.Owner.ID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	hostIDs := map[string]bool{}
	for _, b := range bindings {
		hostIDs[b.Spec.HostID] = true
	}
	hosts := make([]*host.Host, 0, len(hostIDs))
	for id := range hostIDs {
		hosts = append(hosts, &host.Host{
			Meta: meta.Metadata{ID: id, Name: id, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: "http://x.example"},
		})
	}
	snap := catalog.Build(
		[]*provider.Provider{prov},
		hosts,
		nil, nil,
		[]*model.Model{m},
		nil, nil, nil,
		bindings,
	)
	return snap
}

// makeModelWithSnap builds a Model + catalog snapshot with the given bindings.
func makeModelWithSnap(bs ...struct {
	enabled bool
	adapter adapters.Name
}) (*model.Model, *catalog.Snapshot) {
	provID := meta.NewID()
	modID := meta.NewID()
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "m-snap"}},
			Pointer:   "m-snap",
		},
	}
	bindings := make([]*binding.Binding, 0, len(bs))
	for i, b := range bs {
		hostID := meta.NewID()
		en := b.enabled
		bindings = append(bindings, &binding.Binding{
			Meta: meta.Metadata{
				ID:    meta.NewID(),
				Name:  fmt.Sprintf("b%d", i),
				Owner: meta.Owner{Kind: meta.OwnerSystem},
			},
			Spec: binding.Spec{
				ModelID: modID,
				HostID:  hostID,
				Adapter: b.adapter,
				Enabled: &en,
			},
		})
	}
	return m, snapWithBindings(m, bindings)
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
		bs          []bind
		adapterName adapters.Name
		expect      bool
	}{
		{
			name:        "no bindings",
			bs:          nil,
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "single enabled openai binding",
			bs:          []bind{{true, adapters.OpenAI}},
			adapterName: adapters.OpenAI,
			expect:      true,
		},
		{
			name:        "openai binding disabled",
			bs:          []bind{{false, adapters.OpenAI}},
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "openai disabled, anthropic enabled — looking for openai",
			bs:          []bind{{false, adapters.OpenAI}, {true, adapters.Anthropic}},
			adapterName: adapters.OpenAI,
			expect:      false,
		},
		{
			name:        "mixed adapters — finds anthropic when asked",
			bs:          []bind{{true, adapters.OpenAI}, {true, adapters.Anthropic}},
			adapterName: adapters.Anthropic,
			expect:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, snap := makeModelWithSnap(tc.bs...)
			if got := modelHasAdapter(snap, m, tc.adapterName); got != tc.expect {
				t.Errorf("modelHasAdapter(_, %q) = %v, want %v", tc.adapterName, got, tc.expect)
			}
		})
	}
}
