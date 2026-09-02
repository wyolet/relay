// mode_matrix_test.go runs one CRUD table under both authorization modes.
// RELAY_AUTHZ=single is the upgrade path every existing deployment lands
// on, so "single behaves exactly like it did before scoping" has to be an
// assertion, not a reading of cmd/relay's two-line wiring.
package control

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/internal/config"
)

// authorizerFor mirrors the composition root's choice: anything but
// AuthzRBAC is the permissive single-user authorizer.
func authorizerFor(mode string) authz.Authorizer {
	if mode == config.AuthzRBAC {
		return testRBAC()
	}
	return authz.AlwaysAllowAuthenticated{}
}

func TestModeMatrix(t *testing.T) {
	// alice owns aliceID; bobID and catalogID are rows she does not own.
	routes := []struct {
		name         string
		method       string
		path         string
		body         string
		single, rbac int
	}{
		{"list", http.MethodGet, "/rate-limits", "", 200, 200},
		{"get own", http.MethodGet, "/rate-limits/" + aliceID, "", 200, 200},
		{"get foreign", http.MethodGet, "/rate-limits/" + bobID, "", 200, 404},
		{"get catalog row", http.MethodGet, "/rate-limits/" + catalogID, "", 200, 200},
		{"create personal", http.MethodPost, "/rate-limits",
			`{"metadata":{"name":"mine","displayName":"Mine"}}`, 201, 201},
		{"create catalog-owned", http.MethodPost, "/rate-limits",
			`{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`, 201, 403},
		{"update own", http.MethodPut, "/rate-limits/by-id/" + aliceID,
			`{"metadata":{"name":"renamed"}}`, 200, 200},
		{"update foreign", http.MethodPut, "/rate-limits/by-id/" + bobID,
			`{"metadata":{"name":"renamed"}}`, 200, 404},
		{"update catalog row", http.MethodPut, "/rate-limits/by-id/" + catalogID,
			`{"metadata":{"name":"renamed"}}`, 200, 403},
		{"delete own", http.MethodDelete, "/rate-limits/by-id/" + aliceID, "", 204, 204},
		{"delete foreign", http.MethodDelete, "/rate-limits/by-id/" + bobID, "", 204, 404},
		// Governance is not an authorization mode: deleting a catalog-managed
		// row is refused in both, and for the same reason.
		{"delete catalog row", http.MethodDelete, "/rate-limits/by-id/" + catalogID, "", 403, 403},
	}

	for _, mode := range []string{config.AuthzSingle, config.AuthzRBAC} {
		t.Run(mode, func(t *testing.T) {
			for _, rt := range routes {
				t.Run(rt.name, func(t *testing.T) {
					// A fresh store per case: the delete rows would
					// otherwise disappear for the ones after them.
					h, _ := newScopeHarness(t, authorizerFor(mode), seedThings()...)
					want := rt.single
					if mode == config.AuthzRBAC {
						want = rt.rbac
					}
					w := scopeReq(t, h, "alice", rt.method, rt.path, rt.body)
					if w.Code != want {
						t.Fatalf("%s %s under %s = %d, want %d: %s",
							rt.method, rt.path, mode, w.Code, want, w.Body)
					}
				})
			}
		})
	}
}

// Single mode still needs a session: it drops scoping, not authentication.
func TestSingleModeStillRequiresAnActor(t *testing.T) {
	h, _ := newScopeHarness(t, authorizerFor(config.AuthzSingle), seedThings()...)
	if w := scopeReq(t, h, "", http.MethodGet, "/rate-limits", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", w.Code)
	}
}
