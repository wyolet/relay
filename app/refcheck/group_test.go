package refcheck

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/meta"
)

// Group subjects match by name, so renaming a group a binding names would silently strip its members of what the binding grants. A binding the caller may not see still blocks, counted but unnamed.
func TestGroupRename(t *testing.T) {
	binding := func(id, name string, s rolebinding.Subject) *rolebinding.RoleBinding {
		return &rolebinding.RoleBinding{Meta: meta.Metadata{ID: id, Name: name}, Spec: rolebinding.Spec{Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: "u-0"}, s}}}
	}
	group := func(name string) rolebinding.Subject {
		return rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: name}
	}
	few := []*rolebinding.RoleBinding{
		binding("rb-1", "eng-admins", group("eng")),
		binding("rb-2", "ops-admins", group("ops")),
		// A user whose id equals a group's name is no match.
		binding("rb-3", "by-user", rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: "sales"}),
	}
	var many []*rolebinding.RoleBinding
	for i := range 13 {
		many = append(many, binding(fmt.Sprint("rb-", i), fmt.Sprint("b", i), group("eng")))
	}
	const tail = "; remove the group from them or point them at the new name first"
	for _, tc := range []struct {
		name     string
		authz    authz.Authorizer
		rows     []*rolebinding.RoleBinding
		old, new string
		want     string
	}{
		{"one binding names it", nil, few, "eng", "engineering", `409 resource_in_use: group "eng" is named by 1 binding (role-binding "eng-admins")` + tail},
		{"the name is kept", nil, few, "eng", "eng", ""},
		{"no binding names it", nil, few, "sales", "revenue", ""},
		{"at most ten are listed", nil, many, "eng", "x", `409 resource_in_use: group "eng" is named by 13 bindings (role-binding "b0", role-binding "b1", role-binding "b2", role-binding "b3", role-binding "b4", role-binding "b5", role-binding "b6", role-binding "b7", role-binding "b8", role-binding "b9", and 3 more)` + tail},
		{"a hidden binding is counted, not named", hiding("rb-0", "rb-12"), many[:3], "eng", "x", `409 resource_in_use: group "eng" is named by 3 bindings (role-binding "b1", role-binding "b2", and 1 more)` + tail},
		{"only hidden bindings", hiding("rb-1"), few, "eng", "x", `409 resource_in_use: group "eng" is named by 1 binding (and 1 more)` + tail},
	} {
		c := Checker{Authz: tc.authz, Rows: Lookup{RoleBindings: func(context.Context) ([]*rolebinding.RoleBinding, error) { return tc.rows, nil }}}
		wantRefused(t, tc.name, c.GroupRename(context.Background(), tc.old, tc.new), tc.want)
	}
	failing := Checker{Rows: Lookup{RoleBindings: func(context.Context) ([]*rolebinding.RoleBinding, error) { return nil, errors.New("store down") }}}
	wantRefused(t, "the binding list failing", failing.GroupRename(context.Background(), "eng", "x"), "500: store down")
	wantRefused(t, "no binding lookup", Checker{}.GroupRename(context.Background(), "eng", "x"), "")
}
