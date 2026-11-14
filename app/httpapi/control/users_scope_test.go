package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// captureAuthz records the resource the handler authorizes against.
type captureAuthz struct{ got authz.Resource }

func (c *captureAuthz) Authorize(_ context.Context, _ string, res authz.Resource) error {
	c.got = res
	return authz.ErrForbidden
}

func TestUsersListAuthorizesAtTheGlobalScope(t *testing.T) {
	c := &captureAuthz{}
	d := Deps{Authz: c, Users: &user.Store{}}
	h := usersHandlerWith(t, d)
	w := scopeReq(t, h, "alice", http.MethodGet, "/users", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if c.got.Owner == nil || c.got.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("authorized against owner %+v, want {system}", c.got.Owner)
	}
}

func usersHandlerWith(t *testing.T, d Deps) http.Handler {
	t.Helper()
	r, api := testAPI(t, scopeActors)
	registerUsers(api, d, nil)
	return r
}
