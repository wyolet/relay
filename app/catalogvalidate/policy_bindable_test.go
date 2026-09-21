package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func policyOwnedBy(name string, owner manifest.WireOwner) manifest.Document {
	return manifest.Document{Policy: &manifest.PolicyDTO{
		APIVersion: "v1alpha2",
		Kind:       "Policy",
		Metadata:   manifest.WireMeta{Name: name, Owner: owner},
	}}
}

func policyBindingDoc(name, project, pol string) manifest.Document {
	return manifest.Document{PolicyBinding: &manifest.PolicyBindingDTO{
		APIVersion: "v1alpha2",
		Kind:       "PolicyBinding",
		Metadata:   manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "project", Name: project}},
		Spec: manifest.PolicyBindingSpec{
			Project: project, Policy: pol,
			Subjects: []manifest.SubjectDTO{{Kind: "group", Name: "system:authenticated"}},
		},
	}}
}

// D74: the graph linter mirrors the control-plane rule: a
// binder may name only its own project's policy or a system-owned shared
// one, and a host tier policy is never bindable.
func TestValidateGraph_D74BindablePolicy(t *testing.T) {
	base := func(extra ...manifest.Document) []manifest.Document {
		docs := append(fixture(),
			teamDoc("platform"),
			projectDoc("ml-search", "platform"),
			projectDoc("other", "platform"),
			policyOwnedBy("mine", manifest.WireOwner{Kind: "project", Name: "ml-search"}),
			policyOwnedBy("theirs", manifest.WireOwner{Kind: "project", Name: "other"}),
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
		{"another project's policy", policyBindingDoc("pb-foreign", "ml-search", "theirs"),
			`policy "theirs" belongs to project "other"`},
		{"host tier policy", policyBindingDoc("pb-tier", "ml-search", "tier"),
			`policy "tier" is a host tier policy`},
		{"service account on a foreign policy", serviceAccountDoc("sa", "ml-search", "theirs"),
			`policy "theirs" belongs to project "other"`},
		{"service account on a tier policy", serviceAccountDoc("sa", "ml-search", "tier"),
			`policy "tier" is a host tier policy`},
		{"operator's ownerless shared policy", policyBindingDoc("pb-operator", "ml-search", "operator"), ""},
		{"another person's personal policy", policyBindingDoc("pb-personal", "ml-search", "personal"),
			`policy "personal" is personal and cannot be bound`},
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
