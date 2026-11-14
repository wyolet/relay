package audit

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/authz"
)

func TestMarkDecision(t *testing.T) {
	allow := func(a string) decision { return decision{Action: a, Status: StatusAllowed} }
	deny := func(a string) decision { return decision{Action: a, Status: StatusDenied} }

	tests := []struct {
		name       string
		decisions  []decision
		readRoute  bool
		wantMarked bool
		wantAction string
		wantStatus string
	}{
		{name: "no authorize calls", decisions: nil},
		{
			name:       "allowed mutation marks",
			decisions:  []decision{allow("policies.update")},
			wantMarked: true, wantAction: "policies.update", wantStatus: StatusAllowed,
		},
		{
			name:       "denied mutation marks",
			decisions:  []decision{deny("policies.delete")},
			wantMarked: true, wantAction: "policies.delete", wantStatus: StatusDenied,
		},
		{
			name:       "erroring mutation marks",
			decisions:  []decision{{Action: "policies.create", Status: StatusError}},
			wantMarked: true, wantAction: "policies.create", wantStatus: StatusError,
		},
		{
			name:      "allowed read on a read route does not mark",
			decisions: []decision{allow("policies.read")}, readRoute: true,
		},
		{
			name:      "allowed list on a read route does not mark",
			decisions: []decision{allow("policies.list")}, readRoute: true,
		},
		{
			name:       "denied read on a read route marks",
			decisions:  []decision{deny("settings.read")},
			readRoute:  true,
			wantMarked: true, wantAction: "settings.read", wantStatus: StatusDenied,
		},
		{
			name:      "usage probe: denied global read then allowed project get does not mark",
			decisions: []decision{deny("usage.read"), allow("usage.get")},
			readRoute: true,
		},
		{
			name:       "last mutating decision wins",
			decisions:  []decision{allow("keys.read"), deny("keys.update")},
			wantMarked: true, wantAction: "keys.update", wantStatus: StatusDenied,
		},
		{
			name:       "mutating decision wins over a later read",
			decisions:  []decision{deny("host-keys.rotate"), allow("host-keys.read")},
			wantMarked: true, wantAction: "host-keys.rotate", wantStatus: StatusDenied,
		},
		{
			name:       "denial off a read route marks even without a mutating verb",
			decisions:  []decision{deny("debug.snapshot")},
			wantMarked: true, wantAction: "debug.snapshot", wantStatus: StatusDenied,
		},
		{
			name:      "allowed non-mutating call off a read route does not mark",
			decisions: []decision{allow("debug.snapshot")},
		},
		{
			name:       "every mutating verb marks",
			decisions:  []decision{allow("system.reload")},
			wantMarked: true, wantAction: "system.reload", wantStatus: StatusAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, marked := markDecision(tt.decisions, tt.readRoute)
			if marked != tt.wantMarked {
				t.Fatalf("marked = %v, want %v", marked, tt.wantMarked)
			}
			if !marked {
				return
			}
			if got.Action != tt.wantAction || got.Status != tt.wantStatus {
				t.Fatalf("decision = %q/%q, want %q/%q", got.Action, got.Status, tt.wantAction, tt.wantStatus)
			}
		})
	}
}

func TestMarkDecisionCoversEveryMutatingVerb(t *testing.T) {
	for verb := range mutatingVerbs {
		if _, marked := markDecision([]decision{{Action: "thing." + verb, Status: StatusAllowed}}, true); !marked {
			t.Errorf("verb %q on a read route: not marked", verb)
		}
	}
}

