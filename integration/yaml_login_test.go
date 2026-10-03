//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"testing"
)

// yamlLoginStack boots a stack whose identity dir holds one YAML user, and
// returns a login attempt against it.
func yamlLoginStack(t *testing.T, mode string) (*stack, func() int) {
	t.Helper()
	dir := t.TempDir()
	yaml := "apiVersion: relay.wyolet.dev/v1\nkind: User\nmetadata:\n  name: yaml-op\n" +
		"spec:\n  username: yamlop\n  email: yamlop@example.test\n  password: 'yaml-pw-12345'\n"
	if err := os.WriteFile(filepath.Join(dir, "op.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newStackAuthz(t, mode, dir)
	login := func() int {
		jar, _ := cookiejar.New(nil)
		us := &userSession{t: t, base: st.control.URL, client: &http.Client{Jar: jar}}
		code, _ := us.do(http.MethodPost, "/api/auth/login", `{"username":"yamlop","password":"yaml-pw-12345"}`)
		return code
	}
	return st, login
}

// Disabling the user row locks out the YAML fallback too: the YAML file is
// a bootstrap credential, not a way around the operator's switch.
func TestIntegration_DisabledUserCannotLogInThroughYAML(t *testing.T) {
	st, login := yamlLoginStack(t, "")
	ctx := context.Background()
	row, err := st.users.ByUsername(ctx, "yamlop")
	if err != nil || row == nil {
		t.Fatalf("seeded row: %v", err)
	}
	row.Disabled = true
	if err := st.users.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	if code := login(); code != http.StatusUnauthorized {
		t.Fatalf("disabled user's YAML login = %d, want 401", code)
	}
}

// Under RBAC a session must carry a real user id: a YAML user whose row is
// gone would act under a slug no binding or owner names.
func TestIntegration_RBACYAMLUserWithoutRowGetsNoSession(t *testing.T) {
	st, login := yamlLoginStack(t, "rbac")
	ctx := context.Background()
	row, err := st.users.ByUsername(ctx, "yamlop")
	if err != nil || row == nil {
		t.Fatalf("seeded row: %v", err)
	}
	if err := st.users.Delete(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if code := login(); code != http.StatusUnauthorized {
		t.Fatalf("row-less YAML login under rbac = %d, want 401", code)
	}
}
