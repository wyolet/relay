package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
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

// A scoped caller cannot tell an unknown name, or a row it may not see, from any other refused row; an admin still gets the name back. A refusal with a code keeps it, as the control API's own write would.
func TestPlanFailure(t *testing.T) {
	scoped := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-low", Username: "low"})
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-root", Username: "root", Roles: []string{user.RoleAdmin}})
	nf := unresolvedUserErr(t)
	hiddenRow := &apply.AuthzError{Entry: apply.Entry{Kind: "ServiceAccount", Name: "x"}, Err: authz.ErrForbidden}
	missingRef := &refcheck.Error{Status: http.StatusNotFound, Msg: "project p-1 not found"}
	inUse := &refcheck.Error{Status: http.StatusConflict, Code: refcheck.CodeInUse, Msg: "in use"}
	denied := fmt.Errorf("team x: %w", authz.ErrForbidden)

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
		{"hidden row, admin", admin, testRBAC(), hiddenRow, http.StatusForbidden, "forbidden"},
		{"missing reference, admin", admin, testRBAC(), missingRef, http.StatusForbidden, "forbidden"},
		{"unknown name, admin", admin, testRBAC(), nf, http.StatusBadRequest, nf.Error()},
		{"unknown name, single-user", scoped, authz.AlwaysAllowAuthenticated{}, nf, http.StatusBadRequest, nf.Error()},
		{"refusal with a code", scoped, testRBAC(), fmt.Errorf("pool a: %w", inUse), http.StatusConflict, "pool a: in use"},
		{"denied write", scoped, testRBAC(), denied, http.StatusForbidden, denied.Error()},
		{"anything else", scoped, testRBAC(), errors.New("spec.amount: must be positive"), http.StatusBadRequest, "spec.amount: must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := planFailure(tc.ctx, tc.a, tc.err)
			if status, msg := statusOf(t, err), err.Error(); status != tc.status || msg != tc.message {
				t.Fatalf("planFailure = %d %q, want %d %q", status, msg, tc.status, tc.message)
			}
		})
	}
	if !strings.Contains(nf.Error(), "bob@example.test") {
		t.Fatalf("fixture: %v does not name the unresolved user", nf)
	}
	var ae *httpapi.APIError
	if err := planFailure(scoped, testRBAC(), inUse); !errors.As(err, &ae) || ae.Err.Code != "resource_in_use" || ae.Err.Type != "invalid_request_error" {
		t.Fatalf("refusal with a code = %#v, want the envelope with resource_in_use", err)
	}
}

// A write that fails during Execute answers as the control API's own write would; a refused oauth value is the caller's 400 and keeps its reason.
func TestStoreFailure(t *testing.T) {
	refused := &hostkey.ValueRefusedError{Err: errors.New("the token endpoint rejected the credential with HTTP 400: invalid_grant")}
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"managed row", &directory.ManagedError{Kind: "group", Name: "g", Source: "scim"}, http.StatusConflict},
		{"stale version", directory.ErrStaleResourceVersion, http.StatusConflict},
		{"denied write", fmt.Errorf("team x: %w", authz.ErrForbidden), http.StatusForbidden},
		{"refused oauth value", refused, http.StatusBadRequest},
		{"anything else", errors.New("pool closed"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, msg := storeFailure(&apply.StoreError{Entry: apply.Entry{Kind: "HostKey", Name: "k"}, Err: tc.err})
			if status != tc.status {
				t.Fatalf("status = %d, want %d (%s)", status, tc.status, msg)
			}
		})
	}
	if _, msg := storeFailure(&apply.StoreError{Entry: apply.Entry{Kind: "HostKey", Name: "k"}, Err: refused}); !strings.Contains(msg, "invalid_grant") {
		t.Fatalf("message = %q, want the refusal's reason", msg)
	}
}
