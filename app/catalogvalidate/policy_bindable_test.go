package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func policyOwnedBy(name string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{Policy: &manifest.PolicyDTO{
		APIVersion: "v1alpha3",
		Kind:       "Policy",
		Metadata:   manifest.WireMeta{Name: name, Owner: owner},
	}}
}

func policyBindingDoc(name, project, pol string) manifest.Document {
	return scopedBindingDoc(name, manifest.WireOwner{Kind: "project", Name: project}, pol)
}

func scopedBindingDoc(name string, owner manifest.WireOwner, pol string, accounts ...string) manifest.Document {
	if len(accounts) == 0 {
		accounts = []string{"some-account"}
	}
	return manifest.Document{PolicyBinding: &manifest.PolicyBindingDTO{
		APIVersion: "v1alpha3",
		Kind:       "PolicyBinding",
		Metadata:   manifest.WireMeta{Name: name, Owner: owner},
		Spec:       manifest.PolicyBindingSpec{Policy: pol, Accounts: accounts},
	}}
}

// The graph linter mirrors the control-plane rule: a binding uses a policy of
// its own scope, an ancestor of it, the system, or a host (a tier preset);
// a personal policy is bound by its owner or a team or project.
func TestValidateGraph_BindsOnlyPoliciesOfItsScope(t *testing.T) {
	base := func(extra ...manifest.Document) []manifest.Document {
		docs := append(fixture(),
			teamDoc("platform"),
			teamDoc("other-team"),
			projectDoc("ml-search", "platform"),
			projectDoc("other", "platform"),
			policyOwnedBy("mine", manifest.WireOwner{Kind: "project", Name: "ml-search"}),
			policyOwnedBy("theirs", manifest.WireOwner{Kind: "project", Name: "other"}),
			policyOwnedBy("team-pol", manifest.WireOwner{Kind: "team", Name: "platform"}),
			policyOwnedBy("other-team-pol", manifest.WireOwner{Kind: "team", Name: "other-team"}),
			policyOwnedBy("shared", manifest.WireOwner{Kind: "system"}),
			policyOwnedBy("tier", manifest.WireOwner{Kind: "host", Name: "openai-host"}),
			policyOwnedBy("operator", manifest.WireOwner{Kind: "user"}),
			policyOwnedBy("personal", manifest.WireOwner{Kind: "user", Name: "alice"}),
		)
		return append(docs, extra...)
	}

	for _, tc := range []struct {
		name string
		doc  manifest.Document
		want string
	}{
		{"own project policy", policyBindingDoc("pb-own", "ml-search", "mine"), ""},
		{"shared policy", policyBindingDoc("pb-shared", "ml-search", "shared"), ""},
		{"team policy from one of its projects", policyBindingDoc("pb-team", "ml-search", "team-pol"), ""},
		{"host tier policy", policyBindingDoc("pb-tier", "ml-search", "tier"), ""},
		{"another project's policy", policyBindingDoc("pb-foreign", "ml-search", "theirs"),
			`policy "theirs" belongs to project "other"`},
		{"another team's policy", policyBindingDoc("pb-other-team", "ml-search", "other-team-pol"),
			`policy "other-team-pol" belongs to team "other-team"`},
		{"operator's ownerless shared policy", policyBindingDoc("pb-operator", "ml-search", "operator"), ""},
		{"a project binding a person's policy", policyBindingDoc("pb-personal", "ml-search", "personal"), ""},
		{"a system binding a person's policy", scopedBindingDoc("pb-personal-system", manifest.WireOwner{Kind: "system"}, "personal"),
			`policy "personal" is personal`},
		{"a person binding their own policy", scopedBindingDoc("pb-alice", manifest.WireOwner{Kind: "user", Name: "alice"}, "personal"), ""},
		{"a person binding another's policy", scopedBindingDoc("pb-bob", manifest.WireOwner{Kind: "user", Name: "bob"}, "personal"), `policy "personal" is personal`},
		{"team binding on its own policy", scopedBindingDoc("pb-team-own", manifest.WireOwner{Kind: "team", Name: "platform"}, "team-pol"), ""},
		{"a binding naming no account", func() manifest.Document {
			d := scopedBindingDoc("pb-empty", manifest.WireOwner{Kind: "system"}, "shared")
			d.PolicyBinding.Spec.Accounts = nil
			return d
		}(), "binds at least one account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateGraph(base(tc.doc))
			report := Format(issues)
			if tc.want == "" {
				if HasErrors(issues) {
					t.Fatalf("want clean, got:\n%s", report)
				}
				return
			}
			if !HasErrors(issues) {
				t.Fatalf("want an error mentioning %q, got a clean report", tc.want)
			}
			if !strings.Contains(report, tc.want) {
				t.Fatalf("report does not mention %q:\n%s", tc.want, report)
			}
		})
	}
}
