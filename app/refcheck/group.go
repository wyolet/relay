package refcheck

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// CodeInUse is the error code of a write refused because other rows still
// name the row, as a refused delete of a referenced row answers.
const CodeInUse = "resource_in_use"

// maxNamedBindings caps how many bindings a refusal lists by name.
const maxNamedBindings = 10

// GroupRename refuses changing a group's name while a role or policy binding
// names the old one. Group subjects match by name, so the rename would
// silently detach every such binding from the group's members.
func (c Checker) GroupRename(ctx context.Context, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	want := (&rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: oldName}).Key()
	var named []string
	total := 0
	add := func(kind, id, name string, owner meta.Owner, subjects []rolebinding.Subject) {
		if !namesSubject(subjects, want) {
			return
		}
		total++
		// A binding the caller may not see still blocks, but is only counted.
		if len(named) < maxNamedBindings && Visible(ctx, c.Authz, kind, id, owner) {
			named = append(named, fmt.Sprintf("%s %q", kind, name))
		}
	}
	if c.Rows.RoleBindings != nil {
		rbs, err := c.Rows.RoleBindings(ctx)
		if err != nil {
			return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
		}
		for _, rb := range rbs {
			add("role-binding", rb.Meta.ID, rb.Meta.Name, rb.Meta.Owner, rb.Spec.Subjects)
		}
	}
	if c.Rows.PolicyBindings != nil {
		pbs, err := c.Rows.PolicyBindings(ctx)
		if err != nil {
			return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
		}
		for _, pb := range pbs {
			add("policy-binding", pb.Meta.ID, pb.Meta.Name, pb.Meta.Owner, pb.Spec.Subjects)
		}
	}
	if total == 0 {
		return nil
	}
	list := strings.Join(named, ", ")
	if rest := total - len(named); rest > 0 {
		if list != "" {
			list += ", "
		}
		list += fmt.Sprintf("and %d more", rest)
	}
	noun := "bindings"
	if total == 1 {
		noun = "binding"
	}
	return &Error{
		Status: http.StatusConflict,
		Code:   CodeInUse,
		Msg: fmt.Sprintf("group %q is named by %d %s (%s); remove the group from them or point them at the new name first",
			oldName, total, noun, list),
	}
}

func namesSubject(subjects []rolebinding.Subject, key string) bool {
	for i := range subjects {
		if subjects[i].Key() == key {
			return true
		}
	}
	return false
}
