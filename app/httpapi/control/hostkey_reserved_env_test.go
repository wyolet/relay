package control

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/ids"
)

// The refusal holds for the admin token too: no caller may point a key at relay's own configuration, on create or update.
func TestHostKeyRefusesReservedEnvRef(t *testing.T) {
	h := newSharedCatalogHarness(t)
	hostID, policyID := ids.New(), ids.New()
	body := func(env string) string {
		return fmt.Sprintf(`{"metadata":{"name":"env-key"},"spec":{"hostId":%q,"policyId":%q,"valueFrom":{"kind":"env","env":%q}}}`, hostID, policyID, env)
	}

	if w := scopeReq(t, h, "token", http.MethodPost, "/host-keys", body("RELAY_MASTER_KEY")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reserved") {
		t.Fatalf("create with a reserved env ref = %d %s, want 400", w.Code, w.Body)
	}
	w := scopeReq(t, h, "token", http.MethodPost, "/host-keys", body("OPENAI_API_KEY"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create with an allowed env ref = %d: %s", w.Code, w.Body)
	}
	id := createdID(t, w.Body)
	for _, env := range []string{"RELAY_ADMIN_TOKEN", "WYOLET_OIDC_CLIENT_SECRET"} {
		w := scopeReq(t, h, "token", http.MethodPut, "/host-keys/by-id/"+id, body(env))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reserved") {
			t.Errorf("update to %s = %d %s, want 400", env, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), env) {
			t.Errorf("update to %s: response echoes the variable name: %s", env, w.Body)
		}
	}
	if w := scopeReq(t, h, "token", http.MethodPut, "/host-keys/by-id/"+id, body("ACME_API_KEY")); w.Code != http.StatusOK {
		t.Errorf("update to an allowed env ref = %d: %s", w.Code, w.Body)
	}
}
