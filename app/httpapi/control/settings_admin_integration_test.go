//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/secret"
)

// Under the default single-user authorizer every signed-in user holds
// settings.update; the sections that resolve a secret toward a destination
// they name still take an admin.
func TestSecretSettingsNeedAnAdmin(t *testing.T) {
	ctx := t.Context()
	pool := storagetest.Pool(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	deps := mountDeps(t)
	deps.Stores = stores
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", map[string]*actor.Actor{"bob": {UserID: "u-bob", Username: "bob", SessionID: "s"}}))
	Mount(r, deps)

	put := func(section, body string, admin bool) int {
		req := httptest.NewRequest(http.MethodPut, "/settings/"+section, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if admin {
			req.Header.Set("Authorization", "Bearer test-admin-token")
		} else {
			req.Header.Set("X-Test-User", "bob")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	oidc := `{"enabled":true,"issuer":"https://idp.example.com","clientId":"c","clientSecretEnv":"RELAY_OIDC_CLIENT_SECRET","redirectUrl":"https://relay.example.com/api/auth/oidc/callback"}`
	raw, _ := json.Marshal(settings.PayloadLogging{Enabled: true, Backend: "s3", S3: settings.PayloadS3{
		Endpoint: "s3.example.com", Bucket: "b", UseSSL: true,
		AccessKey: secret.Ref{Kind: secret.KindEnv, Env: "RELAY_PAYLOAD_S3_ACCESS_KEY"},
		SecretKey: secret.Ref{Kind: secret.KindEnv, Env: "RELAY_PAYLOAD_S3_SECRET_KEY"},
	}})
	payload := string(raw)
	for section, body := range map[string]string{"auth:oidc": oidc, "payload-logging": payload} {
		if code := put(section, body, false); code != http.StatusForbidden {
			t.Errorf("non-admin PUT %s = %d, want 403", section, code)
		}
		if code := put(section, body, true); code != http.StatusOK {
			t.Errorf("admin PUT %s = %d, want 200", section, code)
		}
	}
	if code := put("parsing", `{"richParsing":true}`, false); code != http.StatusOK {
		t.Errorf("non-admin PUT parsing = %d, want 200", code)
	}
}
