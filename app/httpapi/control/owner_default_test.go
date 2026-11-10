package control

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/meta"
)

// Every kind a person creates in a context lands in the caller's personal scope when the body names no owner (the system for the admin token): the UI has no owner picker, so a kind that defaulted elsewhere would put a row out of its creator's reach.
func TestOmittedOwnerIsTheCallers(t *testing.T) {
	cfg := huma.DefaultConfig("owner-default", "0")
	cfg.OpenAPI.Components.Schemas = httpapi.NewRegistry()
	rows := registerCRUD(humachi.New(chi.NewRouter(), cfg), mountDeps(t), nil)
	for _, plural := range []string{"host-keys", "pools", "accounts", "budgets", "rate-limits", "policies", "policy-bindings"} {
		if got := rows[plural].defaultOwner; got != meta.OwnerUser {
			t.Errorf("%s: an omitted owner defaults to %q, want the caller (%q)", plural, got, meta.OwnerUser)
		}
	}
}
