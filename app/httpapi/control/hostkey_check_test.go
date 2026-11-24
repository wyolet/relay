package control

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// The check of a raw value tells the host's answers apart by what the host did, and names the host but never the value.
func TestCheckKeyValue(t *testing.T) {
	answers := map[string]int{"sk-good": http.StatusOK, "sk-bare": http.StatusOK, "sk-revoked": http.StatusUnauthorized, "sk-banned": http.StatusForbidden, "sk-busy": http.StatusTooManyRequests}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")[len("Bearer "):]
		if v == "sk-good" {
			w.Header().Set("X-Org-Id", "org-a")
			w.Header().Set("X-Org-Name", "Acme")
		}
		w.WriteHeader(answers[v])
	}))
	defer up.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "check-provider", Owner: system}}
	m := &model.Model{Meta: meta.Metadata{ID: meta.NewID(), Name: "check-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "check-model"}}, Pointer: "check-model"}}
	newHost := func(name, url string) *host.Host {
		return &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: system}, Spec: host.Spec{BaseURL: url}}
	}
	probed, down, bare := newHost("probed", up.URL), newHost("down", gone.URL), newHost("bare", up.URL)
	var bindings []*binding.Binding
	for _, h := range []*host.Host{probed, down} {
		bindings = append(bindings, &binding.Binding{Meta: meta.Metadata{ID: meta.NewID(), Name: h.Meta.Name + "-binding", Owner: system}, Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI}})
	}
	snap := catalogtest.Catalog{Providers: []*provider.Provider{prov}, Models: []*model.Model{m}, Hosts: []*host.Host{probed, down, bare}, Bindings: bindings}.Snapshot()
	specs := adapter.NewRegistry((&adapter.Spec{
		Name: adapters.OpenAI,
		Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Org:  &adapter.OrgProbe{Path: "/v1/models", OrgHeader: "X-Org-Id", NameHeader: "X-Org-Name"},
	}).Build())

	for _, tc := range []struct {
		name, hostID, value string
		want                keyCheck
	}{
		{"detected", probed.Meta.ID, "sk-good", keyCheck{Outcome: keyDetected, Org: adapter.Org{ID: "org-a", Name: "Acme"}}},
		{"answered without an organization", probed.Meta.ID, "sk-bare", keyCheck{Outcome: keyUnsupported, Reason: `host "probed" answered without naming an organization`}},
		{"401", probed.Meta.ID, "sk-revoked", keyCheck{Outcome: keyRefused, Status: 401, Reason: `host "probed" answered 401`}},
		{"403", probed.Meta.ID, "sk-banned", keyCheck{Outcome: keyRefused, Status: 403, Reason: `host "probed" answered 403`}},
		{"another status", probed.Meta.ID, "sk-busy", keyCheck{Outcome: keyUnreachable, Status: 429, Reason: `host "probed" answered 429`}},
		{"no shape that asks", bare.Meta.ID, "sk-good", keyCheck{Outcome: keyUnsupported, Reason: `host "bare" does not name the organization a key belongs to`}},
		{"a host not loaded", meta.NewID(), "sk-good", keyCheck{Outcome: keyUnsupported, Reason: "the key's host is not loaded or has no base URL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, checkKeyValue(t.Context(), snap, specs, tc.hostID, tc.value)); diff != "" {
				t.Errorf("check (-want +got):\n%s", diff)
			}
		})
	}

	got := checkKeyValue(t.Context(), snap, specs, down.Meta.ID, "sk-good")
	if got.Outcome != keyUnreachable || got.Status != 0 || got.Reason == "" {
		t.Errorf("nothing listening = %+v, want unreachable with no status", got)
	}
}
