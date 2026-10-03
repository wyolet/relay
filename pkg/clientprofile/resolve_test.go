package clientprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/pkg/httpheader"
)

// stub is a test profile matching on a User-Agent prefix.
type stub struct {
	name  string
	shape string
	ua    string
}

func (s stub) Name() string  { return s.name }
func (s stub) Shape() string { return s.shape }
func (s stub) Match(r *http.Request) bool {
	return s.ua != "" && r.Header.Get("User-Agent") == s.ua
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := New()
	for _, p := range []Profile{
		stub{name: "alpha", shape: "anthropic", ua: "alpha-cli/1.0"},
		stub{name: "beta", shape: "openai"},
	} {
		if err := reg.Register(p); err != nil {
			t.Fatalf("register %s: %v", p.Name(), err)
		}
	}
	return reg
}

func TestResolve_Precedence(t *testing.T) {
	reg := testRegistry(t)

	cases := []struct {
		name   string
		path   string
		header string
		ua     string
		want   string
	}{
		{name: "header wins over prefix and ua", path: "/beta/v1/messages", header: "alpha", ua: "alpha-cli/1.0", want: "alpha"},
		{name: "prefix wins over ua", path: "/beta/v1/chat/completions", ua: "alpha-cli/1.0", want: "beta"},
		{name: "ua when neither header nor prefix", path: "/v1/messages", ua: "alpha-cli/1.0", want: "alpha"},
		{name: "unknown header falls through to prefix", path: "/beta/v1/chat/completions", header: "gamma", want: "beta"},
		{name: "unknown header falls through to ua", path: "/v1/messages", header: "gamma", ua: "alpha-cli/1.0", want: "alpha"},
		{name: "nothing matches", path: "/v1/messages", ua: "curl/8.0", want: ""},
		{name: "unknown prefix", path: "/gamma/v1/messages", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if tc.header != "" {
				r.Header.Set(httpheader.HeaderClient, tc.header)
			}
			if tc.ua != "" {
				r.Header.Set("User-Agent", tc.ua)
			}
			if got := reg.Resolve(r).Name(); got != tc.want {
				t.Fatalf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolve_EmptyRegistryIsDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/alpha/v1/messages", nil)
	if got := New().Resolve(r); got != Default {
		t.Fatalf("Resolve on empty registry = %v, want Default", got)
	}
	var nilReg *Registry
	if got := nilReg.Resolve(r); got != Default {
		t.Fatalf("Resolve on nil registry = %v, want Default", got)
	}
}

func TestFromContext_DefaultWhenUnset(t *testing.T) {
	if got := FromContext(context.Background()); got != Default {
		t.Fatalf("FromContext = %v, want Default", got)
	}
	ctx := WithProfile(context.Background(), stub{name: "alpha"})
	if got := FromContext(ctx).Name(); got != "alpha" {
		t.Fatalf("FromContext = %q, want alpha", got)
	}
}

func TestMiddleware_StoresResolvedProfile(t *testing.T) {
	reg := testRegistry(t)

	var got string
	h := Middleware(reg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context()).Name()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/alpha/v1/messages", nil))
	if got != "alpha" {
		t.Fatalf("profile on ctx = %q, want alpha", got)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if got != "" {
		t.Fatalf("profile on ctx = %q, want Default", got)
	}
}

func TestMiddleware_NilRegistryPassesThrough(t *testing.T) {
	called := false
	h := Middleware(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if FromContext(r.Context()) != Default {
			t.Error("nil registry must leave the context untouched")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/alpha/v1/messages", nil))
	if !called {
		t.Fatal("next handler not called")
	}
}

func TestFirstPathSegment(t *testing.T) {
	cases := map[string]string{
		"/alpha/v1/messages": "alpha",
		"/alpha":             "alpha",
		"alpha/v1":           "alpha",
		"/":                  "",
		"":                   "",
	}
	for in, want := range cases {
		if got := FirstPathSegment(in); got != want {
			t.Errorf("FirstPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
