package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/pkg/meta"
)

const orgRowID = "01950000-0000-7000-8000-0000000000a9"

// orgThings is seedThings plus one organization-owned row.
func orgThings() []*scopedThing {
	return append(seedThings(), &scopedThing{Meta: meta.Metadata{ID: orgRowID, Name: "org-row", Owner: meta.Owner{Kind: meta.OwnerOrganization}}})
}

// An organization row is created, moved, edited and seen by an admin alone. Anyone else is refused the write with 403 and reads it as missing; without a scoping authorizer every caller is the operator, and governance still keeps its edits to an admin.
func TestOrganizationOwnerByMode(t *testing.T) {
	const (
		orgBody   = `{"metadata":{"name":"org-cap","owner":{"kind":"organization"}}}`
		intoOrg   = `{"metadata":{"name":"alice-row","owner":{"kind":"organization"}}}`
		rename    = `{"metadata":{"name":"renamed"}}`
		notAdmin  = "authz: forbidden: an organization-owned policy requires an admin"
		orgEdit   = "policy is owned by the organization: only an admin may edit it"
		orgDelete = "policy is owned by the organization: only an admin may delete it"
		reserved  = "owner.kind=system is reserved for seed; omit owner.kind on create"
		byID      = "/policies/by-id/"
	)
	type outcome struct {
		status int
		msg    string
	}
	ok := func(status int) outcome { return outcome{status: status} }
	hidden := outcome{404, `policy "` + orgRowID + `" not found`}
	hiddenID := outcome{404, `policy with id "` + orgRowID + `" not found`}

	for _, tc := range []struct {
		name, who, method, path, body string
		single, rbac                  outcome
	}{
		{"admin creates", "root", "POST", "/policies", orgBody, ok(201), ok(201)},
		{"admin token creates", "token", "POST", "/policies", orgBody, ok(201), ok(201)},
		{"a user creates", "alice", "POST", "/policies", orgBody, ok(201), outcome{403, notAdmin}},
		{"system stays reserved", "root", "POST", "/policies", `{"metadata":{"name":"sys","owner":{"kind":"system"}}}`, outcome{400, reserved}, outcome{400, reserved}},

		{"admin gets", "root", "GET", "/policies/" + orgRowID, "", ok(200), ok(200)},
		{"a user gets", "alice", "GET", "/policies/" + orgRowID, "", ok(200), hidden},

		{"admin moves a row in", "root", "PUT", byID + aliceID, intoOrg, ok(200), ok(200)},
		{"a user moves their row in", "alice", "PUT", byID + aliceID, intoOrg, ok(200), outcome{403, notAdmin}},
		{"admin edits", "root", "PUT", byID + orgRowID, rename, ok(200), ok(200)},
		{"a user edits", "alice", "PUT", byID + orgRowID, rename, outcome{403, orgEdit}, hiddenID},
		{"admin deletes", "root", "DELETE", byID + orgRowID, "", ok(204), ok(204)},
		{"a user deletes", "alice", "DELETE", byID + orgRowID, "", outcome{403, orgDelete}, hiddenID},
	} {
		for mode, want := range map[string]outcome{"single": tc.single, "rbac": tc.rbac} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var authzr authz.Authorizer = authz.AlwaysAllowAuthenticated{}
				if mode == "rbac" {
					authzr = testRBAC()
				}
				h, store := newThingHarness(t, authzr, "policies", "policy", noSettings{}, orgThings()...)
				before := newMemStore(thingMeta, orgThings()...).items
				w := scopeReq(t, h, tc.who, tc.method, tc.path, tc.body)
				if want.msg != "" {
					wantAPIError(t, w, want.status, "", want.msg)
					if diff := cmp.Diff(before, store.items); diff != "" {
						t.Fatalf("a refused %s changed the store (-before +after):\n%s", tc.method, diff)
					}
					return
				}
				if w.Code != want.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, want.status, w.Body)
				}
				if tc.method == "POST" || (tc.method == "PUT" && tc.body == intoOrg) {
					if got := decodeMeta(t, w, want.status); got.Owner != (meta.Owner{Kind: meta.OwnerOrganization}) {
						t.Fatalf("owner = %+v, want the organization", got.Owner)
					}
				}
			})
		}
	}
}

