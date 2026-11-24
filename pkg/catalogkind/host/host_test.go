package host

import (
	"errors"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
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
		{"param renames", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"max_tokens": "max_completion_tokens"}, "anthropic": {}}
		}, nil},
		{"rename without an adapter", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"": {"max_tokens": "max_completion_tokens"}}
		}, []string{`host "h": requestParamRenames: adapter name must not be empty`}},
		{"rename from an empty key", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"": "max_completion_tokens"}}
		}, []string{`host "h": requestParamRenames[openai]: key names must not be empty`}},
		{"rename to an empty key", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"max_tokens": ""}}
		}, []string{`host "h": requestParamRenames[openai]: key names must not be empty`}},
		{"rename to itself", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"max_tokens": "max_tokens"}}
		}, []string{`host "h": requestParamRenames[openai][max_tokens]: renamed to itself`}},
		{"target is also a source", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"a": "b", "b": "c"}}
		}, []string{`host "h": requestParamRenames[openai][a]: target "b" is also renamed`}},
		{"two sources share a target", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"a": "c", "b": "c"}}
		}, []string{`host "h": requestParamRenames[openai][b]: target "c" is also the target of "a"`}},
		{"the same key renamed under two adapters", func(h *Host) {
			h.Spec.RequestParamRenames = map[adapters.Name]map[string]string{"openai": {"a": "b"}, "gemini": {"b": "a"}}
		}, nil},
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

func TestParamRenames(t *testing.T) {
	s := Spec{RequestParamRenames: map[adapters.Name]map[string]string{
		"openai":    {"max_tokens": "max_completion_tokens", "a": "b"},
		"anthropic": {},
	}}
	for _, tc := range []struct {
		shape adapters.Name
		want  []ParamRename
	}{
		{"openai", []ParamRename{{"a", "b"}, {"max_tokens", "max_completion_tokens"}}},
		{"anthropic", nil},
		{"gemini", nil},
	} {
		if diff := cmp.Diff(tc.want, s.ParamRenames(tc.shape)); diff != "" {
			t.Errorf("ParamRenames(%s) (-want +got):\n%s", tc.shape, diff)
		}
	}
	if got := (Spec{}).ParamRenames("openai"); got != nil {
		t.Errorf("no table: ParamRenames = %v, want nil", got)
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

// A path override is routable only when its backend fills every placeholder; Validate accepts it either way, as the operator may fill backend after install.
func TestRoutable_PathPlaceholders(t *testing.T) {
	filled := map[string]string{"project": "p1", "location": "global"}
	for _, tc := range []struct {
		name     string
		path     string
		paths    map[adapters.Name]string
		backend  map[string]string
		routable bool
	}{
		{name: "no placeholder", path: "/v1/chat", routable: true},
		{name: "path filled", path: "/v1/projects/{project}/locations/{location}/chat", backend: filled, routable: true},
		{name: "paths filled", paths: map[adapters.Name]string{adapters.OpenAI: "/v1/{project}/chat"}, backend: filled, routable: true},
		{name: "path missing a key", path: "/v1/{project}/{zone}", backend: filled},
		{name: "path with an empty value", path: "/v1/{project}", backend: map[string]string{"project": ""}},
		{name: "no backend", path: "/v1/{project}"},
		{name: "one paths entry unfilled", paths: map[adapters.Name]string{adapters.OpenAI: "/v1/{project}", adapters.Anthropic: "/v1/{zone}"}, backend: filled},
		{name: "malformed: unclosed", path: "/v1/{project", backend: filled},
		{name: "malformed: empty key", path: "/v1/{}/chat", backend: filled},
		{name: "malformed: key with a dash", path: "/v1/{pro-ject}", backend: map[string]string{"pro-ject": "x"}},
		{name: "malformed: nested", path: "/v1/{{project}}", backend: filled},
	} {
		h := &Host{Meta: meta.Metadata{Name: "h"}, Spec: Spec{BaseURL: "https://x", Paths: tc.paths, Backend: tc.backend}}
		if tc.path != "" {
			h.Spec.Path = &tc.path
		}
		if got := h.Routable(); got != tc.routable {
			t.Errorf("%s: Routable() = %v, want %v", tc.name, got, tc.routable)
		}
		if err := h.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		}
	}
}

func TestPathFor(t *testing.T) {
	own, compat := "/own", "/v1beta/openai/chat/completions"
	both := Spec{Path: &own, Paths: map[adapters.Name]string{adapters.OpenAI: compat, adapters.Anthropic: ""}}
	for _, tc := range []struct {
		name  string
		spec  Spec
		shape adapters.Name
		want  *string
	}{
		{"listed shape", both, adapters.OpenAI, &compat},
		{"listed as the whole endpoint", both, adapters.Anthropic, new(string)},
		{"unlisted shape uses path", both, adapters.Gemini, &own},
		{"no override", Spec{}, adapters.Gemini, nil},
	} {
		if diff := cmp.Diff(tc.want, tc.spec.PathFor(tc.shape)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestValidate_PathsNeedAnAdapterName(t *testing.T) {
	h := &Host{Meta: meta.Metadata{Name: "h"}, Spec: Spec{Paths: map[adapters.Name]string{"": "/x"}}}
	if err := h.Validate(); err == nil {
		t.Fatal("a paths entry without an adapter name was accepted")
	}
}
