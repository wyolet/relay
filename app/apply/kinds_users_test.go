package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/user"
	authmanifest "github.com/wyolet/relay/auth/manifest"
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

// A password or identity the caller chose lets it sign in as the account, so only an admin sets one, on another account or on a new one; the plan refuses before anything is written.
func TestPlanUsersRefusesCredentialsFromANonAdmin(t *testing.T) {
	const (
		hash  = "$2a$10$CwTycUXWue0Thq9StjUM0uJ8.6Y3bOS2jvUJFXJ3m/BS/YwgMm9aW"
		other = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	)
	doc := func(name, spec string) string {
		return "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: " + name + "}\nspec: {" + spec + "}\n"
	}
	stored := func() []*user.User {
		return []*user.User{{ID: "u-1", Username: "alice", Roles: []string{"admin"}, PasswordHash: hash, OIDCSubject: "https://idp|alice"}}
	}
	for name, src := range map[string]string{
		"password changed":          doc("alice", "roles: [admin], password: {hash: '"+other+"'}"),
		"password cleared":          doc("alice", "roles: [admin], password: null"),
		"identity changed":          doc("alice", "roles: [admin], identity: {issuer: 'https://idp', subject: mallory}"),
		"identity cleared":          doc("alice", "roles: [admin], identity: null"),
		"new account with password": doc("mallory", "password: {hash: '"+hash+"'}"),
		"new account with identity": doc("mallory", "identity: {issuer: 'https://idp', subject: alice-2}"),
	} {
		b := usersBuilder(stored(), &builder{opts: Options{Authz: seesOnly{}}})
		err := b.run(context.Background(), userDocs(t, src))
		var ae *AuthzError
		if !errors.As(err, &ae) || ae.Entry.Kind != "User" || !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%s: err = %v, want a forbidden AuthzError on the user", name, err)
		}
		if len(b.entries) != 0 {
			t.Fatalf("%s: a refused plan kept entries: %+v", name, b.entries)
		}
		b = usersBuilder(stored(), &builder{admin: true})
		if err := b.run(context.Background(), userDocs(t, src)); err != nil || len(b.entries) != 1 || b.entries[0].write == nil {
			t.Fatalf("%s as an admin: err = %v, entries = %+v", name, err, b.entries)
		}
	}
	// Leaving the credentials out keeps them, which needs no admin.
	b := usersBuilder(stored(), &builder{opts: Options{Authz: seesOnly{}}})
	if err := b.run(context.Background(), userDocs(t, doc("alice", "roles: [admin], email: alice@example.com"))); err != nil {
		t.Fatalf("an edit that keeps the credentials: %v", err)
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

// Clearing the sign-in identity of an account the document leaves without a password is refused while planning, so the bundle writes nothing.
func TestPlanUsersRefusesAnIdentityClearWithoutAPassword(t *testing.T) {
	stored := []*user.User{{ID: "u-1", Username: "carol", OIDCSubject: "https://idp|c"}}
	b := usersBuilder(stored, &builder{admin: true})
	err := b.run(context.Background(), userDocs(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: carol}\nspec: {identity: null}\n"))
	var invalid *InvalidError
	if !errors.As(err, &invalid) || invalid.Name != "carol" || !errors.Is(err, authmanifest.ErrNoSignIn) {
		t.Fatalf("err = %v, want the identity clear refused on carol", err)
	}
	if len(b.entries) != 0 {
		t.Fatalf("a refused plan kept entries: %+v", b.entries)
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
