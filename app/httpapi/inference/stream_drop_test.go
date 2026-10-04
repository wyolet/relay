package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/metrics"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// failOn returns a stream transform that passes frames through unchanged and
// fails, like a translator decoding a malformed payload, on any frame carrying marker.
func failOn(marker string) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) {
		if bytes.Contains(b, []byte(marker)) {
			var v struct{}
			return nil, json.Unmarshal([]byte(`{`), &v)
		}
		return b, nil
	}
}

func identityStream(b []byte) ([]byte, error) { return b, nil }

func runStreamCanonical(t *testing.T, stream string, shapes streamShapes, toCanon, fromCanon func([]byte) ([]byte, error)) string {
	t.Helper()
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	w := httptest.NewRecorder()
	streamCanonical(d, w, r, io.NopCloser(strings.NewReader(stream)), false, false, shapes, toCanon, fromCanon)
	return w.Body.String()
}

func droppedCount(adapter, direction, reason string) float64 {
	return testutil.ToFloat64(metrics.StreamEventsDropped.WithLabelValues(adapter, direction, reason))
}

const canonStreamWithBadDelta = "" +
	"event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n" +
	"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"Hel\"}\n\n" +
	"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"BAD\"}\n\n" +
	"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"lo\"}\n\n" +
	"event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n"

func TestStreamCanonical_UntranslatableUpstreamFrameSkippedAndCounted(t *testing.T) {
	shapes := streamShapes{upstream: "up-shape-a", inbound: "in-shape-a"}
	before := droppedCount("up-shape-a", "to_canonical", "syntax")

	out := runStreamCanonical(t, canonStreamWithBadDelta, shapes, failOn("BAD"), identityStream)

	if got := droppedCount("up-shape-a", "to_canonical", "syntax") - before; got != 1 {
		t.Fatalf("dropped counter delta = %v, want 1", got)
	}
	for _, want := range []string{`"delta":"Hel"`, `"delta":"lo"`, "event: generation.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %s after the dropped frame:\n%s", want, out)
		}
	}
	if strings.Contains(out, "BAD") || strings.Contains(out, "event: error") {
		t.Errorf("dropped frame leaked or a terminal error was added to a stream that completed:\n%s", out)
	}
}

func TestStreamCanonical_UntranslatableInboundFrameSkippedAndCounted(t *testing.T) {
	shapes := streamShapes{upstream: "up-shape-b", inbound: "in-shape-b"}
	before := droppedCount("in-shape-b", "from_canonical", "syntax")

	out := runStreamCanonical(t, canonStreamWithBadDelta, shapes, identityStream, failOn("BAD"))

	if got := droppedCount("in-shape-b", "from_canonical", "syntax") - before; got != 1 {
		t.Fatalf("dropped counter delta = %v, want 1", got)
	}
	for _, want := range []string{`"delta":"Hel"`, `"delta":"lo"`, "event: generation.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %s after the dropped frame:\n%s", want, out)
		}
	}
}

func TestStreamCanonical_LostTerminalFrameEndsWithError(t *testing.T) {
	stream := "" +
		"event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n" +
		"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"Hel\"}\n\n" +
		"event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"BAD\"}\n\n"
	shapes := streamShapes{upstream: "up-shape-c", inbound: "in-shape-c"}

	out := runStreamCanonical(t, stream, shapes, failOn("BAD"), identityStream)

	frames := splitCanonFrames([]byte(out + "\n\n"))
	if len(frames) == 0 {
		t.Fatal("empty stream")
	}
	event, data, _ := v1.ParseSSEChunk(frames[len(frames)-1])
	if event != v1.EventError {
		t.Fatalf("last event = %q, want %q so the caller cannot read the stream as complete:\n%s", event, v1.EventError, out)
	}
	var ev v1.ErrorEvent
	if err := json.Unmarshal(data, &ev); err != nil || ev.Code == "" {
		t.Fatalf("terminal error event = %s (%v), want a coded error", data, err)
	}
}
