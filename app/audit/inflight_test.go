package audit

import (
	"net/http"
	"testing"
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
