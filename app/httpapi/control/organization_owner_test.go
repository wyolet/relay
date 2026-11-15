package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

const orgRowID = "01950000-0000-7000-8000-0000000000a9"

// orgThings is seedThings plus one organization-owned row.
func orgThings() []*scopedThing {
	return append(seedThings(), &scopedThing{Meta: meta.Metadata{ID: orgRowID, Name: "org-row", Owner: meta.Owner{Kind: meta.OwnerOrganization}}})
}

// An organization row is created, moved, edited and seen by an admin alone. Anyone else is refused the write with 403 and, under a scoping authorizer, reads it as missing; without one every caller reads it, and an admin alone writes it, as POST /apply has it.
func TestOrganizationOwnerByMode(t *testing.T) {
	const (
		orgBody   = `{"metadata":{"name":"org-cap","owner":{"kind":"organization"}}}`
		intoOrg   = `{"metadata":{"name":"alice-row","owner":{"kind":"organization"}}}`
		rename    = `{"metadata":{"name":"renamed"}}`
		notAdmin  = "an organization-owned policy requires an admin"
		orgEdit   = "policy is owned by the organization: only an admin may edit it"
		orgDelete = "policy is owned by the organization: only an admin may delete it"
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
		{"a user creates", "alice", "POST", "/policies", orgBody, outcome{403, notAdmin}, outcome{403, notAdmin}},
		// A body from before the organization owner: system names it, for an admin alone.
		{"system names the organization", "root", "POST", "/policies", `{"metadata":{"name":"sys","owner":{"kind":"system"}}}`, ok(201), ok(201)},
		{"a user's system body", "alice", "POST", "/policies", `{"metadata":{"name":"sys","owner":{"kind":"system"}}}`, outcome{403, notAdmin}, outcome{403, notAdmin}},
		{"an admin's system body keeps an organization row", "root", "PUT", byID + orgRowID, `{"metadata":{"name":"org-row","owner":{"kind":"system"}}}`, ok(200), ok(200)},

		{"admin gets", "root", "GET", "/policies/" + orgRowID, "", ok(200), ok(200)},
		{"a user gets", "alice", "GET", "/policies/" + orgRowID, "", ok(200), hidden},

		{"admin moves a row in", "root", "PUT", byID + aliceID, intoOrg, ok(200), ok(200)},
		{"a user moves their row in", "alice", "PUT", byID + aliceID, intoOrg, outcome{403, notAdmin}, outcome{403, notAdmin}},
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
				if tc.method == "POST" || (tc.method == "PUT" && strings.Contains(tc.body, `"owner"`)) {
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

// The deployment's own kinds take an organization owner and the catalog's and the derived ones refuse it; the kind is checked before the caller, so an admin hears the same 400.
func TestOrganizationOwnerKinds(t *testing.T) {
	body := `{"metadata":{"name":"org-row","owner":{"kind":"organization"}}}`
	refused := func(singular string) string { return singular + " cannot be owned by the organization" }
	for _, tc := range []struct {
		plural, singular string
		status           int
		msg              string
	}{
		{"policies", "policy", 201, ""},
		{"rate-limits", "rate-limit", 201, ""},
		{"budgets", "budget", 201, ""},
		{"pools", "pool", 201, ""},
		{"host-keys", "host-key", 201, ""},
		{"accounts", "account", 201, ""},
		{"teams", "team", 201, ""},
		{"groups", "group", 201, ""},
		{"roles", "role", 201, ""},
		{"policy-bindings", "policy-binding", 201, ""},
		{"hosts", "host", 400, refused("host")},
		{"models", "model", 400, refused("model")},
		{"keys", "key", 400, refused("key")},
		{"projects", "project", 400, refused("project")},
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

// The owner check policy duplication shares with create: a 400 for a kind the organization owns no rows of, a 403 for a non-admin in either mode on the admin-only kinds, and nothing for the rest, whose grant the authorizer decides.
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
		{"user, single mode", "alice", authz.AlwaysAllowAuthenticated{}, "policy", org, 403},
		{"admin, single mode", "root", authz.AlwaysAllowAuthenticated{}, "policy", org, 0},
		{"global admin binding", "granted", globalAdminRBAC(t), "policy", org, 0},
		{"admin, host", "root", testRBAC(), "host", org, 400},
		{"user, policy binding", "alice", testRBAC(), "policy-binding", org, 0},
		{"user, team", "alice", testRBAC(), "team", org, 0},
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

// globalAdminRBAC is testRBAC with the built-in roles and a global binding of the admin role to the "granted" actor.
func globalAdminRBAC(t *testing.T) authz.RBAC {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	on := true
	b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "granted-admin", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	for _, r := range builtins {
		r.Meta.ID, r.Spec.Enabled = ids.New(), &on
		if r.Meta.Name == role.BuiltinAdmin {
			b.Spec.RoleID = r.Meta.ID
		}
	}
	b.Spec.Scope, b.Spec.Enabled = meta.Owner{Kind: meta.OwnerSystem}, &on
	b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: scopeActors["granted"].UserID}}
	cat := catalogtest.Catalog{Roles: builtins, RoleBindings: []*rolebinding.RoleBinding{b}}.Load(t)
	return authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
}

// A user the admin role is bound to globally writes and governs organization rows as the admin role does.
func TestOrganizationRowsByGlobalAdminBinding(t *testing.T) {
	h, store := newThingHarness(t, globalAdminRBAC(t), "policies", "policy", noSettings{}, orgThings()...)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/policies", `{"metadata":{"name":"org-cap","owner":{"kind":"organization"}}}`},
		{"PUT", "/policies/by-id/" + aliceID, `{"metadata":{"name":"alice-row","owner":{"kind":"organization"}}}`},
		{"PUT", "/policies/by-id/" + orgRowID, `{"metadata":{"name":"renamed"}}`},
		{"DELETE", "/policies/by-id/" + orgRowID, ""},
	} {
		if w := scopeReq(t, h, "granted", tc.method, tc.path, tc.body); w.Code >= 300 {
			t.Fatalf("%s %s = %d: %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	if got := store.items[aliceID].Meta.Owner; got != (meta.Owner{Kind: meta.OwnerOrganization}) {
		t.Fatalf("alice-row owner = %+v, want the organization", got)
	}
}

// A row the catalog dropped is governed as a user's; moved into the organization it loses that mark and is an admin's alone.
func TestDetachedRowMovedIntoOrganization(t *testing.T) {
	detached := time.Now()
	rows := append(seedThings(), &scopedThing{Meta: meta.Metadata{ID: orgRowID, Name: "dropped", Owner: meta.Owner{Kind: meta.OwnerSystem}, CatalogDetachedAt: &detached}})
	h, store := newThingHarness(t, authz.AlwaysAllowAuthenticated{}, "policies", "policy", noSettings{}, rows...)
	if w := scopeReq(t, h, "root", "PUT", "/policies/by-id/"+orgRowID, `{"metadata":{"name":"dropped","owner":{"kind":"organization"}}}`); w.Code != 200 {
		t.Fatalf("admin move = %d: %s", w.Code, w.Body)
	}
	if m := store.items[orgRowID].Meta; m.Owner.Kind != meta.OwnerOrganization || m.CatalogDetachedAt != nil {
		t.Fatalf("moved row owner %+v, detached at %v; want the organization and no mark", m.Owner, m.CatalogDetachedAt)
	}
	wantAPIError(t, scopeReq(t, h, "alice", "PUT", "/policies/by-id/"+orgRowID, `{"metadata":{"name":"renamed"}}`), 403, "", "policy is owned by the organization: only an admin may edit it")
	wantAPIError(t, scopeReq(t, h, "alice", "DELETE", "/policies/by-id/"+orgRowID, ""), 403, "", "policy is owned by the organization: only an admin may delete it")
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
