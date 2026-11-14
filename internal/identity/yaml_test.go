package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/password"
)

// user renders one User document; spec is the body of its spec mapping.
func user(name, spec string) string {
	return "apiVersion: relay.wyolet.dev/v1\nkind: User\nmetadata: {name: " + name + "}\nspec:\n" + spec + "\n"
}

const validSpec = "  username: alice\n  email: alice@example.com\n  password: longenoughpw\n  roles: [admin, viewer]"

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(t.TempDir(), "pw.txt")
	if err := os.WriteFile(pwFile, []byte("hunter2hunter2\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := password.Hash("bcrypt-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_USERNAME", "bob")
	t.Setenv("RELAY_TEST_HASH", hash)

	// Two users in one multi-document file beside another kind, one in a nested .yml, and a file with another extension that must not be read.
	writeFile(t, dir, "users.yaml", user("zed", validSpec)+"---\n"+
		"apiVersion: relay.wyolet.dev/v1\nkind: Provider\nmetadata: {name: openai}\nspec: {kind: openai}\n---\n"+
		user("bob", "  username: {valueFrom: {env: RELAY_TEST_USERNAME}}\n  email: {value: bob@example.com}\n  password: {valueFrom: {file: "+pwFile+"}}"))
	writeFile(t, dir, "nested/hashed.yml", user("carol", "  username: carol\n  email: Carol <carol@example.com>\n  password: {valueFrom: {env: RELAY_TEST_HASH}}"))
	writeFile(t, dir, "ignored.txt", "kind: User\nthis is not: [valid")

	store, err := LoadYAML(dir)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ name, username, usernameSrc, email, pw, pwSrc, roles string }
	var got []row
	for _, u := range store.Users() {
		got = append(got, row{u.Metadata.Name, u.Spec.Username.Get(), u.Spec.Username.Source(), u.Spec.Email.Get(), u.Spec.Password.Get(), u.Spec.Password.Source(), strings.Join(u.Spec.Roles, ",")})
	}
	want := []row{
		{"bob", "bob", "env:RELAY_TEST_USERNAME", "bob@example.com", "hunter2hunter2", "file:" + pwFile, ""},
		{"carol", "carol", "literal", "Carol <carol@example.com>", hash, "env:RELAY_TEST_HASH", ""},
		{"zed", "alice", "literal", "alice@example.com", "longenoughpw", "literal", "admin,viewer"},
	}
	if len(got) != len(want) {
		t.Fatalf("users = %+v, want %d sorted by name", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("user %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Lookup is by metadata name or by resolved username, never one for the other.
	if u, ok := store.ByName("zed"); !ok || u.Spec.Username.Get() != "alice" {
		t.Errorf("ByName(zed) = %v, %v", u, ok)
	}
	if u, ok := store.ByUsername("alice"); !ok || u.Metadata.Name != "zed" {
		t.Errorf("ByUsername(alice) = %v, %v", u, ok)
	}
	for _, miss := range []string{"alice", ""} {
		if u, ok := store.ByName(miss); ok {
			t.Errorf("ByName(%q) = %v, want a miss", miss, u)
		}
	}
	for _, miss := range []string{"zed", ""} {
		if u, ok := store.ByUsername(miss); ok {
			t.Errorf("ByUsername(%q) = %v, want a miss", miss, u)
		}
	}

	bob, _ := store.ByName("bob")
	carol, _ := store.ByName("carol")
	for _, tc := range []struct {
		name string
		u    *User
		pw   string
		want bool
	}{
		{"plain password", bob, "hunter2hunter2", true},
		{"plain password with the file's newline", bob, "hunter2hunter2\n", false},
		{"plain wrong", bob, "hunter2", false},
		{"bcrypt password", carol, "bcrypt-secret", true},
		{"bcrypt hash itself is not the password", carol, hash, false},
		{"empty password", bob, "", false},
		{"nil user", nil, "hunter2hunter2", false},
		{"user whose secrets were never resolved", &User{Spec: UserSpec{Password: SecretRef{Value: "longenoughpw"}}}, "longenoughpw", false},
	} {
		if got := Verify(tc.u, tc.pw); got != tc.want {
			t.Errorf("Verify %s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoadYAML_EmptyStores(t *testing.T) {
	for name, dir := range map[string]string{"missing directory": filepath.Join(t.TempDir(), "absent"), "empty directory": t.TempDir()} {
		store, err := LoadYAML(dir)
		if err != nil || len(store.Users()) != 0 {
			t.Errorf("%s: %d users, err %v; want an empty store", name, len(store.Users()), err)
		}
	}
}

func TestLoadYAML_Rejects(t *testing.T) {
	spec := func(username, email, pw string) string {
		return "  username: " + username + "\n  email: " + email + "\n  password: " + pw
	}
	tests := []struct {
		name  string
		files map[string]string
		want  string // error with the temp dir cut off the front; a trailing "…" matches by prefix (library text follows)
	}{
		{"value and valueFrom together", map[string]string{"u.yaml": user("bad", spec("{value: x, valueFrom: {env: FOO}}", "x@y.com", "longenoughpw"))},
			`User "bad".spec.username: set either value or valueFrom, not both`},
		{"neither value nor valueFrom", map[string]string{"u.yaml": user("bad", spec("alice", "{}", "longenoughpw"))},
			`User "bad".spec.email: missing value or valueFrom`},
		{"field absent", map[string]string{"u.yaml": user("bad", "  username: alice\n  email: x@y.com")},
			`User "bad".spec.password: missing value or valueFrom`},
		{"env and file together", map[string]string{"u.yaml": user("bad", spec("alice", "x@y.com", "{valueFrom: {env: A, file: /b}}"))},
			`User "bad".spec.password: valueFrom must specify exactly one of env or file`},
		{"env variable unset", map[string]string{"u.yaml": user("bad", spec("{valueFrom: {env: RELAY_DEFINITELY_NOT_SET_XYZ}}", "x@y.com", "longenoughpw"))},
			`User "bad".spec.username: env var "RELAY_DEFINITELY_NOT_SET_XYZ" not set or empty`},
		{"env variable empty", map[string]string{"u.yaml": user("bad", spec("{valueFrom: {env: RELAY_TEST_EMPTY}}", "x@y.com", "longenoughpw"))},
			`User "bad".spec.username: env var "RELAY_TEST_EMPTY" not set or empty`},
		{"secret file missing", map[string]string{"u.yaml": user("bad", spec("alice", "x@y.com", "{valueFrom: {file: /no/such/file}}"))},
			`User "bad".spec.password: read file "/no/such/file": …`},
		{"email not an address", map[string]string{"u.yaml": user("bad", spec("alice", "not-an-email", "longenoughpw"))},
			`User "bad": invalid email: …`},
		{"password under 8 characters", map[string]string{"u.yaml": user("bad", spec("alice", "x@y.com", "7chars!"))},
			`User "bad": password must be at least 8 characters`},
		{"published placeholder password", map[string]string{"u.yaml": user("bad", spec("alice", "x@y.com", "{valueFrom: {env: RELAY_TEST_PLACEHOLDER}}"))},
			`User "bad": password is a published placeholder (env:RELAY_TEST_PLACEHOLDER); set a real one`},
		{"wrong apiVersion", map[string]string{"u.yaml": strings.Replace(user("bad", validSpec), "relay.wyolet.dev/v1", "relay.wyolet.dev/v2", 1)},
			`/u.yaml: User "bad": unsupported apiVersion "relay.wyolet.dev/v2" (want "relay.wyolet.dev/v1")`},
		{"metadata.name missing", map[string]string{"u.yaml": "apiVersion: relay.wyolet.dev/v1\nkind: User\nspec:\n" + validSpec + "\n"},
			`/u.yaml: User: metadata.name required`},
		{"secret given as a list", map[string]string{"u.yaml": user("bad", spec("[a, b]", "x@y.com", "longenoughpw"))},
			`/u.yaml: User "bad": SecretRef: expected string or {value,valueFrom} mapping: …`},
		{"same name twice in one file", map[string]string{"u.yaml": user("dup", validSpec) + "---\n" + user("dup", validSpec)},
			`/u.yaml: duplicate User "dup"`},
		{"same name in two files", map[string]string{"a.yaml": user("dup", validSpec), "b.yaml": user("dup", validSpec)},
			`/b.yaml: duplicate User "dup"`},
		{"malformed second document", map[string]string{"u.yaml": user("ok", validSpec) + "---\nkind: [unclosed\n"},
			"/u.yaml [doc 1]: yaml: …"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("RELAY_TEST_EMPTY", "")
			t.Setenv("RELAY_TEST_PLACEHOLDER", "change-me-please")
			for name, body := range tc.files {
				writeFile(t, dir, name, body)
			}
			store, err := LoadYAML(dir)
			if store != nil || err == nil {
				t.Fatalf("LoadYAML = %v, %v; want nil and an error", store, err)
			}
			got := strings.TrimPrefix(err.Error(), dir)
			if prefix, ok := strings.CutSuffix(tc.want, "…"); ok && strings.HasPrefix(got, prefix) {
				return
			}
			if got != tc.want {
				t.Errorf("error = %q\n     want %q", got, tc.want)
			}
		})
	}

	// Two users resolving to one username: which of the pair is named first depends on map order.
	dir := t.TempDir()
	writeFile(t, dir, "u.yaml", user("a", validSpec)+"---\n"+user("b", validSpec))
	if _, err := LoadYAML(dir); err == nil || !strings.Contains(err.Error(), `duplicate username "alice" (already used by User `) {
		t.Errorf("duplicate username: err = %v", err)
	}
	for _, pw := range []string{"change-me", "CHANGEME", "password"} {
		dir := t.TempDir()
		writeFile(t, dir, "u.yaml", user("bad", spec("alice", "x@y.com", pw)))
		if _, err := LoadYAML(dir); err == nil || err.Error() != `User "bad": password is a published placeholder (literal); set a real one` {
			t.Errorf("placeholder %q: err = %v", pw, err)
		}
	}
}

func TestSecretRef_GetBeforeResolvePanics(t *testing.T) {
	defer func() {
		if r := recover(); r != "identity.SecretRef.Get: not resolved (call Resolve first)" {
			t.Errorf("recovered %v, want the not-resolved panic", r)
		}
	}()
	ref := SecretRef{Value: "x"}
	if ref.IsLiteral() || ref.Source() != "" {
		t.Errorf("unresolved ref: literal %v source %q", ref.IsLiteral(), ref.Source())
	}
	_ = ref.Get()
}
