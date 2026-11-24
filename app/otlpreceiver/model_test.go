package otlpreceiver

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// A reported "<snapshot>@<host>" whose model is not served at the host names no model, as routing answers it, rather than another model declaring the snapshot name as an alias.
func TestLookup_UnservedPinNeverFallsToAnotherModelsAlias(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}}
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "host-a", Owner: system}, Spec: host.Spec{BaseURL: "https://host-a.example"}}
	owner := meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}
	m1 := &model.Model{Meta: meta.Metadata{ID: meta.NewID(), Name: "m1", Owner: owner}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1"}, {Name: "m1-remote"}}, Pointer: "m1"}}
	m2 := &model.Model{Meta: meta.Metadata{ID: meta.NewID(), Name: "m2", Owner: owner}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m2"}}, Pointer: "m2", Aliases: []string{"m1"}}}
	snap := appcatalog.BuildCatalog([]*provider.Provider{prov}, []*host.Host{h}, []*model.Model{m1, m2}, nil, []*binding.Binding{
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-host-a", Owner: system}, Spec: binding.Spec{ModelID: m1.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI, Snapshots: []string{"m1-remote"}}},
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "m2-on-host-a", Owner: system}, Spec: binding.Spec{ModelID: m2.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI}},
	}, nil)

	got := map[string]string{}
	for _, ref := range []string{"m1", "m1@host-a", "m1-remote@host-a"} {
		if m, _, ok := lookup(snap, ref); ok {
			got[ref] = m.Meta.Name
		} else {
			got[ref] = ""
		}
	}
	if diff := cmp.Diff(map[string]string{"m1": "m1", "m1@host-a": "", "m1-remote@host-a": "m1"}, got); diff != "" {
		t.Errorf("lookup (-want +got):\n%s", diff)
	}
}