// A list shows organization rows to an admin and leaves them out, uncounted, for anyone else.
func TestOrganizationRowsListedForAdminsOnly(t *testing.T) {
	for _, tc := range []struct {
		who  string
		want []string
	}{
		{"alice", []string{"alice-row", "catalog-row", "system-row"}},
		{"root", []string{"alice-row", "org-row", "bob-row", "catalog-row", "project-row", "system-row"}},
		{"token", []string{"alice-row", "org-row", "bob-row", "catalog-row", "project-row", "system-row"}},
	} {
		t.Run(tc.who, func(t *testing.T) {
			h, _ := newThingHarness(t, testRBAC(), "policies", "policy", noSettings{}, orgThings()...)
			var out struct {
				Items []scopedThing `json:"items"`
				Total int           `json:"total"`
			}
			w := scopeReq(t, h, tc.who, http.MethodGet, "/policies", "")
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
				t.Fatalf("list = %d (%v): %s", w.Code, err, w.Body)
			}
			var got []string
			for _, it := range out.Items {
				got = append(got, it.Meta.Name)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" || out.Total != len(tc.want) {
				t.Fatalf("total %d, names (-want +got):\n%s", out.Total, diff)
			}
		})
	}
}

// Only policies, rate limits, budgets and pools take an organization owner; the kind is checked before the caller, so an admin hears the same 400.
func TestOrganizationOwnerKinds(t *testing.T) {
	body := `{"metadata":{"name":"org-row","owner":{"kind":"organization"}}}`
	for _, tc := range []struct {
		plural, singular string
		status           int
		msg              string
	}{
		{"policies", "policy", 201, ""},
		{"rate-limits", "rate-limit", 201, ""},
		{"budgets", "budget", 201, ""},
		{"pools", "pool", 201, ""},
		{"host-keys", "host-key", 400, "host-key cannot be owned by the organization; only policies, rate limits, budgets and pools can"},
		{"accounts", "account", 400, "account cannot be owned by the organization; only policies, rate limits, budgets and pools can"},
		{"hosts", "host", 400, "host cannot be owned by the organization; only policies, rate limits, budgets and pools can"},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			h, store := newThingHarness(t, testRBAC(), tc.plural, tc.singular, noSettings{})
			w := scopeReq(t, h, "root", http.MethodPost, "/"+tc.plural, body)
			if tc.msg != "" {
				wantAPIError(t, w, tc.status, "", tc.msg)
				if len(store.items) != 0 {
					t.Fatal("a refused create stored a row")
				}
				return
			}
			if got := decodeMeta(t, w, tc.status); got.Owner != (meta.Owner{Kind: meta.OwnerOrganization}) {
				t.Fatalf("owner = %+v, want the organization", got.Owner)
			}
		})
	}
}

// The owner check policy duplication shares with create: a 400 for a kind the organization owns no rows of, a 403 for a non-admin under a scoping authorizer.
func TestRefuseOrganizationOwner(t *testing.T) {
	org := meta.Owner{Kind: meta.OwnerOrganization}
	for _, tc := range []struct {
		name     string
		who      string
		authzr   authz.Authorizer
		singular string
		owner    meta.Owner
		status   int
	}{
		{"admin, policy", "root", testRBAC(), "policy", org, 0},
		{"admin token, policy", "token", testRBAC(), "policy", org, 0},
		{"user, policy", "alice", testRBAC(), "policy", org, 403},
		{"user, single mode", "alice", authz.AlwaysAllowAuthenticated{}, "policy", org, 0},
		{"admin, policy binding", "root", testRBAC(), "policy-binding", org, 400},
		{"user, policy binding", "alice", testRBAC(), "policy-binding", org, 400},
		{"another owner is not judged", "alice", testRBAC(), "host-key", meta.Owner{Kind: meta.OwnerUser}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseOrganizationOwner(actor.WithActor(context.Background(), scopeActors[tc.who]), tc.authzr, tc.singular, tc.owner)
			if got := statusOf(t, err); got != tc.status {
				t.Fatalf("refuseOrganizationOwner = %v (status %d), want status %d", err, got, tc.status)
			}
		})
	}
}

// usable_by names the organization the way it names the system.
func TestParseScopeOrganization(t *testing.T) {
	for in, want := range map[string]meta.Owner{
		"organization": {Kind: meta.OwnerOrganization},
		"system":       {Kind: meta.OwnerSystem},
		"team:t-1":     {Kind: meta.OwnerTeam, ID: "t-1"},
	} {
		if got, err := parseScope(in); err != nil || got != want {
			t.Errorf("parseScope(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := parseScope("organization:o-1"); err == nil {
		t.Error("parseScope accepted an organization id")
	}
}
