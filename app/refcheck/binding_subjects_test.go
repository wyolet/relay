package refcheck

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
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

// A binding naming a host key is checked, and saved, as one naming the key's account: an account another binding holds refuses it with that binding named, whichever of its keys it came through.
func TestPolicyBindingOnAKeyBindsItsAccount(t *testing.T) {
	acct := meta.NewID()
	k1, k2 := meta.NewID(), meta.NewID()
	held := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "holds-the-account", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policybinding.Spec{PolicyID: meta.NewID(), Subjects: []rolebinding.Subject{{Kind: policybinding.SubjectAccount, ID: acct}}},
	}
	keys := map[string]*hostkey.HostKey{
		k1: {Meta: meta.Metadata{ID: k1, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{AccountID: acct}},
		k2: {Meta: meta.Metadata{ID: k2, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{AccountID: acct}},
	}
	c := Checker{Rows: Lookup{
		HostKey: func(_ context.Context, id string) *hostkey.HostKey { return keys[id] },
		PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) {
			return []*policybinding.PolicyBinding{held}, nil
		},
	}}
	onKey := func(id, key string) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: id, Name: "on-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: policybinding.Spec{PolicyID: meta.NewID(), Subjects: []rolebinding.Subject{{Kind: policybinding.SubjectCredential, ID: key}}},
		}
	}
	for _, k := range []string{k1, k2} {
		err := c.PolicyBinding(context.Background(), onKey(meta.NewID(), k))
		var re *Error
		if !errors.As(err, &re) || re.Status != http.StatusConflict || !strings.Contains(re.Msg, "holds-the-account") {
			t.Fatalf("binding on a key of a held account: err = %v, want a 409 naming holds-the-account", err)
		}
	}
	edit := onKey(held.Meta.ID, k1)
	if err := c.PolicyBinding(context.Background(), edit); err != nil {
		t.Fatalf("editing the holding binding to name a key of its account: %v", err)
	}
	if s := edit.Spec.Subjects; len(s) != 1 || s[0].Kind != policybinding.SubjectAccount || s[0].ID != acct {
		t.Fatalf("subjects after the check = %+v, want the account %s", s, acct)
	}
}
