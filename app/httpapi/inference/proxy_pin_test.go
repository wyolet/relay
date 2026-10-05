package inference

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

// proxyPinCatalog holds a keyed host granted to policy "keyed-only" and a keyless host granted only to policy "local".
func proxyPinCatalog(t *testing.T) (*catalog.Catalog, *apphost.Host, *apphost.Host) {
	t.Helper()
	provID, hostID, hkID, modID, keyedPolID, localPolID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	keyed := &apphost.Host{Meta: meta.Metadata{ID: hostID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: apphost.Spec{BaseURL: "http://upstream.invalid"}}
	keyless := &apphost.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "local-llm", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: apphost.Spec{BaseURL: "http://local.invalid", NoAuth: true}}
	hk := &hostkey.HostKey{Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}, Spec: hostkey.Spec{HostID: hostID, PolicyID: keyedPolID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}}
	m := &model.Model{Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")}}
	onKeyed := &binding.Binding{Meta: meta.Metadata{ID: meta.NewID(), Name: "tm-acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI}}
	onKeyless := &binding.Binding{Meta: meta.Metadata{ID: meta.NewID(), Name: "tm-local", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: keyless.Meta.ID, Adapter: adapters.OpenAI}}
	keyedOnly := &policy.Policy{Meta: meta.Metadata{ID: keyedPolID, Name: "keyed-only", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policy.Spec{Models: []string{"acme/test-model@acme"}, HostKeyIDs: []string{hkID}}}
	local := &policy.Policy{Meta: meta.Metadata{ID: localPolID, Name: "local", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policy.Spec{Models: []string{"acme/test-model@local-llm"}}}
	wildcard := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "wildcard", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policy.Spec{HostKeyIDs: []string{hkID}}}
	cat := catalog.New(provListD{prov}, hostListD{keyed, keyless}, polListD{keyedOnly, local, wildcard}, modListD{m}, keyListD{hk}, rlListD{}, rkListD{}, rcListD{}, bndListD{onKeyed, onKeyless})
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	return cat, keyed, keyless
}

func passthroughPrincipal(t *testing.T, cat *catalog.Catalog, policyName string) *Principal {
	t.Helper()
	pol, ok := cat.Current().PolicyByName(policyName)
	if !ok {
		t.Fatalf("policy %q missing", policyName)
	}
	return &Principal{CredentialKind: CredentialKey, Key: &key.Key{}, Policy: pol, PassthroughAllowed: true}
}

// A keyless host ignores the forwarded credential, so a host pin reaches one only through an explicit grant, as normal-mode routing requires.
func TestProxyPinAllowed_KeylessHostNeedsAGrant(t *testing.T) {
	cat, keyed, keyless := proxyPinCatalog(t)
	snap := cat.Current()

	for _, tc := range []struct {
		name      string
		principal *Principal
		host      *apphost.Host
		want      bool
	}{
		{"anonymous on keyless", nil, keyless, false},
		{"out-of-policy key on keyless", passthroughPrincipal(t, cat, "keyed-only"), keyless, false},
		{"implicit wildcard on keyless", passthroughPrincipal(t, cat, "wildcard"), keyless, false},
		{"granted key on keyless", passthroughPrincipal(t, cat, "local"), keyless, true},
		{"anonymous on keyed", nil, keyed, true},
		{"any key on keyed", passthroughPrincipal(t, cat, "local"), keyed, true},
	} {
		if got := proxyPinAllowed(snap, tc.principal, tc.host); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The host listing offers exactly the hosts the caller may pin.
func TestProxyHostsFor_HidesUngrantedKeylessHosts(t *testing.T) {
	cat, _, _ := proxyPinCatalog(t)
	snap := cat.Current()
	slugs := func(p *Principal) []string {
		var out []string
		for _, e := range proxyHostsFor(snap, p) {
			out = append(out, e.Slug)
		}
		return out
	}
	if got := slugs(nil); len(got) != 1 || got[0] != "acme" {
		t.Errorf("anonymous sees %v, want [acme]", got)
	}
	if got := slugs(passthroughPrincipal(t, cat, "local")); len(got) != 2 {
		t.Errorf("granted key sees %v, want both hosts", got)
	}
}
