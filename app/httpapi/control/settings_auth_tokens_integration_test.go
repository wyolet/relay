//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

// The TTLs are writable through the API, but the signing-key refs stay
// where rotation put them: a PUT that leaves them out keeps them and one
// naming another secret is refused.
func TestAuthTokensSettingsKeepSigningKeys(t *testing.T) {
	ctx := t.Context()
	pool := storagetest.Pool(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	current := secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key-b"}
	previous := secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key-a"}
	seeded, _ := json.Marshal(settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour,
		SigningKey: current, PreviousSigningKey: previous})
	if _, err := stores.Settings.Upsert(ctx, settings.AuthTokensSection, seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deps := mountDeps(t)
	deps.Stores = stores
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", map[string]*actor.Actor{"bob": {UserID: "u-bob", Username: "bob", SessionID: "s"}}))
	Mount(r, deps)

	put := func(body string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/settings/"+settings.AuthTokensSection, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if admin {
			req.Header.Set("Authorization", "Bearer test-admin-token")
		} else {
			req.Header.Set("X-Test-User", "bob")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	stored := func() *settings.AuthTokens {
		row, err := stores.Settings.Get(ctx, settings.AuthTokensSection)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		return row.Value.(*settings.AuthTokens)
	}

	ttls := `{"enabled":true,"defaultTTL":7200000000000,"maxTTL":43200000000000}`
	if w := put(ttls, false); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT = %d, want 403: %s", w.Code, w.Body)
	}
	if w := put(ttls, true); w.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d, want 200: %s", w.Code, w.Body)
	}
	got := stored()
	if got.DefaultTTL != 2*time.Hour || got.MaxTTL != 12*time.Hour {
		t.Errorf("ttls = %v/%v, want 2h/12h", got.DefaultTTL, got.MaxTTL)
	}
	if got.SigningKey != current || got.PreviousSigningKey != previous {
		t.Fatalf("a PUT without refs changed them: %+v / %+v", got.SigningKey, got.PreviousSigningKey)
	}

	echoed, _ := json.Marshal(got)
	if w := put(string(echoed), true); w.Code != http.StatusOK {
		t.Fatalf("PUT of the section as read = %d, want 200: %s", w.Code, w.Body)
	}
	repointed := `{"enabled":true,"defaultTTL":3600000000000,"maxTTL":86400000000000,"signingKey":{"kind":"stored","id":"host-key-secret"}}`
	if w := put(repointed, true); w.Code != http.StatusBadRequest {
		t.Fatalf("PUT naming another signing key = %d, want 400: %s", w.Code, w.Body)
	}
	if got := stored(); got.SigningKey != current || got.DefaultTTL != 2*time.Hour {
		t.Fatalf("refused PUT was written: %+v", got)
	}
}
