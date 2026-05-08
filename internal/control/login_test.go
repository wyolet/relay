package control

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/internal/identity"
)

func mustStore(t *testing.T) *identity.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RELAY_TEST_PW", "hunter2hunter2")
	body := `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: admin}
spec:
  username: admin
  email: admin@example.com
  password: {valueFrom: {env: RELAY_TEST_PW}}
  roles: [admin]
`
	if err := os.WriteFile(filepath.Join(dir, "u.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := identity.LoadYAML(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidateLogin_Success(t *testing.T) {
	u, err := ValidateLogin(mustStore(t), "admin", "hunter2hunter2")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Metadata.Name != "admin" {
		t.Errorf("name=%q", u.Metadata.Name)
	}
}

func TestValidateLogin_BadPassword(t *testing.T) {
	_, err := ValidateLogin(mustStore(t), "admin", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateLogin_UnknownUser(t *testing.T) {
	_, err := ValidateLogin(mustStore(t), "nobody", "hunter2hunter2")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateLogin_NilStore(t *testing.T) {
	_, err := ValidateLogin(nil, "admin", "x")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err=%v", err)
	}
}