func TestRefusedRoute(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		code       int
		wantMarked bool
		wantAction string
		wantKind   string
		wantID     string
	}{
		{
			name: "PUT by-id hidden by visibility", method: http.MethodPut,
			path: "/api/policies/by-id/p-1", code: http.StatusNotFound,
			wantMarked: true, wantAction: "policies.update", wantKind: "policy", wantID: "p-1",
		},
		{
			name: "DELETE by-id forbidden", method: http.MethodDelete,
			path: "/api/keys/by-id/k-9", code: http.StatusForbidden,
			wantMarked: true, wantAction: "keys.delete", wantKind: "key", wantID: "k-9",
		},
		{
			name: "POST rotate hidden by visibility", method: http.MethodPost,
			path: "/api/host-keys/by-id/h-2/rotate", code: http.StatusNotFound,
			wantMarked: true, wantAction: "host-keys.rotate", wantKind: "host-key", wantID: "h-2",
		},
		{
			name: "PUT sub-resource attach", method: http.MethodPut,
			path: "/api/policies/by-id/p-1/keys/k-2/attach", code: http.StatusForbidden,
			wantMarked: true, wantAction: "policies.attach", wantKind: "policy", wantID: "p-1",
		},
		{
			name: "POST collection", method: http.MethodPost,
			path: "/api/policies", code: http.StatusUnauthorized,
			wantMarked: true, wantAction: "policies.create", wantKind: "policy",
		},
		{
			name: "POST sub-resource with no trailing verb is a create", method: http.MethodPost,
			path: "/api/policies/by-id/p-1/keys", code: http.StatusNotFound,
			wantMarked: true, wantAction: "policies.create", wantKind: "policy", wantID: "p-1",
		},
		{
			name: "path without the /api mount prefix", method: http.MethodDelete,
			path: "/policies/by-id/p-1", code: http.StatusNotFound,
			wantMarked: true, wantAction: "policies.delete", wantKind: "policy", wantID: "p-1",
		},
		{
			name: "GET 404 earns no row", method: http.MethodGet,
			path: "/api/policies/by-id/p-1", code: http.StatusNotFound,
		},
		{
			name: "non-refusal status earns no row", method: http.MethodPut,
			path: "/api/policies/by-id/p-1", code: http.StatusInternalServerError,
		},
		{
			name: "success earns no row", method: http.MethodDelete,
			path: "/api/policies/by-id/p-1", code: http.StatusNoContent,
		},
		{
			name: "root path earns no row", method: http.MethodPost,
			path: "/api", code: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, marked := refusedRoute(tt.method, tt.path, tt.code)
			if marked != tt.wantMarked {
				t.Fatalf("marked = %v, want %v", marked, tt.wantMarked)
			}
			if !marked {
				return
			}
			if got.Action != tt.wantAction || got.Resource.Kind != tt.wantKind || got.Resource.ID != tt.wantID {
				t.Fatalf("decision = %q %s/%s, want %q %s/%s",
					got.Action, got.Resource.Kind, got.Resource.ID,
					tt.wantAction, tt.wantKind, tt.wantID)
			}
			if got.Status != StatusDenied {
				t.Fatalf("status = %q, want denied", got.Status)
			}
		})
	}
}

// The fallback only fires when nothing authorized: a request that reached
// Authorize keeps the decision it recorded.
func TestRefusedRouteDoesNotOverrideARecordedDecision(t *testing.T) {
	f := &inflight{request: Request{Method: http.MethodDelete, Path: "/api/policies/by-id/p-1"}}
	f.add(decision{Action: "policies.delete", Resource: Resource{Kind: "policy", ID: "p-1"}, Status: StatusAllowed})
	ev, ok := f.event(http.StatusNoContent)
	if !ok || ev.Resource.Kind != "policy" || ev.Outcome.Status != StatusAllowed {
		t.Fatalf("event = %+v (marked %v), want the recorded allowed decision", ev, ok)
	}
}

// A fallback row's resource.kind must read the same as a handler-recorded
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

// The token routes hang off /auth, so the path-derived fallback has to name
// them explicitly or a refused mint is recorded as auth.create.
func TestRefusedRouteNamesTheTokenActions(t *testing.T) {
	for _, tc := range []struct {
		method, path, wantAction, wantID string
	}{
		{http.MethodPost, "/api/auth/token", "tokens.mint", ""},
		{http.MethodPost, "/api/auth/token/revoke", "tokens.revoke", ""},
		{http.MethodDelete, "/api/auth/token/j-1", "tokens.revoke", "j-1"},
		{http.MethodPost, "/api/auth/token/revoke-all", "tokens.revoke-all", ""},
		{http.MethodPost, "/api/auth/token/keys/rotate", "tokens.rotate", ""},
	} {
		got, marked := refusedRoute(tc.method, tc.path, http.StatusUnauthorized)
		if !marked {
			t.Errorf("%s %s earned no row", tc.method, tc.path)
			continue
		}
		if got.Action != tc.wantAction {
			t.Errorf("%s %s action = %q, want %q", tc.method, tc.path, got.Action, tc.wantAction)
		}
		if got.Resource.Kind != "token" {
			t.Errorf("%s %s kind = %q, want token", tc.method, tc.path, got.Resource.Kind)
		}
		if got.Resource.ID != tc.wantID {
			t.Errorf("%s %s id = %q, want %q", tc.method, tc.path, got.Resource.ID, tc.wantID)
		}
	}
}
