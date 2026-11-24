package adapter

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/pipeline"
)

func TestPathSegment(t *testing.T) {
	for in, want := range map[string]string{
		"gemini-2.5-flash":        "gemini-2.5-flash",
		"m:tag@v1":                "m:tag@v1",
		"a/../b?x=1":              "a%2F..%2Fb%3Fx=1",
		"gemini-2.5-flash[1m]":    "gemini-2.5-flash%5B1m%5D",
		".":                       "%2E",
		"..":                      "%2E%2E",
		"with space#and-fragment": "with%20space%23and-fragment",
	} {
		if got := pathSegment(in); got != want {
			t.Errorf("pathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFillPath(t *testing.T) {
	backend := map[string]string{"project": "p1", "location": "global", "Region_2": "eu", "empty": "", "slash": "a/b", "space": "a b"}
	for _, tc := range []struct {
		name, path, want string
		// errKey is the key a *pipeline.PathPlaceholderError names; "-" means a malformed placeholder.
		errKey string
	}{
		{name: "no placeholder", path: "/v1/chat/completions", want: "/v1/chat/completions"},
		{name: "empty path", path: "", want: ""},
		{name: "one", path: "/v1/projects/{project}/chat", want: "/v1/projects/p1/chat"},
		{name: "several", path: "/v1/projects/{project}/locations/{location}/r/{Region_2}", want: "/v1/projects/p1/locations/global/r/eu"},
		{name: "repeated key", path: "/{project}/{project}", want: "/p1/p1"},
		{name: "adjacent to literal text", path: "/m/x-{location}:generate?alt={project}", want: "/m/x-global:generate?alt=p1"},
		{name: "value with a slash stays one segment", path: "/a/{slash}/b", want: "/a/a%2Fb/b"},
		{name: "value with a space", path: "/a/{space}", want: "/a/a%20b"},
		{name: "a lone closing brace passes", path: "/a}/{project}", want: "/a}/p1"},
		{name: "missing key", path: "/v1/{zone}/chat", errKey: "zone"},
		{name: "empty value", path: "/v1/{empty}/chat", errKey: "empty"},
		{name: "unclosed", path: "/v1/{project", errKey: "-"},
		{name: "empty key", path: "/v1/{}/chat", errKey: "-"},
		{name: "key with a dash", path: "/v1/{pro-ject}/chat", errKey: "-"},
		{name: "nested brace", path: "/v1/{{project}}/chat", errKey: "-"},
		{name: "trailing brace", path: "/v1/{", errKey: "-"},
	} {
		got, err := FillPath(tc.path, backend)
		if tc.errKey == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: FillPath(%q) = %q, %v; want %q", tc.name, tc.path, got, err, tc.want)
			}
			continue
		}
		perr, ok := errors.AsType[*pipeline.PathPlaceholderError](err)
		wantKey := tc.errKey
		if wantKey == "-" {
			wantKey = ""
		}
		if !ok || got != "" || perr.Key != wantKey || perr.Path != tc.path {
			t.Errorf("%s: FillPath(%q) = %q, %v; want a *pipeline.PathPlaceholderError for key %q", tc.name, tc.path, got, err, wantKey)
		}
	}
}

func TestFillPath_NoPlaceholderAllocatesNothing(t *testing.T) {
	backend := map[string]string{"project": "p1"}
	if n := testing.AllocsPerRun(100, func() { _, _ = FillPath("/v1/chat/completions", backend) }); n != 0 {
		t.Errorf("FillPath without a placeholder: %v allocs, want 0", n)
	}
}
