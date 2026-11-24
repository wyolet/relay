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
		{name: "adjacent to literal text", path: "/m/x-{location}:{project}.gen", want: "/m/x-global:p1.gen"},
		{name: "value with a slash stays one segment", path: "/a/{slash}/b", want: "/a/a%2Fb/b"},
		{name: "value with a space", path: "/a/{space}", want: "/a/a%20b"},
		{name: "query after a placeholder", path: "/m/{project}:stream?alt=sse", want: "/m/p1:stream?alt=sse"},
		{name: "a lone closing brace", path: "/a}/{project}", errKey: "-"},
		{name: "placeholder in the query", path: "/v1/chat?alt={project}", errKey: "-"},
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

// placeholderSyntax is a copy of pkg/catalogkind/host's table of which paths hold only well-formed placeholders, so FillPath and host.Validate agree.
var placeholderSyntax = []struct {
	path       string
	wellFormed bool
}{
	{"", true},
	{"/v1/chat/completions", true},
	{"/v1/projects/{project}/locations/{location}/chat", true},
	{"/m/x-{location}:generate", true},
	{"/{a_1}/{project}", true},
	{"/{project}?alt=sse", true},
	{"/v1/chat?alt=sse", true},
	{"/v1/{project", false},
	{"/v1/{}/chat", false},
	{"/v1/{pro-ject}", false},
	{"/v1/{{project}}", false},
	{"/v1/{", false},
	{"/v1/}", false},
	{"/a}/{project}", false},
	{"/v1/chat?alt={project}", false},
	{"/v1/{project}?x={location}", false},
}

// With every key set, FillPath fails exactly on the paths host.Validate refuses, and names no key for them.
func TestFillPath_SyntaxAgreesWithHostValidate(t *testing.T) {
	backend := map[string]string{"project": "p", "location": "l", "a_1": "a", "pro-ject": "x"}
	for _, tc := range placeholderSyntax {
		_, err := FillPath(tc.path, backend)
		perr, ok := errors.AsType[*pipeline.PathPlaceholderError](err)
		if tc.wellFormed != (err == nil) || (!tc.wellFormed && (!ok || perr.Key != "")) {
			t.Errorf("FillPath(%q) = %v, want well-formed %v", tc.path, err, tc.wellFormed)
		}
	}
}

func TestFillPath_NoPlaceholderAllocatesNothing(t *testing.T) {
	backend := map[string]string{"project": "p1"}
	if n := testing.AllocsPerRun(100, func() { _, _ = FillPath("/v1/chat/completions", backend) }); n != 0 {
		t.Errorf("FillPath without a placeholder: %v allocs, want 0", n)
	}
}
