package adapterspecs

import (
	"maps"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
)

// cmd/relay refuses to boot on a wiring fault, such as an upstream-binding name without a spec.
func TestSpecsAreWired(t *testing.T) {
	if err := adapter.NewRegistry(Specs()...).AssertWired(); err != nil {
		t.Fatal(err)
	}
}

// Claude on Vertex sends the anthropic body through the same translator, so byte-pass and canonical failover both reach it from the anthropic shape.
func TestVertexAnthropicSpec(t *testing.T) {
	reg := adapter.NewRegistry(Specs()...)
	s := reg.Spec(adapters.VertexAnthropic)
	if s == nil {
		t.Fatal("no vertex_anthropic spec")
	}
	if s.FamilyName() != adapters.Anthropic {
		t.Errorf("family = %q, want anthropic", s.FamilyName())
	}
	if g := reg.TranslatorGroup(adapters.VertexAnthropic); g == 0 || g != reg.TranslatorGroup(adapters.Anthropic) {
		t.Errorf("translator group = %d, want the anthropic spec's %d", g, reg.TranslatorGroup(adapters.Anthropic))
	}
	if len(s.InboundPaths) != 0 || s.CountPath != "" || s.Org != nil || s.Overloaded != nil {
		t.Errorf("inbound %v, count path %q, org %v, overloaded %v; want none", s.InboundPaths, s.CountPath, s.Org, s.Overloaded)
	}
	if s.Auth.Header != "Authorization" || s.Auth.Scheme != "Bearer" || len(s.Auth.ExtraHeaders) != 0 || s.OAuthAuth.Header != "" {
		t.Errorf("auth = %+v, oauth %+v; want a bearer token alone", s.Auth, s.OAuthAuth)
	}
	for stream, want := range map[bool]string{
		false: "/v1/projects/{project}/locations/{location}/publishers/anthropic/models/m:rawPredict",
		true:  "/v1/projects/{project}/locations/{location}/publishers/anthropic/models/m:streamRawPredict",
	} {
		if got := s.UpstreamPathFn("m", stream); got != want {
			t.Errorf("stream=%v: path %q, want %q", stream, got, want)
		}
	}
}

// vertex_gemini sends the Gemini body from its own Vertex path with a bearer token, through a translator of its own: a canonical body serialized for the gemini spec is never sent to it.
func TestVertexGeminiSpec(t *testing.T) {
	reg := adapter.NewRegistry(Specs()...)
	s, gemini := reg.Spec(adapters.VertexGemini), reg.Spec(adapters.Gemini)
	if s.Family != adapters.Gemini || len(s.InboundPaths) != 0 || s.Envelope != nil || s.CountPath != "" || s.Org != nil {
		t.Errorf("family %q, inbound paths %v, envelope %v, count path %q, org %v; want family gemini and none of the rest", s.Family, s.InboundPaths, s.Envelope, s.CountPath, s.Org)
	}
	if s.Translator != (pkggemini.GeminiTranslator{Vertex: true}) || reg.TranslatorGroup(adapters.VertexGemini) == reg.TranslatorGroup(adapters.Gemini) {
		t.Errorf("translator %#v in group %d beside gemini's %d; want the Vertex variant in a group of its own", s.Translator, reg.TranslatorGroup(adapters.VertexGemini), reg.TranslatorGroup(adapters.Gemini))
	}
	if !maps.Equal(s.ParamPaths, gemini.ParamPaths) {
		t.Errorf("param paths %v, want gemini's %v", s.ParamPaths, gemini.ParamPaths)
	}
	const models = "/v1/projects/{project}/locations/{location}/publishers/google/models/m"
	for stream, want := range map[bool]string{false: models + ":generateContent", true: models + ":streamGenerateContent?alt=sse"} {
		if got := s.UpstreamPathFn("m", stream); got != want {
			t.Errorf("stream=%v: path %q, want %q", stream, got, want)
		}
	}
	if a := s.Auth; a.Header != "Authorization" || a.Scheme != "Bearer" || len(a.ExtraHeaders) != 0 || s.OAuthAuth.Header != "" {
		t.Errorf("auth %+v, OAuth auth %+v; want a bearer Authorization header alone", a, s.OAuthAuth)
	}
}

// Anthropic's clients retry a 529 overloaded_error as an overload; a 429 reads to them as their own rate limit.
func TestAnthropicSpecAnswersOverloadAs529(t *testing.T) {
	for _, s := range Specs() {
		if s.Name != adapters.Anthropic {
			continue
		}
		if s.Overloaded == nil || s.Overloaded.Status != 529 {
			t.Fatalf("Overloaded = %+v, want status 529", s.Overloaded)
		}
		return
	}
	t.Fatal("no anthropic spec")
}
