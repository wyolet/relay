package pseudo

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// stepTargets are the three accounts a target stands for: a team's own, a project's own and a service account's lane.
func stepTargets() map[string]*account.Account {
	team := account.ForTeam("team", "research", "Research")
	team.Meta.ID = "team-acct"
	project := account.ForProject("proj", "search", "Search")
	project.Meta.ID = "proj-acct"
	lane := account.ForServiceAccount("sa", "ci-bot", "CI bot", "proj")
	lane.Meta.ID = "sa-acct"
	return map[string]*account.Account{"team": team, "project": project, "service-account": lane}
}

func stepReads(acc *account.Account) PolicyReads {
	n := 0
	return PolicyReads{
		Account: acc, TargetLabel: "Research",
		Uncarried: func(*policy.Policy) map[string]int { return nil },
		Names:     &Namer{Taken: func(string, string) bool { return false }},
		NewID:     func() string { n++; return "id-" + strconv.Itoa(n) },
	}
}

func addModel(s *policy.Spec) { s.Models = append(s.Models, "acme/m2") }

func heldBy(policyID string, accountIDs ...string) *policybinding.PolicyBinding {
	return &policybinding.PolicyBinding{Meta: meta.Metadata{ID: "held", Name: "held", Owner: meta.Global, ResourceVersion: "7"}, Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: accountIDs}}
}

// rebinding is the step binding acc alone to policyID, as PUT /accounts/{ref}/policy does.
func rebinding(policyID string, acc *account.Account, pointer string) Row {
	rb := &AccountRebinding{Account: acc, PolicyID: policyID}
	return Row{Kind: "policy-binding", Pointer: pointer, Meta: &rb.Binding.Meta, Value: rb}
}

