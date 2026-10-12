//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

// productRows are rows as the product writes them today: the same documents the golden tests pin, inserted by hand.
const productRows = `
INSERT INTO users (id, username, email, password_hash, oidc_subject, roles, disabled)
VALUES ('u-1', 'ada', 'ada@example.com', NULL, 'https://idp.example.com|sub-1', '{admin}', false);
INSERT INTO teams (id, name, display_name, metadata, spec)
VALUES ('t-1', 'core', 'Core', '{"description":"the core team","owner":{"kind":"system"},"labels":{"tier":"gold"},"annotations":{"note":"seeded"},"dirty":true}',
        '{"enabled":false,"budget":{"amount":"10.00","period":"month"}}');
INSERT INTO projects (id, name, display_name, team_id, metadata, spec)
VALUES ('p-1', 'api', 'API', 't-1', '{"owner":{"kind":"team","id":"t-1"}}',
        '{"teamId":"t-1","enabled":false,"budget":{"amount":"5","period":"week","onExceed":"warn"}}');
INSERT INTO service_accounts (id, name, display_name, project_id, metadata, spec)
VALUES ('sa-1', 'ci', 'CI', 'p-1', '{"owner":{"kind":"project","id":"p-1"}}', '{"projectId":"p-1","policyId":"pol-1","enabled":false}');
INSERT INTO groups (id, name, display_name, metadata, spec)
VALUES ('g-1', 'ops', 'Ops', '{"owner":{"kind":"user","id":"u-1"}}', '{"enabled":false}');
INSERT INTO group_members (group_id, user_id, position) VALUES ('g-1', 'u-1', 0);
INSERT INTO roles (id, name, display_name, metadata, spec)
VALUES ('r-1', 'reader', 'Reader', '{"owner":{"kind":"system"}}', '{"rules":[{"kinds":["documents"],"verbs":["get","list"]}]}');
INSERT INTO role_bindings (id, name, display_name, role_id, scope_kind, scope_id, metadata, spec)
VALUES ('b-1', 'readers', 'Readers', 'r-1', 'project', 'p-1', '{"owner":{"kind":"project","id":"p-1"}}',
        '{"roleId":"","scope":{},"subjects":null,"enabled":false}'),
       ('b-2', 'everyone', 'Everyone', 'r-1', 'system', NULL, '{"owner":{"kind":"system"}}', '{"roleId":"","scope":{},"subjects":null}');
INSERT INTO role_binding_subjects (binding_id, kind, subject_id, subject_name, subject_user_id, subject_sa_id, position)
VALUES ('b-1', 'user', 'u-1', NULL, 'u-1', NULL, 0),
       ('b-1', 'serviceaccount', 'sa-1', NULL, NULL, 'sa-1', 1),
       ('b-1', 'group', NULL, 'ops', NULL, NULL, 2),
       ('b-2', 'group', NULL, 'system:authenticated', NULL, NULL, 0);
`

// documents snapshots every JSONB document and junction row, so a read-and-write-back can be compared against the original.
const documents = `
SELECT 'teams ' || id || ' ' || metadata::text || ' ' || spec::text FROM teams
UNION ALL SELECT 'projects ' || id || ' ' || metadata::text || ' ' || spec::text FROM projects
UNION ALL SELECT 'service_accounts ' || id || ' ' || metadata::text || ' ' || spec::text FROM service_accounts
UNION ALL SELECT 'groups ' || id || ' ' || metadata::text || ' ' || spec::text FROM groups
UNION ALL SELECT 'roles ' || id || ' ' || metadata::text || ' ' || spec::text FROM roles
UNION ALL SELECT 'role_bindings ' || id || ' ' || scope_kind || ' ' || coalesce(scope_id, '-') || ' ' || metadata::text || ' ' || spec::text FROM role_bindings
UNION ALL SELECT 'members ' || group_id || ' ' || user_id || ' ' || position FROM group_members
UNION ALL SELECT 'subjects ' || binding_id || ' ' || position || ' ' || kind || ' ' || coalesce(subject_id, '-') || ' ' ||
          coalesce(subject_name, '-') || ' ' || coalesce(subject_user_id, '-') || ' ' || coalesce(subject_sa_id, '-') FROM role_binding_subjects
UNION ALL SELECT 'users ' || id || ' ' || username || ' ' || coalesce(email, '-') || ' ' || coalesce(oidc_subject, '-') || ' ' ||
          roles::text || ' ' || disabled || ' ' || token_version FROM users
ORDER BY 1`

func snapshotDocuments(t *testing.T, dsn string) []string {
	t.Helper()
	rows, err := openPool(t, dsn).Query(context.Background(), documents)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestProductRowsRoundTrip reads every row the product wrote, writes each back unchanged through the module, and requires every document and junction row to come out as it went in.
func TestProductRowsRoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := emptyDB(t)
	migrateProduct(t, dsn)
	if err := Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	pool := openPool(t, dsn)
	if _, err := pool.Exec(ctx, productRows); err != nil {
		t.Fatal(err)
	}
	before := snapshotDocuments(t, dsn)
	s := New(pool)

	u, err := s.Users.ByIdentity(ctx, directory.Identity{Issuer: "https://idp.example.com", Subject: "sub-1"})
	if err != nil || u == nil || u.ID != "u-1" || u.Roles[0] != "admin" {
		t.Fatalf("user by identity = %+v, %v", u, err)
	}
	must(t, s.Users.Upsert(ctx, u))
	rewrite(t, s.Teams, func(x *directory.Team) {
		if string(x.Spec.Attributes["budget"]) != `{"amount":"10.00","period":"month"}` || !x.Meta.Dirty {
			t.Fatalf("team read as %+v", x)
		}
	})
	rewrite(t, s.Projects, nil)
	rewrite(t, s.ServiceAccounts, func(x *directory.ServiceAccount) {
		if !json.Valid(x.Spec.Attributes["policyId"]) || string(x.Spec.Attributes["policyId"]) != `"pol-1"` {
			t.Fatalf("service account read as %+v", x)
		}
	})
	rewrite(t, s.Groups, func(x *directory.Group) {
		if len(x.Spec.MemberIDs) != 1 || x.IsEnabled() {
			t.Fatalf("group read as %+v", x)
		}
	})
	rewrite(t, s.Roles, nil)
	rewrite(t, s.RoleBindings, func(x *directory.RoleBinding) {
		if x.Meta.ID == "b-2" && (x.Spec.Scope != directory.Global || x.Spec.Subjects[0] != directory.AllAuthenticated) {
			t.Fatalf("global binding read as %+v", x)
		}
		if x.Meta.ID == "b-1" && len(x.Spec.Subjects) != 3 {
			t.Fatalf("binding read as %+v", x)
		}
	})

	diff(t, "documents after write-back", snapshotDocuments(t, dsn), before)
}

// rewrite reads every row of a store, checks it, and writes it back at the version it read.
func rewrite[T any](t *testing.T, s directory.Store[T], check func(*T)) {
	t.Helper()
	ctx := context.Background()
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if check != nil {
			check(r)
		}
		must(t, s.Upsert(ctx, r))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
