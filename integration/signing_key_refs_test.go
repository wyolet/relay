//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/ids"
)

// The token signing-key refs are written only by rotation: no settings write
// may point one at a secret the caller chose, or they could sign tokens.
func TestIntegration_SettingsWriteCannotRepointSigningKeys(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	foreign := ids.New()
	body := `{"enabled":true,"defaultTTL":3600000000000,"maxTTL":86400000000000,` +
		`"previousSigningKey":{"kind":"stored","id":"` + foreign + `"}}`
	for _, req := range []struct{ method, path string }{
		{http.MethodPut, "/api/settings/" + settings.AuthTokensSection},
		{http.MethodPatch, "/api/settings/" + settings.AuthTokensSection},
		{http.MethodPut, "/api/settings"},
		{http.MethodPost, "/api/settings"},
	} {
		if code, raw := st.adminDo(req.method, req.path, body); code < 400 {
			t.Fatalf("%s %s = %d, want refused: %s", req.method, req.path, code, raw)
		}
	}
	row, err := st.stores.Settings.Get(context.Background(), settings.AuthTokensSection)
	if err != nil {
		t.Fatal(err)
	}
	if cur := row.Value.(*settings.AuthTokens); cur.PreviousSigningKey.ID == foreign {
		t.Fatalf("signing key ref was repointed: %+v", cur)
	}
}
