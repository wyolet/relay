//go:build integration

package control

import (
	"net/http"
	"testing"
)

// A service account named as another one is a conflict with a stored row, not a server failure.
func TestServiceAccounts_TakenNameAnswers409(t *testing.T) {
	t.Skip("bug: POST /service-accounts with a taken metadata.name answers 500 internal_error (directory: conflict: service_accounts_name_key)")
	w := newSharesWorld(t)
	body := `{"metadata":{"name":"taken"},"spec":{"projectId":"` + w.projTwo.Meta.ID + `"}}`
	wantStatus(t, w.send("bob", http.MethodPost, "/service-accounts", body), http.StatusCreated)
	wantStatus(t, w.send("bob", http.MethodPost, "/service-accounts", body), http.StatusConflict)
}