// none: a policy of the change alone, owned by the account's scope, bound to the account alone; an account not made yet is made first.
func TestChangePolicy_None(t *testing.T) {
	for kind, acc := range stepTargets() {
		for _, fresh := range []bool{false, true} {
			t.Run(kind+"/new account "+strconv.FormatBool(fresh), func(t *testing.T) {
				in := stepReads(acc)
				in.AccountNew = fresh
				got, err := ChangePolicy(in, addModel, "/spec/access")
				if err != nil {
					t.Fatal(err)
				}
				p := &policy.Policy{Meta: meta.Metadata{ID: "id-1", Name: "research", DisplayName: "Research", Owner: acc.Meta.Owner}, Spec: policy.Spec{Models: []string{"acme/m2"}}}
				var want []Row
				if fresh {
					want = append(want, Row{Kind: "account", Pointer: "/spec/access", Meta: &acc.Meta, Value: acc})
				}
				want = append(want, Row{Kind: "policy", Pointer: "/spec/access", Meta: &p.Meta, Value: p}, rebinding("id-1", acc, "/spec/access"))
				if diff := cmp.Diff(PolicyChange{Case: CaseNone, Rows: want, Policy: p}, got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
			})
		}
	}
}

// A binding holding the account whose policy is gone is none only when the caller may change that binding.
func TestPolicyReads_CaseOfAMissingPolicy(t *testing.T) {
	for _, changeable := range []bool{true, false} {
		in := stepReads(stepTargets()["project"])
		in.Held, in.HeldChangeable = heldBy("gone", "proj-acct"), changeable
		want := CaseNone
		if !changeable {
			want = CaseAbove
		}
		if got := in.Case(); got != want {
			t.Errorf("changeable %v: case %s, want %s", changeable, got, want)
		}
	}
}

// own: the policy is updated in place at the version read, and nothing is written when the change leaves it as it is.
func TestChangePolicy_Own(t *testing.T) {
	for kind, acc := range stepTargets() {
		t.Run(kind, func(t *testing.T) {
			in := stepReads(acc)
			in.Policy = &policy.Policy{Meta: meta.Metadata{ID: "pol", Name: "own", Owner: acc.Meta.Owner, ResourceVersion: "3"}, Spec: policy.Spec{Models: []string{"acme/m1"}, PoolIDs: []string{"pool"}}}
			in.Held, in.Alone, in.MayUpdate = heldBy("pol", acc.Meta.ID), true, true
			got, err := ChangePolicy(in, addModel, "/spec/access")
			if err != nil {
				t.Fatal(err)
			}
			next := *in.Policy
			next.Spec = policy.Spec{Models: []string{"acme/m1", "acme/m2"}, PoolIDs: []string{"pool"}}
			want := PolicyChange{Case: CaseOwn, Rows: []Row{{Kind: "policy", Pointer: "/spec/access", Meta: &next.Meta, Value: &next, Op: OpUpdate}}, Policy: &next}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
			if !slices.Equal(in.Policy.Spec.Models, []string{"acme/m1"}) {
				t.Errorf("the policy read was changed: %v", in.Policy.Spec.Models)
			}
			same, err := ChangePolicy(in, func(*policy.Spec) {}, "/spec/access")
			if err != nil || same.Case != CaseOwn || len(same.Rows) != 0 {
				t.Errorf("a change that changes nothing: %+v %v", same, err)
			}
		})
	}
}

// copy: whatever makes it a copy (shared, owned elsewhere, the caller may not update it, a disabled binding), the copy carries models, links, description and labels, takes the change, and the account moves to it alone through the rebinding PUT /accounts/{ref}/policy makes.
func TestChangePolicy_Copy(t *testing.T) {
	no := false
	src := func(owner meta.Owner) *policy.Policy {
		return &policy.Policy{
			Meta: meta.Metadata{ID: "gold", Name: "gold", DisplayName: "Gold", Description: "the gold tier", Labels: map[string]string{"tier": "gold"}, Owner: owner},
			Spec: policy.Spec{Models: []string{"acme/m1"}, PoolIDs: []string{"pool"}, RateLimits: []policy.RateLimitLink{{RateLimitID: "rl", Models: []string{"acme"}, Shared: &no}}, Budgets: []policy.BudgetLink{{BudgetID: "bu"}}},
		}
	}
	for kind, acc := range stepTargets() {
		for _, tc := range []struct {
			name  string
			reads func(in *PolicyReads)
		}{
			{"bound to another account too", func(in *PolicyReads) {
				in.Policy, in.Held, in.MayUpdate = src(acc.Meta.Owner), heldBy("gold", acc.Meta.ID, "other"), true
			}},
			{"bound alone, another binding uses it", func(in *PolicyReads) {
				in.Policy, in.Held, in.MayUpdate = src(acc.Meta.Owner), heldBy("gold", acc.Meta.ID), true
			}},
			{"owned elsewhere", func(in *PolicyReads) {
				in.Policy, in.Held, in.Alone, in.MayUpdate = src(meta.Global), heldBy("gold", acc.Meta.ID), true, true
			}},
			{"may not update it", func(in *PolicyReads) {
				in.Policy, in.Held, in.Alone = src(acc.Meta.Owner), heldBy("gold", acc.Meta.ID), true
			}},
			{"binding disabled", func(in *PolicyReads) {
				in.Policy, in.Held, in.Alone, in.MayUpdate = src(acc.Meta.Owner), heldBy("gold", acc.Meta.ID), true, true
				in.Held.Spec.Enabled = &no
			}},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				in := stepReads(acc)
				in.HeldChangeable = true
				tc.reads(&in)
				got, err := ChangePolicy(in, addModel, "/spec/budget")
				if err != nil {
					t.Fatal(err)
				}
				cp := src(acc.Meta.Owner)
				cp.Meta = meta.Metadata{ID: "id-1", Name: "gold-for-research", DisplayName: "Gold for Research", Description: "the gold tier", Labels: map[string]string{"tier": "gold"}, Owner: acc.Meta.Owner}
				cp.Spec.Models = []string{"acme/m1", "acme/m2"}
				want := PolicyChange{Case: CaseCopy, Policy: cp, Rows: []Row{
					{Kind: "policy", Pointer: "/spec/budget", Meta: &cp.Meta, Value: &PolicyCopy{Policy: cp, Source: in.Policy}},
					rebinding("id-1", acc, "/spec/budget"),
				}}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(src(in.Policy.Meta.Owner).Spec, in.Policy.Spec); diff != "" {
					t.Errorf("the source changed (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// A copy refuses to carry links its scope may not link, counted by kind, unless the change removes them.
func TestChangePolicy_CopyUncarried(t *testing.T) {
	acc := stepTargets()["team"]
	in := stepReads(acc)
	in.Policy = &policy.Policy{Meta: meta.Metadata{ID: "org", Name: "org", Owner: meta.Global}, Spec: policy.Spec{PoolIDs: []string{"org-pool", "org-pool-2"}, Budgets: []policy.BudgetLink{{BudgetID: "org-budget"}}}}
	in.Held, in.HeldChangeable = heldBy("org", acc.Meta.ID), true
	in.Uncarried = func(p *policy.Policy) map[string]int {
		out := map[string]int{}
		out["pool"] = len(p.Spec.PoolIDs)
		if len(p.Spec.Budgets) > 0 {
			out["budget"] = len(p.Spec.Budgets)
		}
		if out["pool"] == 0 {
			delete(out, "pool")
		}
		return out
	}
	_, err := ChangePolicy(in, addModel, "/spec/budget")
	var ue *UncarriedError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v, want links not carried", err)
	}
	if diff := cmp.Diff(&UncarriedError{Counts: map[string]int{"pool": 2, "budget": 1}}, ue); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if want := "links 1 budget and 2 pools that scope may not link"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q lacks %q", err, want)
	}
	if strings.Contains(err.Error(), "org-") {
		t.Errorf("message names a link: %q", err)
	}
	dropAll := func(s *policy.Spec) { s.PoolIDs, s.Budgets = []string{"made-by-the-request"}, nil }
	if c, err := ChangePolicy(in, dropAll, "/spec/access"); err != nil || c.Case != CaseCopy {
		t.Errorf("a change removing them and linking a pool of its own: %+v %v", c, err)
	}
}

// above: a binding or policy the caller may not see, or a binding the caller may not change when the account would move, refuses naming only the scope; it never falls back to none.
func TestChangePolicy_Above(t *testing.T) {
	for kind, acc := range stepTargets() {
		for _, tc := range []struct {
			name  string
			reads func(in *PolicyReads)
			want  Scope
			text  string
		}{
			{"a binding or policy hidden, the scope hidden too", func(in *PolicyReads) { in.Above = &Scope{Kind: "team"} }, Scope{Kind: "team"}, "a team you may not see"},
			{"a policy of the organization", func(in *PolicyReads) { in.Above = &Scope{Kind: "organization"} }, Scope{Kind: "organization"}, "the organization"},
			{"a binding the caller may not change", func(in *PolicyReads) {
				in.Policy, in.Held, in.HeldScope = &policy.Policy{Meta: meta.Metadata{ID: "secret-policy", Name: "secret-policy", Owner: meta.Global}}, heldBy("secret-policy", acc.Meta.ID), Scope{Kind: "team", Name: "platform"}
			}, Scope{Kind: "team", Name: "platform"}, `team "platform"`},
			{"a binding the caller may not change, its policy gone", func(in *PolicyReads) {
				in.Held, in.HeldScope = heldBy("secret-policy", acc.Meta.ID), Scope{Kind: "team", Name: "platform"}
			}, Scope{Kind: "team", Name: "platform"}, `team "platform"`},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				in := stepReads(acc)
				tc.reads(&in)
				got, err := ChangePolicy(in, addModel, "/spec/target")
				var ae *AboveError
				if !errors.As(err, &ae) || got.Case != CaseAbove || len(got.Rows) != 0 {
					t.Fatalf("change %+v, error %v; want above and no rows", got, err)
				}
				if diff := cmp.Diff(tc.want, ae.Scope); diff != "" {
					t.Errorf("scope (-want +got):\n%s", diff)
				}
				if !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "held") {
					t.Errorf("message %q: want %q and nothing of the hidden rows", err, tc.text)
				}
			})
		}
	}
}
