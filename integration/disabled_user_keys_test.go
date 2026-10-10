//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// A personal key acts as its user: disabling the user stops the key, and
// re-enabling brings it back.
func TestIntegration_DisabledUsersPersonalKeysStopWorking(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	const keyPlain = "rk_test_secret_value_e2e" // seedHappyPath's personal key
	owner, err := f.users.ByUsername(t.Context(), "e2e-owner")
	if err != nil || owner == nil {
		t.Fatalf("key owner: %v", err)
	}
	if got, reason := f.chat(keyPlain, "test-model"); got != http.StatusOK {
		t.Fatalf("chat before disable = %d (%s)", got, reason)
	}
	if code, raw := f.adminDo(http.MethodPut, "/api/users/by-id/"+owner.ID, `{"disabled":true}`); code != http.StatusOK {
		t.Fatalf("disable = %d: %s", code, raw)
	}
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return !s.UserEnabled(owner.ID) },
		"the user's disable never reached the snapshot")
	if got, reason := f.chat(keyPlain, "test-model"); got != http.StatusUnauthorized {
		t.Fatalf("chat with a disabled user's key = %d (%s), want 401", got, reason)
	}
	if code, raw := f.adminDo(http.MethodPut, "/api/users/by-id/"+owner.ID, `{"disabled":false}`); code != http.StatusOK {
		t.Fatalf("re-enable = %d: %s", code, raw)
	}
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return s.UserEnabled(owner.ID) },
		"the user's re-enable never reached the snapshot")
	if got, reason := f.chat(keyPlain, "test-model"); got != http.StatusOK {
		t.Fatalf("chat after re-enable = %d (%s), want 200", got, reason)
	}
}
