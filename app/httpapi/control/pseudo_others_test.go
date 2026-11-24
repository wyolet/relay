package control

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// Whatever row changes another account's reach, the check after the plan check sees it and refuses: an edit of a policy two accounts share, the delete of a pool another account's policy links, a pool that grows under another account, another account's binding deleted. The same rows with only the target bound pass.
func TestOthersUnchanged(t *testing.T) {
	w := &plantest.World{}
	prov := w.Provider("acme")
	h := w.Host("h", "https://h.example.com")
	m1, m2 := w.Model(prov, "m1"), w.Model(prov, "m2")
	w.Bind(m1, h, adapters.OpenAI)
	w.Bind(m2, h, adapters.OpenAI)
	acct, acct2 := w.Account(h), w.Account(h)
	cred := w.Credential(h, acct, plantest.System, "sk-x")
	cred2 := w.Credential(h, acct2, plantest.System, "sk-y")
	team := w.Team()
	a, b := w.ProjectAccount(w.Project(team)), w.ProjectAccount(w.Project(team))
	pl := w.Pool(h, cred)
	shared := w.Policy("shared", policy.Spec{PoolIDs: []string{pl.Meta.ID}})
	w.BindPolicy(shared, a)
	other := w.BindPolicy(shared, b)
	alone := w.Policy("alone", policy.Spec{PoolIDs: []string{pl.Meta.ID}})
	plans := w.Load(t)
	live := plans.Engine.View()

	narrowed := *shared
	narrowed.Spec.Models = []string{"acme/m1"}
	grown := *pl
	grown.Spec.CredentialIDs = append(grown.Spec.CredentialIDs, cred2.Meta.ID)
	soloEdit := *alone
	soloEdit.Spec.Models = []string{"acme/m1"}
	for _, tc := range []struct {
		name     string
		proposed []any
		refused  bool
	}{
		{"a shared policy edited", []any{&narrowed}, true},
		{"a pool another account's policy links deleted", []any{plan.Deleted{Row: pl}}, true},
		{"a pool another account draws on grown", []any{&grown}, true},
		{"another account's binding deleted", []any{plan.Deleted{Row: other}}, true},
		{"a policy no other account is bound to edited", []any{&soloEdit}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := plan.NewCheck(false, time.Minute).Find(t.Context(), live, nil, tc.proposed)
			if err != nil {
				t.Fatal(err)
			}
			err = othersUnchanged(a.Meta.ID)(f)
			if got := err != nil; got != tc.refused {
				t.Fatalf("refused = %v (%v), want %v", got, err, tc.refused)
			}
			if r, ok := err.(*pseudoRefusal); tc.refused && (!ok || r.Err.Code != codeOthersChanged || r.HTTPStatus != 422) {
				t.Errorf("refusal = %+v, want 422 %s", err, codeOthersChanged)
			}
		})
	}
	unchanged, err := plan.NewCheck(false, time.Minute).Find(t.Context(), live, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := othersUnchanged(a.Meta.ID)(unchanged); err != nil {
		t.Errorf("unchanged plans refused: %v", err)
	}
}
