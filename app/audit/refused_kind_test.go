package audit

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/authz"
)

// A fallback row's resource.kind must read the same as a handler-stamped
// one, or a UI filtering by kind has to sample both vocabularies. The
// plural table in app/authz is the single source.
func TestRefusedRouteKindMatchesTheHandlerVocabulary(t *testing.T) {
	families := []struct {
		method, path, wantAction, wantKind string
	}{
		{http.MethodPost, "/api/policies", "policies.create", "policy"},
		{http.MethodPut, "/api/keys/by-id/k-1", "keys.update", "key"},
		{http.MethodDelete, "/api/host-keys/by-id/h-1", "host-keys.delete", "host-key"},
		{http.MethodPost, "/api/keys/by-id/k-1/rotate", "keys.rotate", "key"},
		{http.MethodPut, "/api/policies/by-id/p-1/keys/k-1/attach", "policies.attach", "policy"},
		{http.MethodPost, "/api/service-accounts", "service-accounts.create", "service-account"},
		{http.MethodPost, "/api/role-bindings", "role-bindings.create", "role-binding"},
		{http.MethodPost, "/api/policy-bindings", "policy-bindings.create", "policy-binding"},
		{http.MethodDelete, "/api/rate-limits/by-id/r-1", "rate-limits.delete", "rate-limit"},
		{http.MethodPost, "/api/host-bindings", "host-bindings.create", "host-binding"},
		// Kinds that are already plural map to themselves.
		{http.MethodPut, "/api/settings/by-id/s-1", "settings.update", "settings"},
	}
	for _, f := range families {
		got, marked := refusedRoute(f.method, f.path, http.StatusForbidden)
		if !marked {
			t.Errorf("%s %s earned no row", f.method, f.path)
			continue
		}
		if got.Action != f.wantAction {
			t.Errorf("%s %s action = %q, want %q", f.method, f.path, got.Action, f.wantAction)
		}
		if got.Resource.Kind != f.wantKind {
			t.Errorf("%s %s kind = %q, want %q", f.method, f.path, got.Resource.Kind, f.wantKind)
		}
		// The action still names the plural, which is what a Role rule uses.
		if authz.Singular(pluralOfAction(got.Action)) != got.Resource.Kind {
			t.Errorf("%s %s: action plural and resource kind disagree (%q vs %q)",
				f.method, f.path, got.Action, got.Resource.Kind)
		}
	}
}

func pluralOfAction(action string) string {
	for i := len(action) - 1; i >= 0; i-- {
		if action[i] == '.' {
			return action[:i]
		}
	}
	return action
}
