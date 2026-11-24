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

// pinWorld builds catalogs of provider acme's models bound at named hosts, for resolution tests that must also hold on catalogs built before bindings could narrow pins.
type pinWorld struct {
	prov     *provider.Provider
	hosts    map[string]*host.Host
	models   []*model.Model
	bindings []*binding.Binding
}

func newPinWorld() *pinWorld {
	return &pinWorld{prov: &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}, hosts: map[string]*host.Host{}}
}

func (w *pinWorld) model(name string, snapshots []string, aliases ...string) *model.Model {
	m := &model.Model{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: w.prov.Meta.ID}}, Spec: model.Spec{Pointer: snapshots[0], Aliases: aliases}}
	for _, s := range snapshots {
		m.Spec.Snapshots = append(m.Spec.Snapshots, model.Snapshot{Name: s})
	}
	w.models = append(w.models, m)
	return m
}

func (w *pinWorld) bind(m *model.Model, hostName string, snapshots ...string) {
	h, ok := w.hosts[hostName]
	if !ok {
		h = &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://" + hostName + ".example"}}
		w.hosts[hostName] = h
	}
	w.bindings = append(w.bindings, &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: m.Meta.Name + "-on-" + hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI, Snapshots: snapshots},
	})
}

// resolve names what each ref resolves to as model/snapshot@pinned-host, "" for a miss.
func (w *pinWorld) resolve(refs ...string) map[string]string {
	var hosts []*host.Host
	for _, h := range w.hosts {
		hosts = append(hosts, h)
	}
	s := BuildCatalog([]*provider.Provider{w.prov}, hosts, w.models, nil, w.bindings, nil)
	out := map[string]string{}
	for _, ref := range refs {
		res := s.ResolveModelRef(ref)
		if len(res.Models) == 0 {
			out[ref] = ""
			continue
		}
		out[ref] = res.Models[0].Meta.Name + "/" + res.Snapshot.Name
		if h, ok := s.HostSlug(res.PinHostID); ok {
			out[ref] += "@" + h
		}
	}
	return out
}

// Unchanged behaviour: a pinned name that never was a snapshot pin resolves through an alias as it always has, in each of the states below.
func TestResolveModelRef_UnchangedAliasPins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(w *pinWorld)
		want  map[string]string
	}{
		{
			name: "a snapshot name pinned where its model has no binding is another model's alias pin",
			build: func(w *pinWorld) {
				w.bind(w.model("a", []string{"foo"}), "h1")
				w.bind(w.model("b", []string{"b"}, "foo"), "h2")
			},
			want: map[string]string{"foo": "a/foo", "foo@h1": "a/foo@h1", "foo@h2": "b/b@h2", "acme/foo@h2": "b/b@h2"},
		},
		{
			name: "an exact alias containing @",
			build: func(w *pinWorld) {
				w.bind(w.model("m1", []string{"m1"}, "m1@20250514"), "h1")
			},
			want: map[string]string{"m1@20250514": "m1/m1", "m1@h1": "m1/m1@h1"},
		},
		{
			name: "an alias that reads like a snapshot pin key",
			build: func(w *pinWorld) {
				w.bind(w.model("a", []string{"m1"}), "east")
				w.bind(w.model("b", []string{"b"}, "m1-east"), "h2")
			},
			want: map[string]string{"m1@east": "a/m1@east", "m1-east@h2": "b/b@h2", "acme/m1-east@h2": "b/b@h2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPinWorld()
			tc.build(w)
			var refs []string
			for ref := range tc.want {
				refs = append(refs, ref)
			}
			if diff := cmp.Diff(tc.want, w.resolve(refs...)); diff != "" {
				t.Errorf("resolution (-want +got):\n%s", diff)
			}
		})
	}
}
