package host

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
	const sharedData = `host "h": hosts are shared catalog data: owner.kind must be system or omitted, got `
	for _, tc := range []struct {
		name string
		edit func(*Host)
		want []string
	}{
		{"system owner", func(*Host) {}, nil},
		{"no owner", func(h *Host) { h.Meta.Owner = meta.Owner{} }, nil},
		{"no baseURL yet", func(h *Host) { h.Spec.BaseURL = "" }, nil},
		{"both strategies", func(h *Host) { h.Spec.PricingStrategies = []string{"api", "sub"} }, nil},
		{"missing name", func(h *Host) { h.Meta.Name = "" }, []string{"Host.Meta.Name required"}},
		{"name is not a slug", func(h *Host) { h.Meta.Name = "Not A Slug" }, []string{"Host.Meta.Name slug"}},
		{"baseURL is not a URL", func(h *Host) { h.Spec.BaseURL = "not-a-url" }, []string{"Host.Spec.BaseURL http_url"}},
		{"baseURL is not http", func(h *Host) { h.Spec.BaseURL = "ftp://api.example.com" }, []string{"Host.Spec.BaseURL http_url"}},
		{"unknown strategy", func(h *Host) { h.Spec.PricingStrategies = []string{"api", "free"} }, []string{"Host.Spec.PricingStrategies[1] oneof"}},
		{"every bad link is reported", func(h *Host) { h.Spec.DocsURL, h.Spec.ConsoleURL = "docs", "console" },
			[]string{"Host.Spec.DocsURL http_url", "Host.Spec.ConsoleURL http_url"}},
		{"provider owner", func(h *Host) { h.Meta.Owner.Kind = meta.OwnerProvider }, []string{sharedData + `"provider"`}},
		{"user owner", func(h *Host) { h.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-1"} }, []string{sharedData + `"user"`}},
		{"user owner without an id", func(h *Host) { h.Meta.Owner = meta.Owner{Kind: meta.OwnerUser} }, []string{sharedData + `"user"`}},
		{"team owner", func(h *Host) { h.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"} }, []string{sharedData + `"team"`}},
		{"project owner", func(h *Host) { h.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"} }, []string{sharedData + `"project"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Host{
				Meta: meta.Metadata{Name: "h", Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: Spec{BaseURL: "https://api.example.com"},
			}
			tc.edit(h)
			if diff := cmp.Diff(tc.want, problems(h.Validate())); diff != "" {
				t.Errorf("Validate (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSpecDefaults(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name                string
		spec                Spec
		enabled, routable   bool
		strategies          []string
		allowsAPI, allowSub bool
	}{
		{"zero spec", Spec{}, true, false, []string{"api"}, true, false},
		{"subscription only", Spec{BaseURL: "https://x", PricingStrategies: []string{"sub"}}, true, true, []string{"sub"}, false, true},
		{"disabled", Spec{BaseURL: "https://x", Enabled: &no, PricingStrategies: []string{"api", "sub"}}, false, true, []string{"api", "sub"}, true, true},
	} {
		h := &Host{Spec: tc.spec}
		got := []any{h.IsEnabled(), h.Routable(), tc.spec.Strategies(), tc.spec.AllowsStrategy("api"), tc.spec.AllowsStrategy("sub"), tc.spec.AllowsStrategy("")}
		want := []any{tc.enabled, tc.routable, tc.strategies, tc.allowsAPI, tc.allowSub, false}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s: enabled, routable, strategies, allows api, allows sub, allows \"\" (-want +got):\n%s", tc.name, diff)
		}
	}
}
