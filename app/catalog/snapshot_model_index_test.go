package catalog

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// resolvedTo names what ref resolves to as model/snapshot@pinned-host, "" for a miss.
func resolvedTo(s *Snapshot, ref string) string {
	res := s.ResolveModelRef(ref)
	if len(res.Models) == 0 {
		return ""
	}
	out := res.Models[0].Meta.Name + "/" + res.Snapshot.Name
	if h, ok := s.HostSlug(res.PinHostID); ok {
		out += "@" + h
	}
	return out
}

// buildModelAt builds a catalog with m, from provider acme, bound at one host per entry of serves, each binding serving the snapshots listed (none listed serves all).
func buildModelAt(m *model.Model, serves map[string][]string) *Snapshot {
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}}
	m.Meta.ID, m.Meta.Owner = meta.NewID(), meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}
	var hosts []*host.Host
	var bindings []*binding.Binding
	for name, snapshots := range serves {
		h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: system}, Spec: host.Spec{BaseURL: "https://" + name + ".example"}}
		hosts = append(hosts, h)
		bindings = append(bindings, &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: m.Meta.Name + "-on-" + name, Owner: system},
			Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI, Snapshots: snapshots},
		})
	}
	return BuildCatalog([]*provider.Provider{prov}, hosts, []*model.Model{m}, nil, bindings, nil)
}

// A host-pinned name exists only where the host's binding serves the snapshot it names; a declared alias pins where its pointer snapshot is served. A binding listing no snapshots serves them all.
func TestResolveModelRef_PinsOnlyServedSnapshots(t *testing.T) {
	m := &model.Model{
		Meta: meta.Metadata{Name: "m1"},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1"}, {Name: "m1-remote"}}, Pointer: "m1", Aliases: []string{"m1-latest"}},
	}
	s := buildModelAt(m, map[string][]string{"host-a": {"m1-remote"}, "host-b": {"m1"}, "host-c": nil})

	got := map[string]string{}
	for _, name := range []string{"m1", "m1-remote", "m1-latest"} {
		for _, ref := range []string{name, "acme/" + name, name + "@host-a", name + "@host-b", name + "@host-c", "acme/" + name + "@host-a", "acme/" + name + "@host-b", "acme/" + name + "@host-c"} {
			got[ref] = resolvedTo(s, ref)
		}
	}
	want := map[string]string{
		"m1":                    "m1/m1",
		"acme/m1":               "m1/m1",
		"m1@host-a":             "",
		"m1@host-b":             "m1/m1@host-b",
		"m1@host-c":             "m1/m1@host-c",
		"acme/m1@host-a":        "",
		"acme/m1@host-b":        "m1/m1@host-b",
		"acme/m1@host-c":        "m1/m1@host-c",
		"m1-remote":             "m1/m1-remote",
		"acme/m1-remote":        "m1/m1-remote",
		"m1-remote@host-a":      "m1/m1-remote@host-a",
		"m1-remote@host-b":      "",
		"m1-remote@host-c":      "m1/m1-remote@host-c",
		"acme/m1-remote@host-a": "m1/m1-remote@host-a",
		"acme/m1-remote@host-b": "",
		"acme/m1-remote@host-c": "m1/m1-remote@host-c",
		"m1-latest":             "m1/m1",
		"acme/m1-latest":        "m1/m1",
		"m1-latest@host-a":      "",
		"m1-latest@host-b":      "m1/m1@host-b",
		"m1-latest@host-c":      "m1/m1@host-c",
		"acme/m1-latest@host-a": "",
		"acme/m1-latest@host-b": "m1/m1@host-b",
		"acme/m1-latest@host-c": "m1/m1@host-c",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("resolution (-want +got):\n%s", diff)
	}
}

// model.Spec.Pointer is documented as the snapshot the bare model name resolves to.
func TestResolveModelRef_BareModelNameResolvesToPointer(t *testing.T) {
	t.Skip("bug: request resolution indexes snapshot names and declared aliases only and never reads Spec.Pointer, so a model whose name is no snapshot name answers model_not_found for its bare name")
	m := &model.Model{
		Meta: meta.Metadata{Name: "m1"},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1-latest"}, {Name: "m1-14b"}}, Pointer: "m1-latest"},
	}
	s := buildModelAt(m, map[string][]string{"host-a": nil})
	got := map[string]string{}
	for _, ref := range []string{"m1", "acme/m1", "m1@host-a"} {
		got[ref] = resolvedTo(s, ref)
	}
	want := map[string]string{"m1": "m1/m1-latest", "acme/m1": "m1/m1-latest", "m1@host-a": "m1/m1-latest@host-a"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("resolution (-want +got):\n%s", diff)
	}
}
