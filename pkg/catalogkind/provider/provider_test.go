package provider

import (
	"errors"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/meta"
)

// problems reports why Validate refused: "Namespace tag" per failed field rule, or the message of a kind-specific rule.
func problems(err error) []string {
	var fields validator.ValidationErrors
	if errors.As(err, &fields) {
		var out []string
		for _, f := range fields {
			out = append(out, f.Namespace()+" "+f.Tag())
		}
		return out
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}

func TestValidate(t *testing.T) {
	const systemOnly = `provider "acme": owner.kind must be system or empty, got `
	for _, tc := range []struct {
		name string
		p    Provider
		want []string
	}{
		{"no owner", Provider{Meta: meta.Metadata{Name: "acme"}}, nil},
		{"system owner", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}, nil},
		{"links", Provider{Meta: meta.Metadata{Name: "acme"}, Spec: Spec{HomepageURL: "https://acme.example", DocsURL: "http://docs.acme.example/x"}}, nil},
		{"missing name", Provider{}, []string{"Provider.Meta.Name required"}},
		{"homepage is not a URL", Provider{Meta: meta.Metadata{Name: "acme"}, Spec: Spec{HomepageURL: "not-a-url"}}, []string{"Provider.Spec.HomepageURL http_url"}},
		{"every bad link is reported", Provider{Meta: meta.Metadata{Name: "acme"}, Spec: Spec{DocsURL: "docs", StatusPageURL: "status"}},
			[]string{"Provider.Spec.DocsURL http_url", "Provider.Spec.StatusPageURL http_url"}},
		// A provider is shared catalog data: nothing but the deployment owns one.
		{"user owner", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}}}, []string{systemOnly + `"user"`}},
		{"user owner without an id", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerUser}}}, []string{systemOnly + `"user"`}},
		{"team owner", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}}}, []string{systemOnly + `"team"`}},
		{"project owner", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}}, []string{systemOnly + `"project"`}},
		{"host owner", Provider{Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}}, []string{systemOnly + `"host"`}},
	} {
		if diff := cmp.Diff(tc.want, problems(tc.p.Validate())); diff != "" {
			t.Errorf("%s: Validate (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		val  *bool
		want bool
	}{{nil, true}, {&yes, true}, {&no, false}} {
		if got := (&Provider{Spec: Spec{Enabled: tc.val}}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with enabled %v = %v, want %v", tc.val, got, tc.want)
		}
	}
}
