package inference

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
)

// pathModelRegistry pairs an OpenAI CC inbound with an upstream shape that puts the model in the URL path, as cmd/relay registers it.
func pathModelRegistry() *adapter.Registry {
	cc := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  productionSpec(adapters.OpenAI).Translator,
	}).Build()
	pathModel := (&adapter.Spec{
		Name: adapters.Gemini,
		Auth: adapter.AuthStrategy{Header: "x-goog-api-key"},
		UpstreamPathFn: func(model string, stream bool) string {
			if stream {
				return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
			}
			return "/v1beta/models/" + model + ":generateContent"
		},
		Translator:    productionSpec(adapters.Gemini).Translator,
		ExtractTokens: productionSpec(adapters.Gemini).ExtractTokens,
	}).Build()
	return adapter.NewRegistry(cc, pathModel)
}

// A wildcard alias forwards the caller's model string verbatim, but only ever as one path segment: it cannot add segments, climb out of the model path or start a query.
func TestDispatch_WildcardAliasModelStaysOnePathSegment(t *testing.T) {
	upURL, last := answeringUpstream(t, "application/json", `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	cat, pr := buildDispatchCatalog(t, "pathmodel", adapters.Gemini)
	snap := cat.Current()
	pointHostAt(t, cat, upURL)
	m := *snap.ModelsByName("test-model")[0]
	m.Spec.Aliases = []string{"gem-v[*]"}
	if err := applyModel(t, cat, &m); err != nil {
		t.Fatal(err)
	}
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, pathModelRegistry())

	const raw = "gem-v-2/../../../v1beta/files?pageSize=3&z="
	w := dispatch(d, pr, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"` + raw + `","messages":[{"role":"user","content":"hi"}]}`),
		ModelName: raw,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if path, _ := last(); path != "/v1beta/models/gem-v-2%2F..%2F..%2F..%2Fv1beta%2Ffiles%3FpageSize=3&z=:generateContent" {
		t.Errorf("upstream path = %q, want the model as one escaped segment", path)
	}
}
