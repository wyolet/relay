package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/user"
)

func userDocs(t *testing.T, src string) []manifest.Document {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func userYAML(name, roles, disabled string) string {
	return fmt.Sprintf("apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: %[1]s}\nspec: {email: %[1]s@example.com, roles: [%[2]s], disabled: %[3]s}\n",
		name, roles, disabled)
}

func usersBuilder(rows []*user.User, b *builder) *builder {
	r := &Rows{Users: rows}
	b.rows, b.idx = r, newIndex(r)
	if b.opts.Stores == nil {
		b.opts.Stores = &Stores{}
	}
	return b
}

func TestPlanUsersDiffsByUsername(t *testing.T) {
	stored := []*user.User{{ID: "u-1", Username: "alice", Email: "alice@example.com", Roles: []string{"admin"}}}
	b := usersBuilder(stored, &builder{admin: true})
	src := userYAML("alice", "admin", "false") + "---\n" + userYAML("bob", "", "true")
	if err := b.run(context.Background(), userDocs(t, src)); err != nil {
		t.Fatal(err)
	}
	if len(b.entries) != 2 {
		t.Fatalf("entries = %+v", b.entries)
	}
	alice, bob := b.entries[0], b.entries[1]
	if alice.Action != ActionUnchanged || alice.ID != "u-1" || alice.write != nil {
		t.Errorf("alice = %+v", alice)
	}
	if bob.Action != ActionCreate || bob.ID == "" || bob.plural != "users" || bob.write == nil {
		t.Errorf("bob = %+v", bob)
	}
}

// users.update is not a grant to hand out roles, here as on the users endpoint.
func TestPlanUsersRefusesARoleChangeFromANonAdmin(t *testing.T) {
	stored := []*user.User{{ID: "u-1", Username: "alice", Roles: []string{"admin"}}, {ID: "u-2", Username: "bob"}}
	b := usersBuilder(stored, &builder{opts: Options{Authz: seesOnly{}}})
	err := b.run(context.Background(), userDocs(t, userYAML("bob", "admin", "false")))
	var ae *AuthzError
	if !errors.As(err, &ae) || ae.Entry.Kind != "User" {
		t.Fatalf("err = %v, want an AuthzError on the user", err)
	}
	// An edit that leaves the roles alone is not refused here; the authorizer decides it.
	b = usersBuilder(stored, &builder{opts: Options{Authz: seesOnly{}}})
	if err := b.run(context.Background(), userDocs(t, userYAML("bob", "", "true"))); err != nil {
		t.Fatalf("a non-admin edit without a role change: %v", err)
	}
}

// Only a loader running as the operator reads a password from its environment; the API refuses one.
func TestPlanUsersReadsEnvPasswordsOnlyWhenAllowed(t *testing.T) {
	t.Setenv("RELAY_TEST_APPLY_PASSWORD", "a long test password")
	src := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: erin}\nspec: {password: {valueFrom: {env: RELAY_TEST_APPLY_PASSWORD}}}\n"
	b := usersBuilder(nil, &builder{admin: true})
	if err := b.run(context.Background(), userDocs(t, src)); !errors.Is(err, errEnvPassword) {
		t.Fatalf("err = %v, want the env-password refusal", err)
	}
	b = usersBuilder(nil, &builder{admin: true, opts: Options{EnvPasswords: true}})
	if err := b.run(context.Background(), userDocs(t, src)); err != nil || len(b.entries) != 1 || b.entries[0].Action != ActionCreate {
		t.Fatalf("with EnvPasswords: err = %v, entries = %+v", err, b.entries)
	}
}

func TestPlanUsersKeepsOneEnabledAdmin(t *testing.T) {
	stored := []*user.User{{ID: "u-1", Username: "alice", Roles: []string{"admin"}}, {ID: "u-2", Username: "bob"}}
	b := usersBuilder(stored, &builder{admin: true})
	err := b.run(context.Background(), userDocs(t, userYAML("alice", "admin", "true")))
	if !errors.Is(err, errLastAdmin) {
		t.Fatalf("err = %v, want the last-admin refusal", err)
	}
	b = usersBuilder(stored, &builder{admin: true})
	src := userYAML("alice", "admin", "true") + "---\n" + userYAML("bob", "admin", "false")
	if err := b.run(context.Background(), userDocs(t, src)); err != nil {
		t.Fatalf("handing the role over in the same bundle: %v", err)
	}
}
