//go:build integration

package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// The catalog NOTIFY triggers are the relay's and live on its tables, so a write through the auth module's store must still fire them for every kind the snapshot reads.
func TestDirectoryStoreWritesFireCatalogNotify(t *testing.T) {
	pool := storagetest.Pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN catalog_events"); err != nil {
		t.Fatal(err)
	}

	s := authpg.New(pool)
	sys := authz.Owner{Kind: directory.OwnerSystem}
	u := &directory.User{ID: directory.NewID(), Username: "notify-user"}
	team := &directory.Team{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-team", Owner: sys}}
	proj := &directory.Project{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-project"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	sa := &directory.ServiceAccount{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-sa"}, Spec: directory.ServiceAccountSpec{ProjectID: proj.Meta.ID}}
	grp := &directory.Group{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-group", Owner: sys}, Spec: directory.GroupSpec{MemberIDs: []string{u.ID}}}
	role := &directory.Role{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-role", Owner: sys},
		Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}}}
	rb := &directory.RoleBinding{Meta: directory.Metadata{ID: directory.NewID(), Name: "notify-binding"},
		Spec: directory.RoleBindingSpec{RoleID: role.Meta.ID, Scope: directory.Global, Subjects: []directory.Subject{{Kind: directory.SubjectUser, ID: u.ID}}}}

	steps := []struct {
		write func() error
		want  string
	}{
		{func() error { return s.Users.Upsert(ctx, u) }, "user:upsert:" + u.ID},
		{func() error { return s.Teams.Upsert(ctx, team) }, "team:upsert:" + team.Meta.ID},
		{func() error { return s.Projects.Upsert(ctx, proj) }, "project:upsert:" + proj.Meta.ID},
		{func() error { return s.ServiceAccounts.Upsert(ctx, sa) }, "serviceaccount:upsert:" + sa.Meta.ID},
		{func() error { return s.Groups.Upsert(ctx, grp) }, "group:upsert:" + grp.Meta.ID},
		{func() error { return s.Roles.Upsert(ctx, role) }, "role:upsert:" + role.Meta.ID},
		{func() error { return s.RoleBindings.Upsert(ctx, rb) }, "rolebinding:upsert:" + rb.Meta.ID},
		{func() error { return s.Users.BumpTokenVersion(ctx, u.ID) }, "user:upsert:" + u.ID},
		{func() error { return s.RoleBindings.Delete(ctx, rb.Meta.ID) }, "rolebinding:delete:" + rb.Meta.ID},
		{func() error { return s.Teams.Delete(ctx, team.Meta.ID) }, "team:delete:" + team.Meta.ID},
	}
	for _, step := range steps {
		if err := step.write(); err != nil {
			t.Fatalf("write for %s: %v", step.want, err)
		}
		if !awaitNotify(ctx, t, conn.Conn(), step.want) {
			t.Fatalf("no %q notification after the write", step.want)
		}
	}
}

// awaitNotify reads notifications until one carries want; others (junction rows, cascades) are skipped.
func awaitNotify(ctx context.Context, t *testing.T, conn *pgx.Conn, want string) bool {
	t.Helper()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return false
		}
		if n.Payload == want {
			return true
		}
	}
}
