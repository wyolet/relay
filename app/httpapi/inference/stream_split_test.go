package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Two upstream content frames that reach dispatch in one read. The SDK
// to-canonical parsers keep only a chunk's last data: line, so dispatch must
// hand them one frame at a time for both deltas to survive.
const twoDeltaFrames = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo."}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

func assertBothDeltas(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{`"delta":"Hel"`, `"delta":"lo."`, "event: generation.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("canonical stream missing %s:\n%s", want, out)
		}
	}
}

func TestStreamCanonical_TwoFramesInOneReadYieldBoth(t *testing.T) {
	out := runStreamCanonical(t, twoDeltaFrames, streamShapes{}, openai.CCTranslator{}.NewToCanonicalStream(), nil)
	assertBothDeltas(t, out)
}

func TestStreamCanonical_CRLFFramesYieldBoth(t *testing.T) {
	crlf := strings.ReplaceAll(twoDeltaFrames, "\n", "\r\n")
	out := runStreamCanonical(t, crlf, streamShapes{}, openai.CCTranslator{}.NewToCanonicalStream(), nil)
	assertBothDeltas(t, out)
}

// A canonical caller streaming from an upstream that ends its SSE lines in CRLF or CR gets the whole generation, not an empty one reported as completed.
func TestDispatch_CRLFUpstreamStreamYieldsFullGeneration(t *testing.T) {
	for name, eol := range map[string]string{"CRLF": "\r\n", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(strings.ReplaceAll(twoDeltaFrames, "\n", eol)))
			}))
			t.Cleanup(up.Close)
			cat, pr := buildDispatchCatalog(t, "upstream", adapters.OpenAI)
			pointHostAt(t, cat, up.URL)
			d := buildRunnableDeps(t, cat)
			useSpecs(&d, adapter.NewRegistry(
				(&adapter.Spec{
					Name:        adapters.OpenAI,
					DefaultPath: "/v1/chat/completions",
					Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
					Translator:  openai.CCTranslator{},
				}).Build(),
				(&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build(),
			))

			r := withNormalContext(httptest.NewRequest(http.MethodPost, "/v1/generate", nil), pr)
			w := httptest.NewRecorder()
			Dispatch(d, w, r, DispatchInput{
				Inbound:   adapters.Canonical,
				Body:      []byte(`{"model":"test-model","input":"hi","output_mode":"stream"}`),
				ModelName: "test-model",
				Stream:    true,
			})

			out := w.Body.String()
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, out)
			}
			assertBothDeltas(t, out)
			mustNotContain(t, "canonical stream", out, "event: error")
		})
	}
}
