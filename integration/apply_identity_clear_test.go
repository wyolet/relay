//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

// A bundle that would leave an account with no identity and no password is refused before any of it is written.
func TestIntegration_ApplyRefusesAnIdentityClearWithZeroWrites(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	carol := &user.User{ID: ids.New(), Username: "carol", Email: "carol@example.com", OIDCSubject: "https://idp.example.com|c"}
	if err := st.users.Upsert(ctx, carol); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	bundle := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: core, owner: {kind: system}}\nspec: {}\n" +
		"---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: carol}\nspec: {email: carol@corp.example.com, identity: null}\n"
	if code, _, raw := st.applyBundle(bundle, ""); code != http.StatusBadRequest {
		t.Fatalf("apply = %d, want 400: %s", code, raw)
	}
	if teams, err := st.stores.Team.List(ctx); err != nil || len(teams) != 0 {
		t.Fatalf("the refused bundle wrote %d teams (%v)", len(teams), err)
	}
	got, err := st.users.Get(ctx, carol.ID)
	if err != nil || got.Email != carol.Email || got.OIDCSubject != carol.OIDCSubject {
		t.Fatalf("the refused bundle changed the account: %+v %v", got, err)
	}
}
