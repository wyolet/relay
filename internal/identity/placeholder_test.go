package identity

import (
	"strings"
	"testing"
)

// The admin User the root docker-compose.yml mounts, with its password read
// from the environment.
const envAdminUserYAML = `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: { name: admin }
spec:
  username: admin
  email: admin@localhost
  password: { valueFrom: { env: RELAY_ADMIN_PASSWORD } }
  roles: [admin]
`

func TestLoadYAML_RejectsPlaceholderPassword(t *testing.T) {
	for _, pw := range []string{"change-me-please", "change-me", "CHANGEME", "password"} {
		t.Run(pw, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "admin.yaml", envAdminUserYAML)
			t.Setenv("RELAY_ADMIN_PASSWORD", pw)

			_, err := LoadYAML(dir)
			if err == nil || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("LoadYAML with %q: err = %v, want placeholder refusal", pw, err)
			}
		})
	}
}

func TestLoadYAML_AcceptsRealPassword(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "admin.yaml", envAdminUserYAML)
	t.Setenv("RELAY_ADMIN_PASSWORD", "c0rrect-horse-battery")

	store, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	u, ok := store.ByUsername("admin")
	if !ok || !Verify(u, "c0rrect-horse-battery") {
		t.Fatal("admin with a real password should load and verify")
	}
}
