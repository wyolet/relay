package inference

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

// buildDispatchCatalog creates a catalog with a model bound to the given
// hostName (Meta.Name) with the provided adapter. Returns the catalog and
// the relay key that authorises access. An optional Capabilities value is
// applied to the model.
func buildDispatchCatalog(t *testing.T, hostName string, hostAdapter adapters.Name, caps ...model.Capabilities) (*catalog.Catalog, *Principal) {
	t.Helper()

	provID := meta.NewID()
	hostID := meta.NewID()
	hkID := meta.NewID()
	modID := meta.NewID()
	polID := meta.NewID()
	rkID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: hostName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.invalid"},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: slug.From("test-model")}},
			Pointer:   slug.From("test-model"),
		},
	}
	if len(caps) > 0 {
		m.Spec.Capabilities = caps[0]
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: hostAdapter},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}},
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: rkID, Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "testhash"},
	}
	// The edge resolves the key to a principal before anything downstream
	// runs; tests start from that same resolved state.
	pr := &Principal{
		CredentialKind: CredentialKey,
		CredentialID:   k.Meta.ID,
		KeyHash:        k.Spec.KeyHash,
		Key:            k,
		Policy:         pol,
	}

	cat := catalogtest.Catalog{
		Providers: []*provider.Provider{prov},
		Hosts:     []*host.Host{h},
		Policies:  []*policy.Policy{pol},
		Models:    []*model.Model{m},
		HostKeys:  []*hostkey.HostKey{hk},
		Keys:      []*key.Key{k},
		Bindings:  []*binding.Binding{b},
	}.Load(t)
	return cat, pr
}

// pointHostAt re-points the catalog's only host at upstreamURL with NoAuth,
// so a request reaches a test server on the anonymous key and no secret is
// resolved.
func pointHostAt(t *testing.T, cat *catalog.Catalog, upstreamURL string) {
	t.Helper()
	hosts := cat.Current().Hosts()
	if len(hosts) != 1 {
		t.Fatalf("fixture hosts: %d", len(hosts))
	}
	h := *hosts[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
}
