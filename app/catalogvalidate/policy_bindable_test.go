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

func policyBindingDoc(name, project, pol string, subjects ...manifest.PolicySubjectDTO) manifest.Document {
	if len(subjects) == 0 {
		subjects = []manifest.PolicySubjectDTO{{Kind: "group", Name: "system:authenticated"}}
	}
	return manifest.Document{PolicyBinding: &manifest.PolicyBindingDTO{
		APIVersion: "v1alpha3",
		Kind:       "PolicyBinding",
		Metadata:   manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "project", Name: project}},
		Spec:       manifest.PolicyBindingSpec{Project: project, Policy: pol, Subjects: subjects},
	}}
}

func scopedBindingDoc(name string, owner manifest.WireOwner, pol string, subjects ...manifest.PolicySubjectDTO) manifest.Document {
	d := policyBindingDoc(name, "", pol, subjects...)
	d.PolicyBinding.Metadata.Owner = owner
	return d
}

// The graph linter mirrors the control-plane rule: a binding uses a policy of
// its own scope, an ancestor of it, the system, or a host (a tier preset);
// a personal policy is never bindable, and only a system binding binds a host.
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
		{"another person's personal policy", policyBindingDoc("pb-personal", "ml-search", "personal"),
			`policy "personal" is personal and cannot be bound`},
		{"team binding on its own policy", scopedBindingDoc("pb-team-own", manifest.WireOwner{Kind: "team", Name: "platform"}, "team-pol"), ""},
		{"system binding on a host", scopedBindingDoc("pb-host", manifest.WireOwner{Kind: "system"}, "tier",
			manifest.PolicySubjectDTO{Kind: "host", Name: "openai-host"}), ""},
		{"project binding on a host", policyBindingDoc("pb-host-project", "ml-search", "tier",
			manifest.PolicySubjectDTO{Kind: "host", Name: "openai-host"}), "bound only by a system-owned binding"},
		{"missing subject", scopedBindingDoc("pb-missing", manifest.WireOwner{Kind: "system"}, "shared",
			manifest.PolicySubjectDTO{Kind: "account", Name: "ghost"}), `account "ghost" not found`},
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
