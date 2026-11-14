package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Upstream content frames that reach dispatch in one read. The to-canonical parsers keep only a chunk's last data: line, so dispatch must hand them one frame at a time for both deltas to survive.
const twoDeltaFrames = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo."}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// canonicalText returns a canonical stream's text deltas in order and its last event's name.
func canonicalText(t *testing.T, stream string) (deltas []string, last string) {
	t.Helper()
	for _, frame := range strings.Split(strings.TrimSuffix(stream, "\n\n"), "\n\n") {
		event, data, _ := v1.ParseSSEChunk([]byte(frame))
		if last = event; event != v1.EventItemDelta {
			continue
		}
		var d struct{ Kind, Delta string }
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("delta frame %q: %v", frame, err)
		}
		if d.Kind == "text" {
			deltas = append(deltas, d.Delta)
		}
	}
	return deltas, last
}

func wantBothDeltas(t *testing.T, stream string) {
	t.Helper()
	deltas, last := canonicalText(t, stream)
	if diff := cmp.Diff([]string{"Hel", "lo."}, deltas); diff != "" || last != v1.EventGenerationCompleted {
		t.Errorf("text deltas (-want +got):\n%s\nlast event %q, want generation.completed\n%s", diff, last, stream)
	}
}

// Line endings are the transport's: the same frames in LF, CRLF or CR yield the same canonical stream.
func TestStreamCanonical_FramesInOneReadYieldEachDelta(t *testing.T) {
	lf := runStreamCanonical(t, twoDeltaFrames, streamShapes{}, productionSpec(adapters.OpenAI).Translator.NewToCanonicalStream(), nil)
	wantBothDeltas(t, lf)
	for name, eol := range map[string]string{"CRLF": "\r\n", "CR": "\r"} {
		got := runStreamCanonical(t, strings.ReplaceAll(twoDeltaFrames, "\n", eol), streamShapes{}, productionSpec(adapters.OpenAI).Translator.NewToCanonicalStream(), nil)
		if got != lf {
			t.Errorf("%s stream\n got %q\nwant %q", name, got, lf)
		}
	}
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
				(&adapter.Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}, Translator: productionSpec(adapters.OpenAI).Translator}).Build(),
				(&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build(),
			))

			w := dispatch(d, pr, DispatchInput{
				Inbound:   adapters.Canonical,
				Body:      []byte(`{"model":"test-model","input":"hi","output_mode":"stream"}`),
				ModelName: "test-model",
				Stream:    true,
			})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			wantBothDeltas(t, w.Body.String())
		})
	}
}
