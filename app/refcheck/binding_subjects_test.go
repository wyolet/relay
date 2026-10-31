package refcheck

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// A subject is bound to one policy: a second binding naming it is refused on save with a conflict that names the binding already holding it. Editing the holding binding, and a group named twice, pass.
func TestPolicyBindingRefusesASubjectAnotherBindingHolds(t *testing.T) {
	group := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}
	user := rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: meta.NewID()}
	held := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "holds-alice"},
		Spec: policybinding.Spec{ProjectID: meta.NewID(), PolicyID: meta.NewID(), Subjects: []rolebinding.Subject{user, group}},
	}
	c := Checker{Rows: Lookup{PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) {
		return []*policybinding.PolicyBinding{held}, nil
	}}}
	ctx := context.Background()
	binding := func(id string, subjects ...rolebinding.Subject) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: id, Name: "next"},
			Spec: policybinding.Spec{ProjectID: held.Spec.ProjectID, PolicyID: meta.NewID(), Subjects: subjects},
		}
	}

	err := c.PolicyBinding(ctx, binding(meta.NewID(), user))
	var re *Error
	if !errors.As(err, &re) || re.Status != http.StatusConflict || re.Code != CodeDuplicateBinding || !strings.Contains(re.Msg, "holds-alice") {
		t.Fatalf("second binding of the user: err = %v, want a 409 duplicate_binding naming holds-alice", err)
	}
	if err := c.PolicyBinding(ctx, binding(held.Meta.ID, user)); err != nil {
		t.Fatalf("editing the binding that holds the user: %v", err)
	}
	if err := c.PolicyBinding(ctx, binding(meta.NewID(), group)); err != nil {
		t.Fatalf("a group named by two bindings: %v", err)
	}
}
