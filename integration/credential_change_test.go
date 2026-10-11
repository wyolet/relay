//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// A password or identity change through the control API's apply ends the account's live sessions and kills the tokens it minted: both were issued under the credentials that just changed.
func TestIntegration_CredentialChangeEndsSessionsAndTokens(t *testing.T) {
	t.Parallel()
	for name, spec := range map[string]string{
		"password": "{password: {hash: '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy'}}",
		"identity": "{identity: {issuer: 'https://idp.example.com', subject: minter}}",
	} {
		t.Run(name, func(t *testing.T) {
			f := newTokenFixture(t)
			us := f.login(t, f.username, f.password)
			code, token, raw := f.mint(us)
			if code != http.StatusOK {
				t.Fatalf("mint = %d: %s", code, raw)
			}
			minted, _ := f.cat.Current().TokenVersion(f.userID)

			bundle := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: " + f.username + "}\nspec: " + spec + "\n"
			if code, _, raw := f.applyBundle(bundle, ""); code != http.StatusOK {
				t.Fatalf("apply = %d: %s", code, raw)
			}
			if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusUnauthorized {
				t.Fatalf("whoami after the %s change = %d, want 401: %s", name, code, raw)
			}
			f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool {
				v, ok := s.TokenVersion(f.userID)
				return ok && v != minted
			}, "the token version bump never reached the snapshot")
			if got, reason := f.chat(token, "test-model"); got != http.StatusUnauthorized || reason != "token_revoked" {
				t.Fatalf("chat after the %s change = %d (%s), want 401 token_revoked", name, got, reason)
			}
		})
	}
}
