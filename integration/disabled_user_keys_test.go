//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"
)

// A personal key acts as its user: disabling the user stops the key, and
// re-enabling brings it back.
func TestIntegration_DisabledUsersPersonalKeysStopWorking(t *testing.T) {
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
	if got, reason := f.waitForRejection(keyPlain, 2*time.Second); got != http.StatusUnauthorized {
		t.Fatalf("chat with a disabled user's key = %d (%s), want 401", got, reason)
	}
	if code, raw := f.adminDo(http.MethodPut, "/api/users/by-id/"+owner.ID, `{"disabled":false}`); code != http.StatusOK {
		t.Fatalf("re-enable = %d: %s", code, raw)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, reason := f.chat(keyPlain, "test-model")
		if got == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat after re-enable = %d (%s), want 200", got, reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
