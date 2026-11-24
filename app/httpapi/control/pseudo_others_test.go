package control

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// The policy step never edits a policy or pool another account uses; if rows ever did, the check after the plan check refuses them: an edit of a policy two accounts share, or the delete of a pool another account's policy links, changes the other account's reach. The same rows with only the target bound pass.
func TestOthersUnchanged(t *testing.T) {
	w := &plantest.World{}
	prov := w.Provider("acme")
	h := w.Host("h", "https://h.example.com")
	m1, m2 := w.Model(prov, "m1"), w.Model(prov, "m2")
	w.Bind(m1, h, adapters.OpenAI)
	w.Bind(m2, h, adapters.OpenAI)
	acct := w.Account(h)
	cred := w.Credential(h, acct, plantest.System, "sk-x")
	team := w.Team()
	a, b := w.ProjectAccount(w.Project(team)), w.ProjectAccount(w.Project(team))
	pl := w.Pool(h, cred)
	shared := w.Policy("shared", policy.Spec{PoolIDs: []string{pl.Meta.ID}})
	w.BindPolicy(shared, a, b)
	alone := w.Policy("alone", policy.Spec{PoolIDs: []string{pl.Meta.ID}})
	plans := w.Load(t)
	live := plans.Engine.View()

	narrowed := *shared
	narrowed.Spec.Models = []string{"acme/m1"}
	edit := []pseudo.Row{{Kind: "policy", Meta: &narrowed.Meta, Value: &narrowed, Op: pseudo.OpUpdate}}
	gone := []pseudo.Row{{Kind: "pool", Meta: &pl.Meta, Value: pl, Op: pseudo.OpDelete}}
	soloEdit := *alone
	soloEdit.Spec.Models = []string{"acme/m1"}
	for _, tc := range []struct {
		name    string
		rows    []pseudo.Row
		refused bool
	}{
		{"a shared policy edited", edit, true},
		{"a pool another account's policy links deleted", gone, true},
		{"a policy no other account is bound to edited", []pseudo.Row{{Kind: "policy", Meta: &soloEdit.Meta, Value: &soloEdit, Op: pseudo.OpUpdate}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposed := make([]any, len(tc.rows))
			for i, r := range tc.rows {
				proposed[i] = r.Value
				if r.Op == pseudo.OpDelete {
					proposed[i] = plan.Deleted{Row: r.Value}
				}
			}
			_, after, err := plan.NewCheck(false, time.Minute).NewIssuesAndView(t.Context(), live, nil, proposed)
			if err != nil {
				t.Fatal(err)
			}
			err = othersUnchanged(live, tc.rows, a.Meta.ID)(after)
			if got := err != nil; got != tc.refused {
				t.Fatalf("refused = %v (%v), want %v", got, err, tc.refused)
			}
			if r, ok := err.(*pseudoRefusal); tc.refused && (!ok || r.Err.Code != codeOthersChanged || r.HTTPStatus != 422) {
				t.Errorf("refusal = %+v, want 422 %s", err, codeOthersChanged)
			}
		})
	}
	if err := othersUnchanged(live, edit, a.Meta.ID)(live); err != nil {
		t.Errorf("unchanged plans refused: %v", err)
	}
}
