package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
)

// pathModelRegistry pairs an OpenAI CC inbound with an upstream shape that puts the model in the URL path, as cmd/relay registers it.
func pathModelRegistry() *adapter.Registry {
	cc := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  pkgopenai.CCTranslator{},
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
		Translator:    pkggemini.GeminiTranslator{},
		ExtractTokens: pkggemini.ExtractTokens,
	}).Build()
	return adapter.NewRegistry(cc, pathModel)
}

// A wildcard alias forwards the caller's model string verbatim, but only ever as one path segment: it cannot add segments, climb out of the model path or start a query.
func TestDispatch_WildcardAliasModelStaysOnePathSegment(t *testing.T) {
	var mu sync.Mutex
	var escapedPath, rawQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		escapedPath, rawQuery = r.URL.EscapedPath(), r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`))
	}))
	defer up.Close()

	cat, pr := buildDispatchCatalog(t, "pathmodel", adapters.Gemini)
	snap := cat.Current()
	pointHostAt(t, cat, up.URL)
	m := *snap.ModelsByName("test-model")[0]
	m.Spec.Aliases = []string{"gem-v[*]"}
	if err := cat.ApplyModelUpsert(&m); err != nil {
		t.Fatal(err)
	}
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, pathModelRegistry())

	raw := "gem-v-2/../../../v1beta/files?pageSize=3&z="
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil), pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"` + raw + `","messages":[{"role":"user","content":"hi"}]}`),
		ModelName: raw,
	})

	mu.Lock()
	defer mu.Unlock()
	if escapedPath == "" {
		t.Fatalf("upstream not called: status %d body %s", w.Code, w.Body)
	}
	if rawQuery != "" {
		t.Errorf("caller input started a query: %q", rawQuery)
	}
	rest, ok := strings.CutPrefix(escapedPath, "/v1beta/models/")
	if !ok || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ":generateContent") {
		t.Errorf("model did not stay one path segment: %q", escapedPath)
	}
}
