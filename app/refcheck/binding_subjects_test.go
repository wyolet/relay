package refcheck

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
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
	held.StampOwner()
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

// A user is bound once per scope: two projects, two teams, or a team and another team's project may each bind the same user, because no request is inside both. The system and any scope, a team and its own project, or the user's own binding and any other overlap and conflict. Other subjects stay at one binding whatever the owners.
func TestPolicyBindingBindsAUserOncePerScope(t *testing.T) {
	teamA, teamB := meta.NewID(), meta.NewID()
	inA, alsoInA, inB := meta.NewID(), meta.NewID(), meta.NewID()
	projects := map[string]string{inA: teamA, alsoInA: teamA, inB: teamB}
	sys := meta.Owner{Kind: meta.OwnerSystem}
	team := func(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerTeam, ID: id} }
	proj := func(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerProject, ID: id} }
	alice := rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: meta.NewID()}
	sa := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: meta.NewID()}
	own := meta.Owner{Kind: meta.OwnerUser, ID: alice.ID}

	bindingOf := func(owner meta.Owner, sub rolebinding.Subject) *policybinding.PolicyBinding {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-" + string(owner.Kind), Owner: owner},
			Spec: policybinding.Spec{PolicyID: meta.NewID(), Subjects: []rolebinding.Subject{sub}},
		}
		b.StampOwner()
		return b
	}
	for _, tc := range []struct {
		name         string
		held, next   meta.Owner
		sub          rolebinding.Subject
		wantConflict bool
	}{
		{"user in two projects", proj(inA), proj(alsoInA), alice, false},
		{"user in two teams", team(teamA), team(teamB), alice, false},
		{"user in a team and another team's project", team(teamA), proj(inB), alice, false},
		{"user in a team and its project", team(teamA), proj(inA), alice, true},
		{"user in a project and its team", proj(inA), team(teamA), alice, true},
		{"user in the system and a project", sys, proj(inA), alice, true},
		{"user in a project and the system", proj(inA), sys, alice, true},
		{"user twice in one project", proj(inA), proj(inA), alice, true},
		{"user's own binding and a project", own, proj(inA), alice, true},
		{"a project and the user's own binding", proj(inB), own, alice, true},
		{"user's own binding and a team", own, team(teamA), alice, true},
		{"user's own binding and the system", own, sys, alice, true},
		{"service account in two scopes", proj(inA), team(teamA), sa, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := bindingOf(tc.held, tc.sub)
			c := Checker{Rows: Lookup{
				PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) {
					return []*policybinding.PolicyBinding{held}, nil
				},
				Project: func(_ context.Context, id string) *project.Project {
					if tm, ok := projects[id]; ok {
						return &project.Project{Meta: meta.Metadata{ID: id}, Spec: project.Spec{TeamID: tm}}
					}
					return nil
				},
			}}
			err := c.subjectsUnbound(context.Background(), bindingOf(tc.next, tc.sub))
			var re *Error
			if conflict := errors.As(err, &re) && re.Code == CodeDuplicateBinding; conflict != tc.wantConflict {
				t.Fatalf("err = %v, want conflict %v", err, tc.wantConflict)
			}
		})
	}
}
