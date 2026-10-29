package refcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

func groupSubject(name string) []rolebinding.Subject {
	return []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: name}}
}

func bindingRows(rbs []*rolebinding.RoleBinding, pbs []*policybinding.PolicyBinding) Lookup {
	return Lookup{
		RoleBindings:   func(context.Context) ([]*rolebinding.RoleBinding, error) { return rbs, nil },
		PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) { return pbs, nil },
	}
}

func refusal(t *testing.T, err error) *Error {
	t.Helper()
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a refcheck refusal", err)
	}
	if re.Status != http.StatusConflict || re.Code != CodeInUse {
		t.Fatalf("status/code = %d/%q, want 409/%q", re.Status, re.Code, CodeInUse)
	}
	return re
}

// Group subjects match by name, so renaming a group a binding names would
// silently strip its members of what the binding grants.
func TestGroupRenameRefusedWhileARoleBindingNamesIt(t *testing.T) {
	rbs := []*rolebinding.RoleBinding{
		{Meta: meta.Metadata{ID: "rb-1", Name: "eng-admins"}, Spec: rolebinding.Spec{Subjects: groupSubject("eng")}},
		{Meta: meta.Metadata{ID: "rb-2", Name: "ops-admins"}, Spec: rolebinding.Spec{Subjects: groupSubject("ops")}},
	}
	err := Checker{Rows: bindingRows(rbs, nil)}.GroupRename(context.Background(), "eng", "engineering")
	want := `group "eng" is named by 1 binding (role-binding "eng-admins"); remove the group from them or point them at the new name first`
	if got := refusal(t, err).Msg; got != want {
		t.Fatalf("msg = %q\nwant  %q", got, want)
	}
}

func TestGroupRenameRefusedWhileAPolicyBindingNamesIt(t *testing.T) {
	pbs := []*policybinding.PolicyBinding{
		{Meta: meta.Metadata{ID: "pb-1", Name: "eng-default"}, Spec: policybinding.Spec{Subjects: []rolebinding.Subject{
			{Kind: rolebinding.SubjectUser, ID: "01a12c00-0000-7000-8000-000000000001"},
			{Kind: rolebinding.SubjectGroup, Name: "eng"},
		}}},
	}
	err := Checker{Rows: bindingRows(nil, pbs)}.GroupRename(context.Background(), "eng", "engineering")
	want := `group "eng" is named by 1 binding (policy-binding "eng-default"); remove the group from them or point them at the new name first`
	if got := refusal(t, err).Msg; got != want {
		t.Fatalf("msg = %q\nwant  %q", got, want)
	}
}

func TestGroupRenameListsAtMostTenBindings(t *testing.T) {
	var rbs []*rolebinding.RoleBinding
	for i := range 12 {
		rbs = append(rbs, &rolebinding.RoleBinding{Meta: meta.Metadata{ID: fmt.Sprint("rb-", i), Name: fmt.Sprint("b", i)}, Spec: rolebinding.Spec{Subjects: groupSubject("eng")}})
	}
	pbs := []*policybinding.PolicyBinding{{Meta: meta.Metadata{ID: "pb-1", Name: "p"}, Spec: policybinding.Spec{Subjects: groupSubject("eng")}}}
	err := Checker{Rows: bindingRows(rbs, pbs)}.GroupRename(context.Background(), "eng", "x")
	want := `group "eng" is named by 13 bindings (role-binding "b0", role-binding "b1", role-binding "b2", role-binding "b3", role-binding "b4", role-binding "b5", role-binding "b6", role-binding "b7", role-binding "b8", role-binding "b9", and 3 more); remove the group from them or point them at the new name first`
	if got := refusal(t, err).Msg; got != want {
		t.Fatalf("msg = %q\nwant  %q", got, want)
	}
}

// Keeping the name (a display-name edit) and renaming a group nothing names
// both pass; a user subject whose id happens to equal the name is no match.
func TestGroupRenameAllowedWhenNoBindingNamesTheGroup(t *testing.T) {
	rbs := []*rolebinding.RoleBinding{
		{Meta: meta.Metadata{ID: "rb-1", Name: "eng-admins"}, Spec: rolebinding.Spec{Subjects: groupSubject("eng")}},
		{Meta: meta.Metadata{ID: "rb-2", Name: "by-user"}, Spec: rolebinding.Spec{Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: "ops"}}}},
	}
	c := Checker{Rows: bindingRows(rbs, nil)}
	if err := c.GroupRename(context.Background(), "eng", "eng"); err != nil {
		t.Fatalf("unchanged name: %v", err)
	}
	if err := c.GroupRename(context.Background(), "ops", "operations"); err != nil {
		t.Fatalf("unreferenced rename: %v", err)
	}
}
