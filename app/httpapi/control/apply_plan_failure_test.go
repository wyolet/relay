package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

// unresolvedUserErr is the plan error for a Group naming an unknown user.
func unresolvedUserErr(t *testing.T) error {
	t.Helper()
	d := &manifest.GroupDTO{APIVersion: manifest.APIVersion, Kind: "Group"}
	d.Metadata.Name = "probe"
	d.Spec.Members = []string{"bob@example.test"}
	_, err := manifest.ToGroup(*d, manifest.MapResolver{Users: map[string]string{"alice@example.test": meta.NewID()}})
	var nf *manifest.RefNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("ToGroup = %v, want a RefNotFoundError", err)
	}
	return err
}

// A scoped caller cannot tell an unknown name, or a row it may not see, from
// any other refused row; an admin still gets the name back.
func TestApplyPlanFailureHidesUnresolvedNamesFromScopedCallers(t *testing.T) {
	scoped := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-low", Username: "low"})
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-root", Username: "root", Roles: []string{user.RoleAdmin}})
	nf := unresolvedUserErr(t)
	hiddenRow := &apply.AuthzError{Entry: apply.Entry{Kind: "ServiceAccount", Name: "x"}, Err: authz.ErrForbidden}

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		a       authz.Authorizer
		err     error
		status  int
		message string
	}{
		{"unknown name, scoped caller", scoped, testRBAC(), nf, http.StatusForbidden, "forbidden"},
		{"hidden row, scoped caller", scoped, testRBAC(), hiddenRow, http.StatusForbidden, "forbidden"},
		{"unknown name, admin", admin, testRBAC(), nf, http.StatusBadRequest, "bob@example.test"},
		{"unknown name, single-user", scoped, authz.AlwaysAllowAuthenticated{}, nf, http.StatusBadRequest, "bob@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := planFailure(tc.ctx, tc.a, tc.err)
			status, msg := statusOf(t, err), err.Error()
			if status != tc.status || !strings.Contains(msg, tc.message) {
				t.Fatalf("planFailure = %d %q, want %d containing %q", status, msg, tc.status, tc.message)
			}
			if tc.message == "forbidden" && msg != "forbidden" {
				t.Fatalf("refusal names more than it should: %q", msg)
			}
		})
	}
}
