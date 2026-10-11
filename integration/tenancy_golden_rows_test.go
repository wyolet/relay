//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
)

// tenancyRowsQuery renders every row of the tables the auth module owns, server timestamps left out. JSONB prints in its canonical key order, so the rows compare whatever order a writer marshalled the keys in.
const tenancyRowsQuery = `
SELECT 'users ' || id || ' ' || username || ' ' || coalesce(email, '-') || ' ' || coalesce(password_hash, '-') || ' ' ||
       coalesce(oidc_subject, '-') || ' ' || roles::text || ' ' || disabled || ' ' || token_version FROM users
UNION ALL SELECT 'teams ' || id || ' ' || name || ' ' || display_name || ' ' || metadata::text || ' ' || spec::text || ' v' || resource_version FROM teams
UNION ALL SELECT 'projects ' || id || ' ' || name || ' ' || display_name || ' ' || team_id || ' ' || metadata::text || ' ' || spec::text || ' v' || resource_version FROM projects
UNION ALL SELECT 'service_accounts ' || id || ' ' || name || ' ' || display_name || ' ' || project_id || ' ' || metadata::text || ' ' || spec::text || ' v' || resource_version FROM service_accounts
UNION ALL SELECT 'groups ' || id || ' ' || name || ' ' || display_name || ' ' || metadata::text || ' ' || spec::text || ' v' || resource_version FROM groups
UNION ALL SELECT 'group_members ' || group_id || ' ' || position || ' ' || user_id FROM group_members
UNION ALL SELECT 'roles ' || id || ' ' || name || ' ' || display_name || ' ' || metadata::text || ' ' || spec::text || ' v' || resource_version FROM roles
UNION ALL SELECT 'role_bindings ' || id || ' ' || name || ' ' || display_name || ' ' || role_id || ' ' || scope_kind || ' ' || coalesce(scope_id, '-') || ' ' ||
          metadata::text || ' ' || spec::text || ' v' || resource_version FROM role_bindings
UNION ALL SELECT 'role_binding_subjects ' || binding_id || ' ' || position || ' ' || kind || ' ' || coalesce(subject_id, '-') || ' ' ||
          coalesce(subject_name, '-') || ' ' || coalesce(subject_user_id, '-') || ' ' || coalesce(subject_sa_id, '-') FROM role_binding_subjects`

// rowNames maps every id in the tenancy tables to "<table>:<name>", so two runs that minted different ids compare equal.
func rowNames(t *testing.T, st *stack) map[string]string {
	t.Helper()
	rows, err := testPool(t, st.dsn).Query(context.Background(), `
SELECT id, 'user:' || username FROM users
UNION ALL SELECT id, 'team:' || name FROM teams
UNION ALL SELECT id, 'project:' || name FROM projects
UNION ALL SELECT id, 'serviceaccount:' || name FROM service_accounts
UNION ALL SELECT id, 'group:' || name FROM groups
UNION ALL SELECT id, 'role:' || name FROM roles
UNION ALL SELECT id, 'rolebinding:' || name FROM role_bindings
UNION ALL SELECT id, 'policy:' || name FROM policies`)
	if err != nil {
		t.Fatalf("row names: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		out[id] = name
	}
	return out
}

func nameIDs(s string, names map[string]string) string {
	return uuidPattern.ReplaceAllStringFunc(s, func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return "<unknown-id>"
	})
}

func tenancyRows(t *testing.T, st *stack, names map[string]string) []string {
	t.Helper()
	rows, err := testPool(t, st.dsn).Query(context.Background(), tenancyRowsQuery)
	if err != nil {
		t.Fatalf("tenancy rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, "row "+nameIDs(s, names))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// decisionKinds and decisionVerbs span the kinds the bundle's roles name plus a few they do not.
var (
	decisionKinds = []string{"teams", "projects", "service-accounts", "groups", "roles", "role-bindings", "keys", "policies", "host-keys", "users", "usage", "audit", "models"}
	decisionVerbs = []string{"get", "list", "create", "update", "delete"}
)

// rbacDecisions evaluates every user against every kind, verb and owner scope under the RBAC authorizer over the live snapshot, and lists the allowed ones.
func rbacDecisions(t *testing.T, st *stack, names map[string]string) []string {
	t.Helper()
	ctx := context.Background()
	snap := st.cat.Current()
	az := authz.RBAC{Snap: func() authz.Snapshot { return st.cat.Current() }}
	byName := map[string]string{}
	for id, n := range names {
		byName[n] = id
	}
	owners := []meta.Owner{
		{Kind: meta.OwnerSystem},
		{Kind: meta.OwnerTeam, ID: byName["team:platform"]},
		{Kind: meta.OwnerTeam, ID: byName["team:research"]},
		{Kind: meta.OwnerProject, ID: byName["project:ml-search"]},
		{Kind: meta.OwnerProject, ID: byName["project:sandbox"]},
	}
	all, err := st.users.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, u := range all {
		a := &actor.Actor{UserID: u.ID, Username: u.Username, Roles: u.Roles,
			Subjects: appcatalog.UserSubjects(u.ID, snap.GroupsForUser(u.ID), nil)}
		actx := actor.WithActor(ctx, a)
		for _, kind := range decisionKinds {
			for _, verb := range decisionVerbs {
				for _, o := range append(owners, meta.Owner{Kind: meta.OwnerUser, ID: u.ID}) {
					owner := o
					res := authz.Resource{Kind: authz.Singular(kind), Owner: &owner}
					if az.Authorize(actx, kind+"."+verb, res) == nil {
						out = append(out, "allow "+u.Username+" "+kind+"."+verb+" "+nameIDs(string(o.Kind)+":"+o.ID, names))
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// diff fails with the lines only one side has; the golden is a set of lines, so order within a section does not matter.
func diff(t *testing.T, got, want []string) {
	t.Helper()
	var missing, extra []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("differs from the golden:\n missing:\n  %s\n unexpected:\n  %s", strings.Join(missing, "\n  "), strings.Join(extra, "\n  "))
	}
}
