package refcheck

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

// scopeChecker is the link rows with a service account per project, a policy per owner, and one member of p-1 ("u-member"); t-1 holds p-1, t-2 holds p-2.
func scopeChecker() Checker {
	rows := newLinkRows()
	c := rows.checker(nil)
	sas := map[string]*serviceaccount.ServiceAccount{
		"sa-p-1": {Meta: meta.Metadata{ID: "sa-p-1", Name: "sa-p-1"}, Spec: serviceaccount.Spec{ProjectID: "p-1"}},
		"sa-p-2": {Meta: meta.Metadata{ID: "sa-p-2", Name: "sa-p-2"}, Spec: serviceaccount.Spec{ProjectID: "p-2"}},
	}
	pols := map[string]*policy.Policy{}
	for id, o := range map[string]meta.Owner{
		"pol-system": {Kind: meta.OwnerSystem},
		"pol-host":   {Kind: meta.OwnerHost, ID: "h-1"},
		"pol-t-1":    {Kind: meta.OwnerTeam, ID: "t-1"},
		"pol-t-2":    {Kind: meta.OwnerTeam, ID: "t-2"},
		"pol-p-1":    {Kind: meta.OwnerProject, ID: "p-1"},
		"pol-p-2":    {Kind: meta.OwnerProject, ID: "p-2"},
		"pol-user":   {Kind: meta.OwnerUser, ID: "u-member"},
	} {
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
	}
	c.Rows.ServiceAccount = func(_ context.Context, id string) *serviceaccount.ServiceAccount { return sas[id] }
	c.Rows.Policy = func(_ context.Context, id string) *policy.Policy { return pols[id] }
	c.Rows.UserActsIn = func(_ context.Context, userID string, scope meta.Owner) (bool, error) {
		return userID == "u-member" && (scope == meta.Owner{Kind: meta.OwnerProject, ID: "p-1"} || scope == meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}), nil
	}
	return c
}

func scopedBinding(owner meta.Owner, policyID string, subjects ...rolebinding.Subject) *policybinding.PolicyBinding {
	b := &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: owner}, Spec: policybinding.Spec{PolicyID: policyID, Subjects: subjects}}
	if owner.Kind == meta.OwnerProject {
		b.Spec.ProjectID = owner.ID
	}
	return b
}

// A binding binds only subjects inside its owner's scope: a project its own service accounts, accounts, credentials and members; a team the same for itself and its projects; only the system binds a host.
func TestPolicyBindingSubjectsStayInsideTheOwner(t *testing.T) {
	c := scopeChecker()
	ctx := context.Background()
	sys := meta.Owner{Kind: meta.OwnerSystem}
	t1 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	p1 := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	sub := func(kind rolebinding.SubjectKind, id string) rolebinding.Subject {
		return rolebinding.Subject{Kind: kind, ID: id}
	}
	for _, tc := range []struct {
		name  string
		owner meta.Owner
		sub   rolebinding.Subject
		want  int // 0 = accepted
	}{
		{"project binds a host", p1, sub(policybinding.SubjectHost, "h-1"), http.StatusBadRequest},
		{"team binds a host", t1, sub(policybinding.SubjectHost, "h-1"), http.StatusBadRequest},
		{"system binds a host", sys, sub(policybinding.SubjectHost, "h-1"), 0},
		{"project binds its service account", p1, sub(rolebinding.SubjectServiceAccount, "sa-p-1"), 0},
		{"project binds another project's service account", p1, sub(rolebinding.SubjectServiceAccount, "sa-p-2"), http.StatusBadRequest},
		{"project binds its account", p1, sub(policybinding.SubjectAccount, "acct-p-1"), 0},
		{"project binds its team's account", p1, sub(policybinding.SubjectAccount, "acct-team-1"), http.StatusBadRequest},
		{"project binds its credential", p1, sub(policybinding.SubjectCredential, "key-project"), 0},
		{"project binds a system credential", p1, sub(policybinding.SubjectCredential, "key-h1"), http.StatusBadRequest},
		{"project binds itself", p1, sub(policybinding.SubjectProject, "p-1"), 0},
		{"project binds its team", p1, sub(policybinding.SubjectTeam, "t-1"), http.StatusBadRequest},
		{"project binds a member", p1, sub(rolebinding.SubjectUser, "u-member"), 0},
		{"project binds a stranger", p1, sub(rolebinding.SubjectUser, "u-stranger"), http.StatusBadRequest},
		{"team binds its project's account", t1, sub(policybinding.SubjectAccount, "acct-p-1"), 0},
		{"team binds its own account", t1, sub(policybinding.SubjectAccount, "acct-team-1"), 0},
		{"team binds another team's project's account", t1, sub(policybinding.SubjectAccount, "acct-p-2"), http.StatusBadRequest},
		{"team binds a shared account", t1, sub(policybinding.SubjectAccount, "acct-shared"), http.StatusBadRequest},
		{"team binds its project's credential", t1, sub(policybinding.SubjectCredential, "key-project"), 0},
		{"team binds its project's service account", t1, sub(rolebinding.SubjectServiceAccount, "sa-p-1"), 0},
		{"team binds its project", t1, sub(policybinding.SubjectProject, "p-1"), 0},
		{"team binds another team's project", t1, sub(policybinding.SubjectProject, "p-2"), http.StatusBadRequest},
		{"team binds a member", t1, sub(rolebinding.SubjectUser, "u-member"), 0},
		{"system binds anyone", sys, sub(rolebinding.SubjectUser, "u-stranger"), 0},
		{"system binds a shared account", sys, sub(policybinding.SubjectAccount, "acct-shared"), 0},
	} {
		if got := status(c.PolicyBinding(ctx, scopedBinding(tc.owner, "pol-system", tc.sub))); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A binding uses a policy of its own scope, an ancestor of it, the system, or a host; a sibling's, a descendant's or a person's is refused.
func TestPolicyBindingPolicyComesFromItsScopeOrAbove(t *testing.T) {
	c := scopeChecker()
	ctx := context.Background()
	project := rolebinding.Subject{Kind: policybinding.SubjectProject, ID: "p-1"}
	team := rolebinding.Subject{Kind: policybinding.SubjectTeam, ID: "t-1"}
	for _, tc := range []struct {
		name   string
		owner  meta.Owner
		sub    rolebinding.Subject
		policy string
		ok     bool
	}{
		{"team binding, its own policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-t-1", true},
		{"team binding, a sibling team's policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-t-2", false},
		{"team binding, its project's policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-p-1", false},
		{"team binding, a system policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-system", true},
		{"team binding, a host tier preset", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-host", true},
		{"project binding, its team's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-t-1", true},
		{"project binding, another team's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-t-2", false},
		{"project binding, another project's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-p-2", false},
		{"project binding, a personal policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-user", false},
		{"system binding, a team's policy", meta.Owner{Kind: meta.OwnerSystem}, project, "pol-t-1", false},
		{"system binding, a host tier preset", meta.Owner{Kind: meta.OwnerSystem}, project, "pol-host", true},
	} {
		err := c.PolicyBinding(ctx, scopedBinding(tc.owner, tc.policy, tc.sub))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && status(err) != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", tc.name, status(err))
		}
	}
}
