package control

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/pkg/kv"
)

// A credential stored before placeholders were refused at load (a seeded
// row, or a bcrypt hash the loader cannot inspect) must still not log in.
func TestLoginRefusesPlaceholderPassword(t *testing.T) {
	const placeholder = "change-me-please"
	hash, err := user.HashPassword(placeholder)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	yaml := "apiVersion: relay.wyolet.dev/v1\nkind: User\nmetadata:\n  name: admin\nspec:\n  username: admin\n  email: admin@example.com\n  password: \"" + hash + "\"\n  roles: [admin]\n"
	if err := os.WriteFile(filepath.Join(dir, "admin.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	idStore, err := identity.LoadYAML(dir)
	if err != nil {
		t.Fatalf("identity.LoadYAML: %v", err)
	}
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })
	sessions := session.New(kvStore, false, "sess:")

	r := chi.NewRouter()
	r.Use(sessions.Middleware)
	api := humachi.New(r, huma.DefaultConfig("auth-placeholder-test", "0"))
	registerAuth(api, Deps{Identity: idStore, Sessions: sessions})

	w := authPost(t, r, "/auth/login", `{"username":"admin","password":"`+placeholder+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
}
